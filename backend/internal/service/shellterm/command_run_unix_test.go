//go:build !windows

package shellterm

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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

func TestRunSessionCommandReportsResultFromTheSessionWorktree(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	workspace := t.TempDir()
	rt := newFakeShellRuntime()
	rt.cueReady = true
	rt.setOutput("Last login\n")
	svc := newTestServiceWithSessions(rt, &fakeShellTerminalStore{}, &fakeProjectRootLocator{}, &fakeSessionWorkspaceLocator{
		// Paused on a permission prompt: the command still runs in its own shell.
		sessions: map[domain.SessionID]fakeSessionWorkspace{"session": {workspacePath: workspace, projectID: "portfolio", activity: domain.ActivityBlocked}},
	})
	svc.dataDir = t.TempDir()
	results := make(chan SessionCommandResult, 1)
	terminal, err := svc.RunSessionCommand(context.Background(), RunSessionCommandInput{
		SessionID: "session", Command: "echo hi\r\n(exit 3)\n", OnDone: func(r SessionCommandResult) { results <- r },
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.WorkingDir != workspace || terminal.SessionID != "session" || terminal.Title != "Run: echo hi" {
		t.Fatalf("terminal = %+v", terminal)
	}
	if sent := <-rt.sentCh; sent.input != "{\necho hi\n(exit 3)\n}" {
		t.Fatalf("typed %q", sent.input)
	}
	rt.setOutput("Last login\n\x1b[1m~/ws\x1b[0m {\ncursh> echo hi\ncursh> (exit 3)\ncursh> }\nhi\n")
	doneFile := rt.created[0].Env["AO_CUE_READY_FILE"] + ".done"
	if err := os.WriteFile(doneFile, []byte("done 3"), 0o600); err != nil {
		t.Fatal(err)
	}
	var result SessionCommandResult
	select {
	case result = <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
	if !result.Finished || result.ExitCode != 3 || result.Output != "hi" || result.Command != "echo hi\n(exit 3)" {
		t.Fatalf("result = %+v", result)
	}
	if want := filepath.Join(svc.dataDir, "command-runs", "session", terminal.HandleID+".log"); result.LogPath != want {
		t.Fatalf("log = %q, want %q", result.LogPath, want)
	}
	if _, err := os.Stat(doneFile); !os.IsNotExist(err) {
		t.Fatalf("completion file left behind: %v", err)
	}
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
