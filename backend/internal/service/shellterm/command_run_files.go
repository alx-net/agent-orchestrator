package shellterm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// pendingSessionCommand is a run whose result has not reached the agent yet. It
// lives on disk because terminal hosts outlive the daemon: AO restarts on every
// update, and a run that spans a restart must still report when it ends.
type pendingSessionCommand struct {
	SessionID domain.SessionID `json:"sessionId"`
	Terminal  ShellTerminal    `json:"terminal"`
	Command   string           `json:"command"`
	DoneFile  string           `json:"doneFile"`
	StartedAt time.Time        `json:"startedAt"`
	// Result is set once the run ended. A retry after another restart then sends
	// the identical message, which the chat recognizes by its idempotency key.
	Result *SessionCommandResult `json:"result,omitempty"`
}

func (s *Service) sessionCommandPath(sessionID domain.SessionID, handleID, ext string) string {
	return filepath.Join(s.dataDir, "command-runs", string(sessionID), handleID+ext)
}

// sessionCommandMarker delimits a run's output in the terminal's raw stream. A
// private OSC prints nothing and terminals ignore it, and unlike matching the
// echo or the prompt it cannot be fooled by what the shell or command draws.
func sessionCommandMarker(kind, handleID string) string {
	return "\x1b]6973;ao-run-" + kind + ";" + handleID + "\x07"
}

// sessionCommandRunner is what the typed line sources in the interactive shell.
// The command itself runs in a subshell, so `set -e`, `set -u`, `exit`, and an
// EXIT trap (all common in what agents write) end the command rather than the
// user's tab or their later prompts. The subshell's directory and exports are
// then applied to the tab, so `cd` and `export` still carry over; other shell
// state does not. The state file can hold exported secrets: it sits in a 0700
// directory and is gone as soon as the shell has read it.
func sessionCommandRunner(script, state, handleID, command string) string {
	quotedCommand := shellSingleQuote(command)
	quotedState := shellSingleQuote(state)
	lines := []string{
		// Up-arrow should bring back the command, not this runner.
		`if [ -n "${ZSH_VERSION-}" ]; then print -rs -- ` + quotedCommand + `; else history -s -- ` + quotedCommand + `; fi`,
		`printf '%s\n' ` + shellSingleQuote("$ "+command),
		`printf '\033]6973;ao-run-start;%s\007' ` + shellSingleQuote(handleID),
		`(`,
		// An interactive zsh writes history when any subshell exits.
		`	unset HISTFILE`,
		`	__ao_run_save() {`,
		`		__ao_run_status=$?`,
		`		{ printf '[ "$PWD" = %q ] || builtin cd -- %q\n' "$PWD" "$PWD"; export -p; } >| ` + quotedState + ` 2>/dev/null`,
		`		exit "$__ao_run_status"`,
		`	}`,
		`	trap __ao_run_save EXIT`,
		`	. ` + shellSingleQuote(script),
		`)`,
		`__ao_run_status=$?`,
		// The newline is required: the host's output buffer only returns whole
		// lines, and a command may have left the cursor mid-line.
		`printf '\033]6973;ao-run-end;%s\007\n' ` + shellSingleQuote(handleID),
		`if [ -f ` + quotedState + ` ]; then . ` + quotedState + ` 2>/dev/null; command rm -f ` + quotedState + `; fi`,
		`eval "unset __ao_run_status; return $__ao_run_status"`,
	}
	return strings.Join(lines, "\n") + "\n"
}

// prepareSessionCommand writes the run's files and returns the one short line
// to type. Typing the command itself would leave it to the line editor, kill the
// tab on `exit` or `set -e`, and hide its status.
func (s *Service) prepareSessionCommand(run pendingSessionCommand) (string, error) {
	handleID := run.Terminal.HandleID
	script := s.sessionCommandPath(run.SessionID, handleID, ".sh")
	runner := s.sessionCommandPath(run.SessionID, handleID, ".run")
	state := s.sessionCommandPath(run.SessionID, handleID, ".state")
	if err := os.MkdirAll(filepath.Dir(script), 0o700); err != nil {
		return "", fmt.Errorf("create command run directory: %w", err)
	}
	if err := os.WriteFile(script, []byte(run.Command+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write command script: %w", err)
	}
	if err := os.WriteFile(runner, []byte(sessionCommandRunner(script, state, handleID, run.Command)), 0o600); err != nil {
		return "", fmt.Errorf("write command runner: %w", err)
	}
	if err := s.savePendingSessionCommand(run); err != nil {
		return "", err
	}
	// The leading space keeps the runner out of history where the shell allows it.
	return " . " + shellSingleQuote(runner), nil
}

func (s *Service) savePendingSessionCommand(run pendingSessionCommand) error {
	data, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("encode pending command: %w", err)
	}
	path := s.sessionCommandPath(run.SessionID, run.Terminal.HandleID, ".json")
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return fmt.Errorf("write pending command: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("write pending command: %w", err)
	}
	return nil
}

// removeSessionCommandFiles forgets a run. Only its output log stays, because
// the message the agent got may point at it.
func (s *Service) removeSessionCommandFiles(run pendingSessionCommand) {
	for _, ext := range []string{".json", ".sh", ".run", ".state"} {
		_ = os.Remove(s.sessionCommandPath(run.SessionID, run.Terminal.HandleID, ext))
	}
	_ = os.Remove(run.DoneFile)
}

// ResumeSessionCommands picks up the runs a previous daemon left unfinished. Call
// it once at boot, before clients can start new runs. ctx bounds every watch from
// then on, new runs included; a run still pending when it ends stays on disk for
// the next boot.
func (s *Service) ResumeSessionCommands(ctx context.Context, onDone func(SessionCommandResult) error) {
	s.commandWatchCtx = ctx
	root := filepath.Join(s.dataDir, "command-runs")
	sessions, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Warn("resume terminal commands: read directory", "error", err)
		}
		return
	}
	for _, session := range sessions {
		dir := filepath.Join(root, session.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			s.log.Warn("resume terminal commands: read directory", "error", err)
			continue
		}
		for _, file := range files {
			if filepath.Ext(file.Name()) != ".json" {
				continue
			}
			path := filepath.Join(dir, file.Name())
			run, err := readPendingSessionCommand(path)
			if err != nil {
				s.log.Warn("resume terminal commands: dropping unreadable run", "path", path, "error", err)
				_ = os.Remove(path)
				continue
			}
			s.log.Info("resuming terminal command", "sessionId", run.SessionID, "handleId", run.Terminal.HandleID)
			go s.awaitSessionCommand(ctx, run, onDone)
		}
	}
}

func readPendingSessionCommand(path string) (pendingSessionCommand, error) {
	var run pendingSessionCommand
	data, err := os.ReadFile(path) //nolint:gosec // path is under the daemon's own data dir
	if err != nil {
		return run, err
	}
	if err := json.Unmarshal(data, &run); err != nil {
		return run, err
	}
	if run.SessionID == "" || run.Terminal.HandleID == "" {
		return run, fmt.Errorf("missing session or terminal")
	}
	return run, nil
}
