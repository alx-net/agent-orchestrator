//go:build !windows

package shellterm

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The completion hook must report the exit status of the whole typed command
// once, even when the user's own prompt hooks run commands of their own.
func TestCommandReadinessReportsExitStatusOfWholeCommand(t *testing.T) {
	for _, name := range []string{"zsh", "bash"} {
		t.Run(name, func(t *testing.T) {
			path, err := exec.LookPath(name)
			if err != nil {
				t.Skipf("%s unavailable: %v", name, err)
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("ZDOTDIR", "")
			env := map[string]string{"HOME": home}
			switch name {
			case "zsh":
				hook := "function user_hook { true; }\nprecmd_functions+=(user_hook)\n"
				if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(hook), 0o600); err != nil {
					t.Fatal(err)
				}
			case "bash":
				env["PROMPT_COMMAND"] = "true"
			}
			ready, err := prepareCommandShellReadiness(t.TempDir(), []string{path}, env)
			if err != nil {
				t.Fatal(err)
			}
			defer ready.cleanup()
			if ready.doneFile == "" {
				t.Fatal("no completion file for " + name)
			}
			cmd := exec.Command(ready.argv[0], ready.argv[1:]...)
			cmd.Dir = home
			cmd.Env = append(os.Environ(), "HOME="+home, "BASH_SILENCE_DEPRECATION_WARNING=1")
			for key, value := range ready.env {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			terminal, err := pty.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = terminal.Close(); _ = cmd.Wait() }()
			go func() { _, _ = io.Copy(io.Discard, terminal) }()
			deadline := time.Now().Add(10 * time.Second)
			waitFor := func(file, want string) {
				t.Helper()
				var got []byte
				for time.Now().Before(deadline) {
					if got, _ = os.ReadFile(file); string(got) == want {
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
				t.Fatalf("%s = %q, want %q", filepath.Base(file), got, want)
			}
			waitFor(ready.file, "ready")
			// The first line succeeds; a per-line hook would report 0 too early.
			line := sessionCommandLine("sleep 0.3\n(exit 3)")
			if _, err := io.WriteString(terminal, line+"\r"); err != nil {
				t.Fatal(err)
			}
			waitFor(ready.doneFile, "done 3")
			// Later commands are the user's own and must not overwrite it.
			if err := os.Remove(ready.doneFile); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(terminal, "touch later\r"); err != nil {
				t.Fatal(err)
			}
			waitFor(filepath.Join(home, "later"), "")
			time.Sleep(200 * time.Millisecond)
			if _, err := os.Stat(ready.doneFile); !os.IsNotExist(err) {
				t.Fatalf("completion reported again after a later command: %v", err)
			}
		})
	}
}

// newCommandRunService is a service whose watches are fast, with a session that
// is paused on a permission prompt: a command still runs in its own shell.
func newCommandRunService(t *testing.T, rt *fakeShellRuntime) (*Service, string, *fakeSessionWorkspaceLocator) {
	t.Helper()
	t.Setenv("SHELL", "/bin/zsh")
	workspace := t.TempDir()
	rt.cueReady = true
	locator := &fakeSessionWorkspaceLocator{
		sessions: map[domain.SessionID]fakeSessionWorkspace{"session": {workspacePath: workspace, projectID: "portfolio", activity: domain.ActivityBlocked}},
	}
	svc := newTestServiceWithSessions(rt, &fakeShellTerminalStore{}, &fakeProjectRootLocator{}, locator)
	svc.dataDir = t.TempDir()
	svc.commandPoll = 5 * time.Millisecond
	svc.commandRetryFirst = 5 * time.Millisecond
	return svc, workspace, locator
}

func awaitRunResult(t *testing.T, results <-chan SessionCommandResult) SessionCommandResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
		return SessionCommandResult{}
	}
}

// finishRun answers as the shell and the host would when the command has ended.
func finishRun(t *testing.T, rt *fakeShellRuntime, handleID, output string, status int) {
	t.Helper()
	rt.setOutput("startup\r\n" + sessionCommandMarker("start", handleID) + output + sessionCommandMarker("end", handleID) + "\r\n")
	doneFile := rt.created[0].Env["AO_CUE_READY_FILE"] + ".done"
	if err := os.WriteFile(doneFile, []byte("done "+strconv.Itoa(status)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commandRunFiles(t *testing.T, dataDir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dataDir, "command-runs", "session", "*"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, filepath.Ext(match))
	}
	sort.Strings(names)
	return names
}

func TestRunSessionCommandReportsResultFromTheSessionWorktree(t *testing.T) {
	rt := newFakeShellRuntime()
	svc, workspace, _ := newCommandRunService(t, rt)
	results := make(chan SessionCommandResult, 1)
	terminal, err := svc.RunSessionCommand(context.Background(), RunSessionCommandInput{
		SessionID: "session", Command: "echo hi\r\n(exit 3)\n", OnDone: func(r SessionCommandResult) error { results <- r; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.WorkingDir != workspace || terminal.SessionID != "session" || terminal.Title != "Run: echo hi" {
		t.Fatalf("terminal = %+v", terminal)
	}
	dir := filepath.Join(svc.dataDir, "command-runs", "session")
	// Only one short line is typed; the command stays out of the line editor.
	if sent := <-rt.sentCh; sent.input != " . '"+filepath.Join(dir, terminal.HandleID+".run")+"'" {
		t.Fatalf("typed %q", sent.input)
	}
	script, err := os.ReadFile(filepath.Join(dir, terminal.HandleID+".sh"))
	if err != nil || string(script) != "echo hi\n(exit 3)\n" {
		t.Fatalf("script = %q, %v", script, err)
	}
	if got := commandRunFiles(t, svc.dataDir); !slices.Equal(got, []string{".json", ".run", ".sh"}) {
		t.Fatalf("files while running = %v", got)
	}
	finishRun(t, rt, terminal.HandleID, "hi\r\n", 3)
	result := awaitRunResult(t, results)
	if !result.Finished || result.ExitCode != 3 || result.Output != "hi" || result.Command != "echo hi\n(exit 3)" {
		t.Fatalf("result = %+v", result)
	}
	if want := filepath.Join(dir, terminal.HandleID+".log"); result.LogPath != want {
		t.Fatalf("log = %q, want %q", result.LogPath, want)
	}
	waitForCommandFiles(t, svc.dataDir, ".log")
	doneFile := rt.created[0].Env["AO_CUE_READY_FILE"] + ".done"
	if _, err := os.Stat(doneFile); !os.IsNotExist(err) {
		t.Fatalf("completion file left behind: %v", err)
	}
}

func waitForCommandFiles(t *testing.T, dataDir string, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := commandRunFiles(t, dataDir)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("files = %v, want %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A failed send must not lose the result: it stays on disk and is sent again.
func TestRunSessionCommandRetriesDeliveryUntilItArrives(t *testing.T) {
	rt := newFakeShellRuntime()
	svc, _, _ := newCommandRunService(t, rt)
	var attempts atomic.Int32
	results := make(chan SessionCommandResult, 1)
	terminal, err := svc.RunSessionCommand(context.Background(), RunSessionCommandInput{
		SessionID: "session", Command: "plan", OnDone: func(r SessionCommandResult) error {
			if attempts.Add(1) < 3 {
				return errors.New("chat is mid-handoff")
			}
			results <- r
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishRun(t, rt, terminal.HandleID, "No changes.\r\n", 0)
	if result := awaitRunResult(t, results); result.Output != "No changes." || attempts.Load() != 3 {
		t.Fatalf("result = %+v after %d attempts", result, attempts.Load())
	}
	waitForCommandFiles(t, svc.dataDir, ".log")
}

// A session that ended cannot receive the result, so retrying would never end.
func TestRunSessionCommandStopsRetryingOnceTheSessionIsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		gone func(*fakeSessionWorkspaceLocator)
	}{
		{"terminated", func(l *fakeSessionWorkspaceLocator) {
			l.sessions = map[domain.SessionID]fakeSessionWorkspace{"session": {workspacePath: "/ws", terminated: true}}
		}},
		{"removed", func(l *fakeSessionWorkspaceLocator) { l.sessions = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeShellRuntime()
			svc, _, locator := newCommandRunService(t, rt)
			terminal, err := svc.RunSessionCommand(context.Background(), RunSessionCommandInput{
				SessionID: "session", Command: "plan", OnDone: func(SessionCommandResult) error { return errors.New("no such session") },
			})
			if err != nil {
				t.Fatal(err)
			}
			tc.gone(locator)
			finishRun(t, rt, terminal.HandleID, "x\r\n", 0)
			waitForCommandFiles(t, svc.dataDir, ".log")
		})
	}
}

// A failed session lookup is not proof the session is gone.
func TestSessionCanReceiveCommandResultOnFailedLookup(t *testing.T) {
	rt := newFakeShellRuntime()
	svc, _, locator := newCommandRunService(t, rt)
	locator.err = errors.New("database busy")
	if !svc.sessionCanReceiveCommandResult(context.Background(), "session") {
		t.Fatal("a failed lookup was taken as the session being gone")
	}
}

// A result saved before a restart goes out unchanged, so the chat's idempotency
// key recognizes it if the previous daemon had already sent it.
func TestResumeSessionCommandsResendsTheSavedResult(t *testing.T) {
	rt := newFakeShellRuntime()
	svc, _, _ := newCommandRunService(t, rt)
	saved := SessionCommandResult{SessionID: "session", Terminal: ShellTerminal{HandleID: "h1"}, Command: "plan", Finished: true, ExitCode: 2, Duration: 74 * time.Second, Output: "boom"}
	run := pendingSessionCommand{SessionID: "session", Terminal: saved.Terminal, Command: "plan", DoneFile: filepath.Join(svc.dataDir, "done"), StartedAt: svc.now(), Result: &saved}
	if err := os.MkdirAll(filepath.Dir(svc.sessionCommandPath("session", "h1", ".json")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := svc.savePendingSessionCommand(run); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.sessionCommandPath("session", "broken", ".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := make(chan SessionCommandResult, 1)
	svc.ResumeSessionCommands(context.Background(), func(r SessionCommandResult) error { results <- r; return nil })
	if got := awaitRunResult(t, results); got.AgentMessage() != saved.AgentMessage() {
		t.Fatalf("resent %q, want %q", got.AgentMessage(), saved.AgentMessage())
	}
	waitForCommandFiles(t, svc.dataDir)
}

// A run with no recorded output still reports when its tab goes away: what the
// host had shown before closing is all that is left of it.
func TestRunSessionCommandKeepsRecentOutputWhenTheTabCloses(t *testing.T) {
	inner := newFakeShellRuntime()
	rt := &closableRuntime{fakeShellRuntime: inner}
	svc, _, _ := newCommandRunService(t, inner)
	svc.runtime = rt
	results := make(chan SessionCommandResult, 1)
	terminal, err := svc.RunSessionCommand(context.Background(), RunSessionCommandInput{
		SessionID: "session", Command: "long", OnDone: func(r SessionCommandResult) error { results <- r; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	inner.setOutput(sessionCommandMarker("start", terminal.HandleID) + "partial\r\n")
	time.Sleep(20 * sessionCommandSnapshotPolls * svc.commandPoll)
	inner.setOutput("")
	rt.closed.Store(true)
	result := awaitRunResult(t, results)
	if result.Finished || result.TimedOut || result.Output != "partial" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.AgentMessage(), "the terminal closed") {
		t.Fatalf("message = %q", result.AgentMessage())
	}
}

type closableRuntime struct {
	*fakeShellRuntime
	closed atomic.Bool
}

func (r *closableRuntime) IsChildAlive(ctx context.Context, handle ports.RuntimeHandle) (bool, error) {
	if r.closed.Load() {
		return false, nil
	}
	return r.fakeShellRuntime.IsChildAlive(ctx, handle)
}

// A command the agent wrote must find what the agent had: its session's identity
// for `ao`, and its account folder for `claude`.
func TestRunSessionCommandGivesTheTabTheAgentsEnvironment(t *testing.T) {
	rt := newFakeShellRuntime()
	svc, _, _ := newCommandRunService(t, rt)
	svc.SetSessionAccountEnv(func(_ context.Context, id domain.SessionID) (map[string]string, error) {
		return map[string]string{"CLAUDE_CONFIG_DIR": "/accounts/" + string(id)}, errors.New("one provider failed")
	})
	t.Setenv("AO_BROWSER_CAPABILITY", "worker-only")
	_, err := svc.RunSessionCommand(context.Background(), RunSessionCommandInput{
		SessionID: "session", Command: "ao status", OnDone: func(SessionCommandResult) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	env := rt.created[0].Env
	for key, want := range map[string]string{
		"AO_SESSION_ID":         "session",
		"AO_PROJECT_ID":         "portfolio",
		"AO_DATA_DIR":           svc.dataDir,
		"CLAUDE_CONFIG_DIR":     "/accounts/session",
		"AO_BROWSER_CAPABILITY": "",
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
	if env["AO_PREVIEW_CAPABILITY"] == "" {
		t.Error("the tab cannot use `ao preview`")
	}
}

// macOS's global zshrc derives HISTFILE from the temporary ZDOTDIR, whose
// directory is gone by the time the shell saves its history.
func TestCommandShellDoesNotKeepPathsIntoTheReadinessWrapper(t *testing.T) {
	path, err := exec.LookPath("zsh")
	if err != nil {
		t.Skipf("zsh unavailable: %v", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	report := filepath.Join(home, "seen")
	rc := "print -r -- \"${ZDOTDIR-unset} $HISTFILE\" >| " + shellSingleQuote(report) + "\n"
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := prepareCommandShellReadiness(t.TempDir(), []string{path}, map[string]string{"HOME": home})
	if err != nil {
		t.Fatal(err)
	}
	defer ready.cleanup()
	wrapper := ready.env["ZDOTDIR"]
	// Stands in for /etc/zshrc on macOS, which the wrapper cannot override.
	if err := os.WriteFile(filepath.Join(wrapper, ".zshenv"), append(readFile(t, filepath.Join(wrapper, ".zshenv")), []byte("HISTFILE=${ZDOTDIR:-$HOME}/.zsh_history\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ready.argv[0], ready.argv[1:]...)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home)
	for key, value := range ready.env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	terminal, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = terminal.Close(); _ = cmd.Wait() }()
	go func() { _, _ = io.Copy(io.Discard, terminal) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(report); err == nil && len(data) > 0 {
			if got, want := strings.TrimSpace(string(data)), "unset "+filepath.Join(home, ".zsh_history"); got != want {
				t.Fatalf("user's .zshrc saw %q, want %q", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the user's .zshrc never ran")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRunSessionCommandRefusesEndedSessions(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	rt := newFakeShellRuntime()
	svc := newTestServiceWithSessions(rt, &fakeShellTerminalStore{}, &fakeProjectRootLocator{}, &fakeSessionWorkspaceLocator{
		sessions: map[domain.SessionID]fakeSessionWorkspace{"session": {workspacePath: t.TempDir(), projectID: "portfolio", terminated: true}},
	})
	if _, err := svc.RunSessionCommand(context.Background(), RunSessionCommandInput{SessionID: "session", Command: "ls"}); err == nil {
		t.Fatal("ran a command in an ended session")
	}
	if len(rt.created) != 0 {
		t.Fatalf("opened %d terminals", len(rt.created))
	}
}
