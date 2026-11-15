package shellterm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// RunSessionCommandInput is a command the user chose to run from an agent's
// reply. It runs in a new shell tab in the session's worktree, as the user,
// so prompts (1Password, Terraform's "yes") work as in their own terminal.
type RunSessionCommandInput struct {
	SessionID domain.SessionID
	Shell     string
	Command   string
	// OnDone receives the result once the command finishes, its terminal
	// closes, or the wait gives up. An error means the result did not arrive, so
	// it is retried. Nil when the caller only wants the shell.
	OnDone func(SessionCommandResult) error
}

// SessionCommandResult is what the terminal showed for one run command.
type SessionCommandResult struct {
	SessionID domain.SessionID
	Terminal  ShellTerminal
	Command   string
	// Finished reports that the shell returned to its prompt; ExitCode is only
	// meaningful then. Otherwise the terminal closed or the wait timed out.
	Finished bool
	ExitCode int
	TimedOut bool
	Duration time.Duration
	// Output is the plain-text output, without the shell's echo of the
	// command. LogPath holds the same text on disk; empty if it was not saved.
	Output  string
	LogPath string
}

const (
	// sessionCommandOutputLines is the pty host's whole rolling buffer.
	sessionCommandOutputLines = 1000
	sessionCommandPoll        = 500 * time.Millisecond
	sessionCommandMaxWait     = 12 * time.Hour
	sessionCommandTitleRunes  = 40
	// sessionCommandSnapshotPolls keeps recent output for a tab closed mid-run,
	// whose host takes its buffer with it.
	sessionCommandSnapshotPolls = 10
	// sessionCommandOutputSettle bounds the wait for the host to read the end of
	// the output: the shell reports completion before the host has drained the PTY.
	sessionCommandOutputSettle = 2 * time.Second
	sessionCommandRetryFirst   = 5 * time.Second
	sessionCommandRetryLongest = 5 * time.Minute
	sessionCommandRetryWindow  = 24 * time.Hour
)

// RunSessionCommand opens a new shell tab in the session's worktree, runs the
// command in it, and returns the tab. When the shell can report completion (zsh,
// bash), OnDone later gets the exit status and output, even if the daemon
// restarted in between.
func (s *Service) RunSessionCommand(ctx context.Context, in RunSessionCommandInput) (ShellTerminal, error) {
	if err := ctx.Err(); err != nil {
		return ShellTerminal{}, err
	}
	if in.SessionID == "" {
		return ShellTerminal{}, apierr.Invalid("SESSION_COMMAND_SESSION_REQUIRED", "A session is required to run a command", nil)
	}
	command := normalizeSessionCommand(in.Command)
	if command == "" {
		return ShellTerminal{}, apierr.Invalid("SESSION_COMMAND_REQUIRED", "A command is required", nil)
	}
	release, err := s.acquireSessionGate(ctx, in.SessionID)
	if err != nil {
		return ShellTerminal{}, err
	}
	defer release()
	workingDir, projectID, err := s.resolveSessionCommandWorkingDir(ctx, in.SessionID)
	if err != nil {
		return ShellTerminal{}, err
	}
	argv, usedFallback := resolveUserLoginShell(in.Shell)
	if usedFallback {
		return ShellTerminal{}, apierr.Invalid("SHELL_TERMINAL_SHELL_UNAVAILABLE",
			fmt.Sprintf("The selected shell is unavailable: %s. Choose another shell in Settings.", in.Shell), nil)
	}
	if len(argv) == 0 {
		return ShellTerminal{}, apierr.Internal("SHELL_TERMINAL_NO_SHELL", "Could not determine a shell to launch. Set SHELL (macOS/Linux) or ComSpec (Windows).")
	}
	projectEnv, err := s.projects.ProjectEnv(ctx, projectID)
	if err != nil {
		return ShellTerminal{}, fmt.Errorf("run session command: resolve project environment: %w", err)
	}
	env, verifier, releasePreview, err := s.sessionCommandEnv(ctx, in.SessionID, projectID, projectEnv)
	if err != nil {
		return ShellTerminal{}, err
	}
	defer releasePreview()
	readiness, err := prepareCommandShellReadiness(s.dataDir, argv, env)
	if err != nil {
		return ShellTerminal{}, err
	}
	defer readiness.cleanup()
	maps.Copy(env, readiness.env)
	terminal, err := s.openTerminal(ctx, openTerminalConfig{argv: readiness.argv, env: env, projectEnv: projectEnv, projectID: projectID,
		sessionID: in.SessionID, workingDir: workingDir, title: sessionCommandTitle(command), previewVerifier: verifier})
	if err != nil {
		return ShellTerminal{}, err
	}
	handle := ports.RuntimeHandle{ID: terminal.HandleID}
	if err := s.waitForCueShellReady(ctx, handle, readiness.file); err != nil {
		return ShellTerminal{}, err
	}
	// Only zsh and bash report completion; other shells get the command typed.
	input := sessionCommandLine(command)
	run := pendingSessionCommand{SessionID: in.SessionID, Terminal: terminal, Command: command, DoneFile: readiness.doneFile, StartedAt: s.now()}
	tracked := readiness.doneFile != ""
	if tracked {
		if input, err = s.prepareSessionCommand(run); err != nil {
			s.removeSessionCommandFiles(run)
			return ShellTerminal{}, fmt.Errorf("run session command: %w", err)
		}
	}
	if err := s.runtime.SendMessage(ctx, handle, input); err != nil {
		if tracked {
			s.removeSessionCommandFiles(run)
		}
		return ShellTerminal{}, fmt.Errorf("run session command: send to terminal %s: %w", terminal.HandleID, err)
	}
	if tracked {
		go s.awaitSessionCommand(s.commandWatchCtx, run, in.OnDone)
	}
	return terminal, nil
}

// sessionCommandEnv gives a run what the session's agent has, so a command the
// agent wrote behaves the same in the tab: the session's identity (which `ao`
// needs) and its account folder (which `claude` needs). PATH needs nothing extra:
// the daemon inherits the login shell's, and the shell adds its own startup files.
func (s *Service) sessionCommandEnv(ctx context.Context, sessionID domain.SessionID, projectID domain.ProjectID, projectEnv map[string]string) (map[string]string, string, func(), error) {
	env := s.pinnedEnv(projectEnv)
	verifier, releasePreview, err := s.addSessionShellEnv(env, sessionID)
	if err != nil {
		return nil, "", nil, fmt.Errorf("run session command: preview capability: %w", err)
	}
	env["AO_PROJECT_ID"] = string(projectID)
	env["AO_DATA_DIR"] = s.dataDir
	if s.sessionAccountEnv != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		accountEnv, err := s.sessionAccountEnv(lookupCtx, sessionID)
		if err != nil {
			s.log.Warn("run session command: session account env incomplete", "sessionId", sessionID, "error", err)
		}
		maps.Copy(env, accountEnv)
	}
	return env, verifier, releasePreview, nil
}

// resolveSessionCommandWorkingDir is the session's live worktree. Unlike Cues,
// a session paused on a permission prompt may run one: the command runs in its
// own shell and its result is queued like any message.
func (s *Service) resolveSessionCommandWorkingDir(ctx context.Context, sessionID domain.SessionID) (string, domain.ProjectID, error) {
	if s.sessions == nil {
		return "", "", apierr.Internal("SHELL_TERMINAL_NO_SESSION_LOOKUP", "Session lookup is unavailable")
	}
	target, err := s.sessions.CueCommandSessionTarget(ctx, sessionID)
	if err != nil {
		return "", "", fmt.Errorf("run session command: resolve session %s: %w", sessionID, err)
	}
	if target.IsTerminated || target.Activity == domain.ActivityExited {
		return "", "", apierr.Conflict("SESSION_COMMAND_TARGET_CONFLICT", "This session has ended, so it cannot run commands", nil)
	}
	workspace := strings.TrimSpace(target.WorkspacePath)
	if workspace == "" || !filepath.IsAbs(workspace) {
		return "", "", apierr.Conflict("SESSION_COMMAND_TARGET_CONFLICT", "The session's worktree is unavailable", nil)
	}
	if info, statErr := os.Stat(workspace); statErr != nil || !info.IsDir() {
		return "", "", apierr.Conflict("SESSION_COMMAND_TARGET_CONFLICT", "The session's worktree is unavailable", nil)
	}
	return filepath.Clean(workspace), target.ProjectID, nil
}

// awaitSessionCommand watches one run and hands over its result. A run only
// leaves disk once the agent has its result or can no longer receive it.
func (s *Service) awaitSessionCommand(ctx context.Context, run pendingSessionCommand, onDone func(SessionCommandResult) error) {
	if run.Result == nil {
		result, ok := s.watchSessionCommand(ctx, run)
		if !ok {
			return
		}
		run.Result = &result
		if err := s.savePendingSessionCommand(run); err != nil {
			s.log.Warn("save terminal command result", "sessionId", run.SessionID, "handleId", run.Terminal.HandleID, "error", err)
		}
	}
	if onDone != nil {
		s.deliverSessionCommandResult(ctx, run, onDone)
		if ctx.Err() != nil {
			return
		}
	}
	s.removeSessionCommandFiles(run)
}

// watchSessionCommand waits for the run to end. It reports false only when ctx
// ends first, leaving the run for the next daemon to resume.
func (s *Service) watchSessionCommand(ctx context.Context, run pendingSessionCommand) (SessionCommandResult, bool) {
	result := SessionCommandResult{SessionID: run.SessionID, Terminal: run.Terminal, Command: run.Command}
	handle := ports.RuntimeHandle{ID: run.Terminal.HandleID}
	finished := func() bool {
		data, err := os.ReadFile(run.DoneFile)
		if err != nil {
			return false
		}
		status, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "done ")
		if !ok {
			return false
		}
		result.Finished = true
		result.ExitCode, _ = strconv.Atoi(status)
		return true
	}
	ticker := time.NewTicker(s.commandPoll)
	defer ticker.Stop()
	deadline := run.StartedAt.Add(sessionCommandMaxWait)
	recent := ""
	for polls := 1; ; polls++ {
		if finished() {
			break
		}
		if alive, err := s.runtime.IsChildAlive(ctx, handle); err == nil && !alive {
			// The shell may have reported just before it exited.
			finished()
			break
		}
		if s.now().After(deadline) {
			result.TimedOut = true
			break
		}
		if polls%sessionCommandSnapshotPolls == 0 {
			if raw, err := s.runtime.GetOutput(ctx, handle, sessionCommandOutputLines); err == nil && raw != "" {
				recent = raw
			}
		}
		select {
		case <-ctx.Done():
			return result, false
		case <-ticker.C:
		}
	}
	result.Duration = s.now().Sub(run.StartedAt).Round(time.Second)
	raw := s.finalSessionCommandOutput(ctx, handle, result.Finished)
	if raw == "" {
		raw = recent
	}
	result.Output = commandOutput(raw, run.Terminal.HandleID)
	result.LogPath = s.saveSessionCommandLog(result)
	return result, true
}

// finalSessionCommandOutput reads the buffer once the host has caught up with a
// finished command, shown by its end marker. A command interrupted with Ctrl-C
// may never print one, so the wait is bounded.
func (s *Service) finalSessionCommandOutput(ctx context.Context, handle ports.RuntimeHandle, finished bool) string {
	end := sessionCommandMarker("end", handle.ID)
	deadline := time.Now().Add(sessionCommandOutputSettle)
	for {
		raw, err := s.runtime.GetOutput(ctx, handle, sessionCommandOutputLines)
		if err != nil {
			raw = ""
		}
		if !finished || strings.Contains(raw, end) || time.Now().After(deadline) {
			return raw
		}
		select {
		case <-ctx.Done():
			return raw
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// deliverSessionCommandResult retries until the result arrives. Delivery can fail
// for a while, as when the chat is mid-handoff or still starting after a restart,
// and a dropped result leaves the agent waiting for an answer the user already gave.
func (s *Service) deliverSessionCommandResult(ctx context.Context, run pendingSessionCommand, onDone func(SessionCommandResult) error) {
	wait := s.commandRetryFirst
	giveUpAt := time.Now().Add(sessionCommandRetryWindow)
	for {
		err := onDone(*run.Result)
		if err == nil {
			return
		}
		if !s.sessionCanReceiveCommandResult(ctx, run.SessionID) || time.Now().After(giveUpAt) {
			s.log.Warn("giving up on terminal command result", "sessionId", run.SessionID, "handleId", run.Terminal.HandleID, "error", err)
			return
		}
		s.log.Warn("deliver terminal command result; will retry", "sessionId", run.SessionID, "handleId", run.Terminal.HandleID, "retryIn", wait, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, sessionCommandRetryLongest)
	}
}

// sessionCanReceiveCommandResult is false only on proof the session is gone: a
// failed lookup is not treated as one.
func (s *Service) sessionCanReceiveCommandResult(ctx context.Context, sessionID domain.SessionID) bool {
	if s.sessions == nil {
		return false
	}
	target, err := s.sessions.CueCommandSessionTarget(ctx, sessionID)
	if err != nil {
		var apiErr *apierr.Error
		return !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindNotFound
	}
	return !target.IsTerminated
}

// saveSessionCommandLog keeps the whole output for the agent to read; the
// message it gets carries only the tail.
func (s *Service) saveSessionCommandLog(run SessionCommandResult) string {
	if run.Output == "" || s.dataDir == "" {
		return ""
	}
	path := s.sessionCommandPath(run.SessionID, run.Terminal.HandleID, ".log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ""
	}
	content := "$ " + run.Command + "\n" + run.Output + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return ""
	}
	return path
}

func normalizeSessionCommand(command string) string {
	command = strings.ReplaceAll(command, "\r\n", "\n")
	return strings.TrimSpace(strings.ReplaceAll(command, "\r", "\n"))
}

// sessionCommandLine is the input typed into a shell that cannot report
// completion. Typed newlines would run each line as its own command, so several
// lines are grouped into one command list.
func sessionCommandLine(command string) string {
	if !strings.Contains(command, "\n") {
		return command
	}
	return "{\n" + command + "\n}"
}

// sessionCommandTitle names the tab after the command's first line.
func sessionCommandTitle(command string) string {
	first, _, _ := strings.Cut(command, "\n")
	first = strings.Join(strings.Fields(first), " ")
	if utf8.RuneCountInString(first) > sessionCommandTitleRunes {
		first = string([]rune(first)[:sessionCommandTitleRunes-1]) + "…"
	}
	if first == "" {
		return "Run"
	}
	return "Run: " + first
}

// terminalEscape matches CSI, OSC, DCS/PM/APC strings, charset selection,
// and single-character escapes.
var terminalEscape = regexp.MustCompile(`\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[PX^_][^\x1b]*\x1b\\|[()*+][0-9A-Za-z]|[=>78DEHMNOc])`)

// fullScreenNote stands in for a full-screen program's screen, whose frames are
// not a transcript and would only bury the output around them.
const fullScreenNote = "[a full-screen program ran here; its screen is not part of this output]"

// plainTerminalText renders raw terminal output as the lines a reader saw:
// escapes removed, and redraws (progress bars, spinners, inline TUIs that move
// the cursor back up) collapsed to their final state.
func plainTerminalText(raw string) string {
	var screen textScreen
	fullScreen := false
	for raw != "" {
		text, escape := raw, ""
		if loc := terminalEscape.FindStringIndex(raw); loc != nil {
			text, escape = raw[:loc[0]], raw[loc[0]:loc[1]]
			raw = raw[loc[1]:]
		} else {
			raw = ""
		}
		if !fullScreen {
			screen.write(text)
		}
		switch {
		case isFullScreenSwitch(escape, 'h'):
			if !fullScreen {
				screen.note(fullScreenNote)
			}
			fullScreen = true
		case isFullScreenSwitch(escape, 'l'):
			fullScreen = false
		case !fullScreen && strings.HasPrefix(escape, "\x1b["):
			screen.apply(escape)
		}
	}
	return screen.String()
}

func isFullScreenSwitch(escape string, action byte) bool {
	for _, mode := range []string{"1049", "1047", "47"} {
		if escape == "\x1b[?"+mode+string(action) {
			return true
		}
	}
	return false
}

// textScreen is just enough of a terminal to replay output line by line. It has
// no fixed height, so it keeps everything that scrolled away, and no fixed width,
// so a line the terminal wrapped counts as one row.
type textScreen struct {
	rows     [][]rune
	row, col int
}

// textScreenMaxCol keeps a stray cursor move from allocating a huge padded line.
const textScreenMaxCol = 4096

func (t *textScreen) write(text string) {
	for _, r := range text {
		switch {
		case r == '\n':
			t.row++
			t.col = 0
			t.growToRow()
		case r == '\r':
			t.col = 0
		case r == '\b':
			t.col = max(0, t.col-1)
		case r == '\t' || !unicode.IsControl(r):
			t.put(r)
		}
	}
}

func (t *textScreen) growToRow() {
	for len(t.rows) <= t.row {
		t.rows = append(t.rows, nil)
	}
}

func (t *textScreen) put(r rune) {
	t.growToRow()
	line := t.rows[t.row]
	for len(line) < t.col {
		line = append(line, ' ')
	}
	if t.col < len(line) {
		line[t.col] = r
	} else {
		line = append(line, r)
	}
	t.rows[t.row] = line
	t.col++
}

// note adds a line of its own, leaving the cursor on the line after it.
func (t *textScreen) note(text string) {
	if t.row < len(t.rows) && len(t.rows[t.row]) > 0 {
		t.write("\n")
	}
	t.write(text + "\n")
}

// apply handles the cursor moves and erases redraws use. Absolute rows mean
// nothing without a known height, so a position only sets the column; programs
// that rely on it use the alternate screen, which is not replayed at all.
func (t *textScreen) apply(sequence string) {
	final := sequence[len(sequence)-1]
	params := sequence[2 : len(sequence)-1]
	if strings.HasPrefix(params, "?") {
		return
	}
	first, second, _ := strings.Cut(params, ";")
	value, _ := strconv.Atoi(first)
	count := max(1, value)
	switch final {
	case 'A':
		t.row = max(0, t.row-count)
	case 'B':
		t.row = max(t.row, min(t.row+count, len(t.rows)-1))
	case 'C':
		t.col = min(t.col+count, textScreenMaxCol)
	case 'D':
		t.col = max(0, t.col-count)
	case 'E':
		t.row = max(t.row, min(t.row+count, len(t.rows)-1))
		t.col = 0
	case 'F':
		t.row = max(0, t.row-count)
		t.col = 0
	case 'G', '`':
		t.col = min(count-1, textScreenMaxCol)
	case 'H', 'f':
		column, _ := strconv.Atoi(second)
		t.col = min(max(1, column)-1, textScreenMaxCol)
	case 'K':
		if t.row < len(t.rows) {
			t.eraseLine(value)
		}
	case 'J':
		switch value {
		case 2, 3:
			t.rows, t.row, t.col = nil, 0, 0
		case 0:
			if t.row < len(t.rows) {
				t.eraseLine(0)
				t.rows = t.rows[:t.row+1]
			}
		}
	}
}

// eraseLine applies the K modes: 0 from the cursor on, 1 up to it, 2 all of it.
func (t *textScreen) eraseLine(mode int) {
	line := t.rows[t.row]
	switch mode {
	case 0:
		t.rows[t.row] = line[:min(t.col, len(line))]
	case 1:
		for i := range min(t.col+1, len(line)) {
			line[i] = ' '
		}
	case 2:
		t.rows[t.row] = nil
	}
}

func (t *textScreen) String() string {
	lines := make([]string, len(t.rows))
	for i, line := range t.rows {
		lines[i] = strings.TrimRightFunc(string(line), unicode.IsSpace)
	}
	return strings.Join(lines, "\n")
}

// commandOutput is what the command printed: the text between its markers. The
// start marker is gone once more than the buffer scrolled past, and the end marker
// is absent from a run that did not reach its end, which leaves the part there is.
func commandOutput(raw, handleID string) string {
	start := sessionCommandMarker("start", handleID)
	if i := strings.LastIndex(raw, start); i >= 0 {
		raw = raw[i+len(start):]
	}
	if i := strings.Index(raw, sessionCommandMarker("end", handleID)); i >= 0 {
		raw = raw[:i]
	}
	return strings.Trim(plainTerminalText(raw), "\n")
}

const (
	agentMessageOutputLines = 60
	agentMessageOutputBytes = 8 << 10
)

// AgentMessage is the chat message that hands the result to the agent: the
// command, how it ended, and the tail of its output, pointing at the full log.
func (r SessionCommandResult) AgentMessage() string {
	var b strings.Builder
	switch {
	case r.Finished:
		fmt.Fprintf(&b, "I ran this in a terminal in your worktree; it exited with code %d after %s.\n\n", r.ExitCode, r.Duration)
	case r.TimedOut:
		fmt.Fprintf(&b, "I started this in a terminal in your worktree; it was still running after %s, so this is the output so far.\n\n", r.Duration)
	default:
		fmt.Fprintf(&b, "I started this in a terminal in your worktree, but the terminal closed after %s, before the command finished.\n\n", r.Duration)
	}
	writeFence(&b, "sh", r.Command)
	output := strings.TrimSpace(r.Output)
	if output == "" {
		b.WriteString("\nIt printed no output.")
		return b.String()
	}
	tail, omitted := outputTail(output)
	b.WriteString("\n")
	switch {
	case omitted > 0 && r.LogPath != "":
		fmt.Fprintf(&b, "Last %d lines of output (full output: `%s`):\n\n", strings.Count(tail, "\n")+1, r.LogPath)
	case omitted > 0:
		fmt.Fprintf(&b, "Last %d lines of output:\n\n", strings.Count(tail, "\n")+1)
	default:
		b.WriteString("Output:\n\n")
	}
	writeFence(&b, "text", tail)
	return strings.TrimRight(b.String(), "\n")
}

// outputTail keeps the last lines within the message budget and reports how
// many lines it left out.
func outputTail(output string) (string, int) {
	lines := strings.Split(output, "\n")
	start := max(0, len(lines)-agentMessageOutputLines)
	size := 0
	for i := len(lines) - 1; i >= start; i-- {
		size += len(lines[i]) + 1
		if size > agentMessageOutputBytes {
			start = i + 1
			break
		}
	}
	return strings.Join(lines[start:], "\n"), start
}

// writeFence wraps text in a code fence longer than any backtick run inside.
func writeFence(b *strings.Builder, lang, text string) {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	b.WriteString(fence + lang + "\n" + text + "\n" + fence + "\n")
}
