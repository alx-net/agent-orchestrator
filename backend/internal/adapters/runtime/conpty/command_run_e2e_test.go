//go:build darwin || linux

package conpty

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

// These run the Run button's whole path for real: a detached pty-host, a real
// shell in it, and the service typing the command and reading the result.

type commandRunStore struct{ shellterm.Store }

func (commandRunStore) InsertShellTerminal(context.Context, shellterm.ShellTerminalRecord) error {
	return nil
}

type commandRunProjects struct{}

func (commandRunProjects) ProjectRoot(context.Context, domain.ProjectID) (string, error) {
	return "", nil
}

func (commandRunProjects) ProjectEnv(context.Context, domain.ProjectID) (map[string]string, error) {
	return nil, nil
}

type commandRunSessions struct{ workspace string }

func (s commandRunSessions) SessionWorkspace(context.Context, domain.SessionID) (string, domain.ProjectID, error) {
	return s.workspace, "project", nil
}

func (s commandRunSessions) CueCommandSessionTarget(context.Context, domain.SessionID) (shellterm.CueCommandSessionTarget, error) {
	return shellterm.CueCommandSessionTarget{ProjectID: "project", WorkspacePath: s.workspace}, nil
}

// commandRunBench is one daemon's view of the world. A restart is a second bench
// over the same data dir: the pty-hosts and their shells keep running.
type commandRunBench struct {
	runtime   *Runtime
	service   *shellterm.Service
	workspace string
	dataDir   string
}

func newCommandRunBench(t *testing.T, workspace, dataDir string) *commandRunBench {
	t.Helper()
	runtime := New(Options{Spawner: defaultSpawnHost})
	service := shellterm.NewService(runtime, commandRunStore{}, commandRunProjects{}, commandRunSessions{workspace}, dataDir, "run", nil)
	return &commandRunBench{runtime: runtime, service: service, workspace: workspace, dataDir: dataDir}
}

// startCommandRunShell isolates HOME and selects the shell the Run tab launches.
func startCommandRunShell(t *testing.T, shell, startupFile, startup string) *commandRunBench {
	t.Helper()
	path, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("%s unavailable: %v", shell, err)
	}
	isolateRegistry(t)
	t.Setenv("SHELL", path)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("BASH_SILENCE_DEPRECATION_WARNING", "1")
	t.Setenv("PROMPT_COMMAND", "")
	if startupFile != "" {
		if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), startupFile), []byte(startup), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return newCommandRunBench(t, t.TempDir(), t.TempDir())
}

func (b *commandRunBench) start(t *testing.T, command string, onDone func(shellterm.SessionCommandResult) error) ports.RuntimeHandle {
	t.Helper()
	terminal, err := b.service.RunSessionCommand(context.Background(), shellterm.RunSessionCommandInput{
		SessionID: "session", Command: command, OnDone: onDone,
	})
	if err != nil {
		t.Fatalf("run %q: %v", command, err)
	}
	handle := ports.RuntimeHandle{ID: terminal.HandleID}
	t.Cleanup(func() { _ = b.runtime.Destroy(context.Background(), handle) })
	return handle
}

func awaitCommandResult(t *testing.T, results <-chan shellterm.SessionCommandResult) shellterm.SessionCommandResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(20 * time.Second):
		t.Fatal("no result reached the agent")
		return shellterm.SessionCommandResult{}
	}
}

// awaitOutput waits for text in the buffer, which holds only whole lines.
func (b *commandRunBench) awaitOutput(t *testing.T, handle ports.RuntimeHandle, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		output, err := b.runtime.GetOutput(context.Background(), handle, 1000)
		if err == nil && strings.Contains(output, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("output lacks %q: %q (err: %v)", want, output, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// typeInTab proves the shell survived with a usable state: a later command in
// the same tab must run, and with `set -u` left on, expanding an unset variable
// would stop it.
func (b *commandRunBench) typeInTab(t *testing.T, handle ports.RuntimeHandle, line, marker string) string {
	t.Helper()
	path := filepath.Join(b.workspace, marker)
	if err := b.runtime.SendMessage(context.Background(), handle, line+` >| `+path); err != nil {
		t.Fatalf("type in tab: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
		if time.Now().After(deadline) {
			output, _ := b.runtime.GetOutput(context.Background(), handle, 1000)
			t.Fatalf("the tab did not run a later command: %q", output)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// heavyZshrc does what plugin managers do to the things AO hooks into.
const heavyZshrc = `setopt noclobber prompt_subst hist_ignore_space auto_pushd
autoload -Uz add-zsh-hook
_plugin_preexec() { print -n "\e]0;title\a"; }
_plugin_precmd() { return 7; }
add-zsh-hook preexec _plugin_preexec
add-zsh-hook precmd _plugin_precmd
zle-line-init() { sleep 0.05; }
zle -N zle-line-init
_plugin_async() { read -r -u "$1" _; zle -F "$1"; exec {1}<&-; }
exec {_plugin_fd}< <(sleep 0.1; echo later)
zle -F "$_plugin_fd" _plugin_async
PROMPT='%~ %# '
RPROMPT='%D{%H:%M}'
`

func TestRunButtonReportsEveryShapeOfCommand(t *testing.T) {
	long := strings.Repeat("x", 3000)
	for _, shell := range []struct{ name, startupFile, startup string }{
		{"zsh", ".zshrc", ""},
		{"zsh-plugins", ".zshrc", heavyZshrc},
		{"bash", "", ""},
	} {
		binary := strings.TrimSuffix(shell.name, "-plugins")
		t.Run(shell.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				command func(workspace string) string
				code    int
				output  string
			}{
				{
					name: "a script that fails",
					command: func(workspace string) string {
						script := filepath.Join(workspace, "apply.sh")
						body := "#!/usr/bin/env bash\nset -euo pipefail\n# guarded apply\necho step1\necho step2 >&2\nexit 4\n"
						if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
							t.Fatal(err)
						}
						return "bash " + script
					},
					code: 4, output: "step1\nstep2",
				},
				{
					name: "comments, heredocs, expansions, quotes, and a long line",
					command: func(string) string {
						return "# a comment, with interactive comments off\n" +
							"X=abc; echo \"${X:-d} ${#X} $(echo sub)\"\n" +
							"cat <<EOF\nhello ${X}\n'single' \"double\" \\$literal\nEOF\n" +
							"printf '%s' " + long + " | wc -c | tr -d ' '"
					},
					output: "abc 3 sub\nhello abc\n'single' \"double\" $literal\n3000",
				},
				{
					name:    "set -e stops the command, not the tab",
					command: func(string) string { return "set -euo pipefail\necho before\nfalse\necho after" },
					code:    1, output: "before",
				},
				{
					name:    "exit ends the command, not the tab",
					command: func(string) string { return "echo bye\nexit 3" },
					code:    3, output: "bye",
				},
				{
					name:    "the command's own EXIT trap runs when it ends",
					command: func(string) string { return "trap 'echo cleanup' EXIT\necho body\nexit 2" },
					code:    2, output: "body\ncleanup",
				},
				{
					name:    "output without a final newline",
					command: func(string) string { return "printf no-newline" },
					output:  "no-newline",
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					bench := startCommandRunShell(t, binary, shell.startupFile, shell.startup)
					results := make(chan shellterm.SessionCommandResult, 1)
					handle := bench.start(t, tc.command(bench.workspace), func(result shellterm.SessionCommandResult) error {
						results <- result
						return nil
					})
					result := awaitCommandResult(t, results)
					if !result.Finished || result.ExitCode != tc.code || result.Output != tc.output {
						t.Fatalf("finished=%v code=%d output=%q; want code %d output %q", result.Finished, result.ExitCode, result.Output, tc.code, tc.output)
					}
					if got := bench.typeInTab(t, handle, `echo "alive${AO_RUN_TEST_UNSET}"`, "alive"); got != "alive" {
						t.Fatalf("later command printed %q", got)
					}
				})
			}
		})
	}
}

func TestRunButtonCarriesDirectoryAndExportsIntoTheTab(t *testing.T) {
	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			bench := startCommandRunShell(t, shell, "", "")
			results := make(chan shellterm.SessionCommandResult, 1)
			command := "mkdir sub\ncd sub\nexport AO_RUN_CARRY='two\nlines'\nFOO=local\nset -u"
			handle := bench.start(t, command, func(result shellterm.SessionCommandResult) error {
				results <- result
				return nil
			})
			if result := awaitCommandResult(t, results); !result.Finished || result.ExitCode != 0 {
				t.Fatalf("result = %+v", result)
			}
			got := bench.typeInTab(t, handle, `printf '%s|%s|%s' "${PWD##*/}" "$AO_RUN_CARRY" "${AO_RUN_TEST_UNSET-unset}"`, "carried")
			if got != "sub|two\nlines|unset" {
				t.Fatalf("tab state = %q", got)
			}
		})
	}
}

func TestRunButtonWaitsThroughPromptsAndInterrupts(t *testing.T) {
	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			t.Run("a prompt the user answers", func(t *testing.T) {
				bench := startCommandRunShell(t, shell, "", "")
				results := make(chan shellterm.SessionCommandResult, 1)
				// The quotes keep the wait from matching the command as the tab prints it.
				handle := bench.start(t, "echo wait\"\"ing\nread answer\necho \"got:$answer\"", func(result shellterm.SessionCommandResult) error {
					results <- result
					return nil
				})
				bench.awaitOutput(t, handle, "waiting\r\n")
				select {
				case result := <-results:
					t.Fatalf("reported before the prompt was answered: %+v", result)
				case <-time.After(700 * time.Millisecond):
				}
				if err := bench.runtime.SendInput(context.Background(), handle, "yes\r"); err != nil {
					t.Fatal(err)
				}
				result := awaitCommandResult(t, results)
				if !result.Finished || result.ExitCode != 0 || result.Output != "waiting\nyes\ngot:yes" {
					t.Fatalf("result = %+v", result)
				}
			})
			t.Run("ctrl-c", func(t *testing.T) {
				bench := startCommandRunShell(t, shell, "", "")
				results := make(chan shellterm.SessionCommandResult, 1)
				handle := bench.start(t, "echo beg\"\"in\nsleep 30\necho never", func(result shellterm.SessionCommandResult) error {
					results <- result
					return nil
				})
				bench.awaitOutput(t, handle, "begin\r\n")
				if err := bench.runtime.SendInput(context.Background(), handle, "\x03"); err != nil {
					t.Fatal(err)
				}
				result := awaitCommandResult(t, results)
				if !result.Finished || result.ExitCode == 0 || !strings.Contains(result.Output, "begin") || strings.Contains(result.Output, "\nnever") {
					t.Fatalf("result = %+v", result)
				}
				if got := bench.typeInTab(t, handle, `echo alive`, "alive"); got != "alive" {
					t.Fatalf("later command printed %q", got)
				}
			})
			t.Run("a full-screen program", func(t *testing.T) {
				bench := startCommandRunShell(t, shell, "", "")
				results := make(chan shellterm.SessionCommandResult, 1)
				command := `printf '\033[?1049h\033[2J\033[1;1HFRAME1\033[1;1HFRAME2'; printf '\033[?1049l'; echo after-tui`
				bench.start(t, command, func(result shellterm.SessionCommandResult) error {
					results <- result
					return nil
				})
				result := awaitCommandResult(t, results)
				if !result.Finished || strings.Contains(result.Output, "FRAME") || !strings.HasSuffix(result.Output, "after-tui") {
					t.Fatalf("result = %+v", result)
				}
			})
		})
	}
}

// AO restarts on every update while terminal hosts live on. The first bench's
// watch dies with its context, exactly as it does with the daemon's process.
func TestRunButtonReportsAcrossADaemonRestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		// finishWhileDown has the command end before the next daemon starts.
		finishWhileDown bool
	}{
		{name: "command ends after the restart"},
		{name: "command ends while no daemon is running", finishWhileDown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := startCommandRunShell(t, "zsh", "", "")
			firstCtx, stopFirst := context.WithCancel(context.Background())
			defer stopFirst()
			first.service.ResumeSessionCommands(firstCtx, nil)
			handle := first.start(t, "echo start\"\"ed\nread answer\necho \"finished:$answer\"\nexit 5", func(shellterm.SessionCommandResult) error {
				t.Error("the stopped daemon delivered a result")
				return nil
			})
			first.awaitOutput(t, handle, "started\r\n")
			stopFirst()
			time.Sleep(50 * time.Millisecond)

			if tc.finishWhileDown {
				if err := first.runtime.SendInput(context.Background(), handle, "go\r"); err != nil {
					t.Fatal(err)
				}
				first.awaitOutput(t, handle, "finished:go")
			}

			second := newCommandRunBench(t, first.workspace, first.dataDir)
			results := make(chan shellterm.SessionCommandResult, 1)
			secondCtx, stopSecond := context.WithCancel(context.Background())
			defer stopSecond()
			second.service.ResumeSessionCommands(secondCtx, func(result shellterm.SessionCommandResult) error {
				results <- result
				return nil
			})
			if !tc.finishWhileDown {
				if err := second.runtime.SendInput(context.Background(), handle, "go\r"); err != nil {
					t.Fatal(err)
				}
			}
			result := awaitCommandResult(t, results)
			if !result.Finished || result.ExitCode != 5 || result.Output != "started\ngo\nfinished:go" {
				t.Fatalf("result = %+v", result)
			}
			dir := filepath.Join(first.dataDir, "command-runs", "session")
			deadline := time.Now().Add(2 * time.Second)
			for {
				files, _ := filepath.Glob(filepath.Join(dir, "*"))
				if len(files) == 1 && strings.HasSuffix(files[0], ".log") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("run files left behind: %v", files)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
