package shellterm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
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
	// closes, or the wait gives up. Nil when the caller only wants the shell.
	OnDone func(SessionCommandResult)
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
)

// RunSessionCommand opens a new shell tab in the session's worktree, types the
// command, and returns the tab. When the shell can report completion (zsh,
// bash), OnDone later gets the exit status and output.
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
	env := s.pinnedEnv(projectEnv)
	readiness, err := prepareCommandShellReadiness(s.dataDir, argv, env)
	if err != nil {
		return ShellTerminal{}, err
	}
	defer readiness.cleanup()
	env = agentlaunch.MergeEnv(env, readiness.env)
	terminal, err := s.openTerminal(ctx, openTerminalConfig{argv: readiness.argv, env: env, projectEnv: projectEnv, projectID: projectID,
		sessionID: in.SessionID, workingDir: workingDir, title: sessionCommandTitle(command)})
	if err != nil {
		return ShellTerminal{}, err
	}
	handle := ports.RuntimeHandle{ID: terminal.HandleID}
	if err := s.waitForCueShellReady(ctx, handle, readiness.file); err != nil {
		return ShellTerminal{}, err
	}
	// The startup output, so the result can leave it (and the prompt) out.
	before, _ := s.runtime.GetOutput(ctx, handle, sessionCommandOutputLines)
	startedAt := s.now()
	if err := s.runtime.SendMessage(ctx, handle, sessionCommandLine(command)); err != nil {
		return ShellTerminal{}, fmt.Errorf("run session command: send to terminal %s: %w", terminal.HandleID, err)
	}
	if readiness.doneFile != "" && in.OnDone != nil {
		run := SessionCommandResult{SessionID: in.SessionID, Terminal: terminal, Command: command}
		go s.awaitSessionCommand(context.WithoutCancel(ctx), run, readiness.doneFile, before, startedAt, in.OnDone)
	}
	return terminal, nil
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

func (s *Service) awaitSessionCommand(ctx context.Context, run SessionCommandResult, doneFile, before string, startedAt time.Time, onDone func(SessionCommandResult)) {
	handle := ports.RuntimeHandle{ID: run.Terminal.HandleID}
	ticker := time.NewTicker(sessionCommandPoll)
	defer ticker.Stop()
	deadline := startedAt.Add(sessionCommandMaxWait)
	for {
		if data, err := os.ReadFile(doneFile); err == nil {
			if status, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "done "); ok {
				run.Finished = true
				run.ExitCode, _ = strconv.Atoi(status)
				break
			}
		}
		if alive, err := s.runtime.IsChildAlive(ctx, handle); err == nil && !alive {
			break
		}
		if s.now().After(deadline) {
			run.TimedOut = true
			break
		}
		<-ticker.C
	}
	_ = os.Remove(doneFile)
	run.Duration = s.now().Sub(startedAt).Round(time.Second)
	if after, err := s.runtime.GetOutput(ctx, handle, sessionCommandOutputLines); err == nil {
		run.Output = commandOutput(before, after, run.Command)
		run.LogPath = s.saveSessionCommandLog(run)
	}
	onDone(run)
}

// saveSessionCommandLog keeps the whole output for the agent to read; the
// message it gets carries only the tail.
func (s *Service) saveSessionCommandLog(run SessionCommandResult) string {
	if run.Output == "" || s.dataDir == "" {
		return ""
	}
	dir := filepath.Join(s.dataDir, "command-runs", string(run.SessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	path := filepath.Join(dir, run.Terminal.HandleID+".log")
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

// sessionCommandLine is the input typed into the shell. Typed newlines would
// run each line as its own command (and fire the completion hook after the
// first), so several lines are grouped into one command list.
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

// plainTerminalText renders raw terminal output as the lines a reader saw:
// escapes removed, carriage-return redraws (progress bars) collapsed to their
// final state, and backspaces applied.
func plainTerminalText(raw string) string {
	lines := strings.Split(terminalEscape.ReplaceAllString(raw, ""), "\n")
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if cut := strings.LastIndexByte(line, '\r'); cut >= 0 {
			line = line[cut+1:]
		}
		var b []rune
		for _, r := range line {
			switch {
			case r == '\b':
				if len(b) > 0 {
					b = b[:len(b)-1]
				}
			case r == '\t' || !unicode.IsControl(r):
				b = append(b, r)
			}
		}
		lines[i] = strings.TrimRightFunc(string(b), unicode.IsSpace)
	}
	return strings.Join(lines, "\n")
}

// commandOutput is what the command printed: the output after the startup
// snapshot, without the shell's echo of the command and blank edges.
func commandOutput(before, after, command string) string {
	raw := after
	if before != "" && strings.HasPrefix(after, before) {
		raw = after[len(before):]
	}
	lines := strings.Split(plainTerminalText(raw), "\n")
	commandLines := strings.Split(command, "\n")
	if last := lastNonEmptyLine(commandLines); last != "" {
		// The echo (prompt, continuation prompts) ends with the command's last
		// line, then a group's closing brace.
		limit := min(len(lines), len(commandLines)+8)
		for i := 0; i < limit; i++ {
			if strings.Contains(lines[i], last) {
				lines = lines[i+1:]
				if len(lines) > 0 && len(commandLines) > 1 && isGroupCloseEcho(lines[0]) {
					lines = lines[1:]
				}
				break
			}
		}
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// isGroupCloseEcho matches the echoed "}" that sessionCommandLine adds, after
// a continuation prompt such as "cursh> " or "> ".
func isGroupCloseEcho(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasSuffix(line, "}") && utf8.RuneCountInString(line) <= 16 && !strings.Contains(line, "{")
}

func lastNonEmptyLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		// A long line wraps in the echo; its end is on the last row.
		if utf8.RuneCountInString(line) > 30 {
			line = string([]rune(line)[utf8.RuneCountInString(line)-30:])
		}
		return line
	}
	return ""
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
