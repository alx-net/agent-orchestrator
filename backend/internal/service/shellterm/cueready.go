package shellterm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// cueShellReadiness installs a one-shot startup signal. It never sends input
// before the shell itself has reached its first prompt.
type cueShellReadiness struct {
	argv []string
	env  map[string]string
	file string
	// doneFile receives "done <status>" once the first command run after the
	// ready prompt finishes. Empty unless completion was requested and the
	// shell supports it; cleanup leaves it for the caller that waits on it.
	doneFile string
	cleanup  func()
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func powershellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func prepareCueShellReadiness(dataDir string, argv []string, env map[string]string) (cueShellReadiness, error) {
	return prepareShellReadiness(dataDir, argv, env, false)
}

// prepareCommandShellReadiness also reports when the first command finishes,
// with its exit status, for zsh and bash. Other shells get readiness only.
func prepareCommandShellReadiness(dataDir string, argv []string, env map[string]string) (cueShellReadiness, error) {
	return prepareShellReadiness(dataDir, argv, env, true)
}

func prepareShellReadiness(dataDir string, argv []string, env map[string]string, trackDone bool) (cueShellReadiness, error) {
	getenv := func(key string) string {
		if value, ok := env[key]; ok {
			return value
		}
		return os.Getenv(key)
	}
	result := cueShellReadiness{argv: append([]string(nil), argv...), env: map[string]string{}, cleanup: func() {}}
	if len(argv) == 0 {
		return result, nil
	}
	name := strings.ToLower(filepath.Base(argv[0]))
	if runtime.GOOS == "windows" {
		switch name {
		case "bash.exe", "sh.exe", "pwsh.exe", "powershell.exe", "cmd.exe":
		default:
			return result, nil
		}
	} else {
		switch name {
		case "bash", "zsh", "sh", "fish":
		default:
			return result, nil
		}
	}
	dir := filepath.Join(dataDir, "cue-readiness")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return result, fmt.Errorf("prepare cue shell readiness: %w", err)
	}
	f, err := os.CreateTemp(dir, "ready-*")
	if err != nil {
		return result, fmt.Errorf("prepare cue shell readiness: %w", err)
	}
	result.file = f.Name()
	result.env["AO_CUE_READY_FILE"] = result.file
	_ = f.Close()
	result.cleanup = func() { _ = os.Remove(result.file) }
	if trackDone && (name == "zsh" || name == "bash") {
		result.doneFile = result.file + ".done"
	}
	switch name {
	case "fish":
		// Fish evaluates init commands after the user's configuration. Register
		// a one-shot prompt event without replacing their prompt or startup files.
		script := "function _ao_cue_ready --on-event fish_prompt; " +
			"printf ready > \"$AO_CUE_READY_FILE\"; functions --erase _ao_cue_ready; end"
		result.argv = append(result.argv, "--init-command", script)
	case "bash", "bash.exe":
		// Bash runs PROMPT_COMMAND immediately before rendering the first prompt.
		// A profile that replaces it will cause a safe timeout.
		hook := "if [ -z \"$AO_CUE_READY_SENT\" ]; then printf ready > " + shellSingleQuote(result.file) + "; AO_CUE_READY_SENT=1; fi"
		if result.doneFile != "" {
			// Runs before the ready hook, so it skips the first prompt and fires
			// on the prompt after the command AO sends.
			hook = "if [ -n \"$AO_CUE_READY_SENT\" ] && [ -z \"$AO_CUE_DONE_SENT\" ]; then printf 'done %s' \"$__ao_cue_status\" > " +
				shellSingleQuote(result.doneFile) + "; AO_CUE_DONE_SENT=1; fi; " + hook
		}
		if previous := getenv("PROMPT_COMMAND"); previous != "" {
			hook = previous + "; " + hook
		}
		if result.doneFile != "" {
			// Captured first, before any other prompt command changes $?.
			hook = "__ao_cue_status=$?; " + hook
		}
		result.env["PROMPT_COMMAND"] = hook
	case "sh", "sh.exe":
		// An interactive POSIX sh reads ENV before its first prompt.
		profile := result.file + ".env"
		content := ""
		if previous := getenv("ENV"); previous != "" {
			content = ". " + shellSingleQuote(previous) + "\n"
		}
		content += "printf ready > " + shellSingleQuote(result.file) + "\n"
		if err := os.WriteFile(profile, []byte(content), 0o600); err != nil {
			result.cleanup()
			return cueShellReadiness{}, err
		}
		result.env["ENV"] = profile
		result.cleanup = func() { _ = os.Remove(result.file); _ = os.Remove(profile) }
	case "zsh":
		// Source the user's original startup files before installing a one-shot
		// precmd hook. Restore ZDOTDIR for subsequent shells.
		wrapper := result.file + ".zsh"
		if err := os.Mkdir(wrapper, 0o700); err != nil {
			result.cleanup()
			return cueShellReadiness{}, err
		}
		original := getenv("ZDOTDIR")
		if original == "" {
			original = getenv("HOME")
		}
		for _, file := range []string{".zshenv", ".zshrc"} {
			content := "if [[ -f " + shellSingleQuote(filepath.Join(original, file)) + " ]]; then source " + shellSingleQuote(filepath.Join(original, file)) + "; fi\n"
			if file == ".zshenv" {
				content += "export ZDOTDIR=" + shellSingleQuote(wrapper) + "\n"
			}
			if file == ".zshrc" {
				content += "function _ao_cue_ready { print -rn -- ready > " + shellSingleQuote(result.file) + "; precmd_functions=(${precmd_functions:#_ao_cue_ready}); }\n"
				content += "precmd_functions+=(_ao_cue_ready)\n"
				if result.doneFile != "" {
					// Armed by the command's preexec and placed first in precmd, so it
					// records that command's status before other hooks run.
					content += "function _ao_cue_done { local s=$?; print -rn -- \"done $s\" > " + shellSingleQuote(result.doneFile) + "; precmd_functions=(${precmd_functions:#_ao_cue_done}); }\n"
					content += "function _ao_cue_arm { preexec_functions=(${preexec_functions:#_ao_cue_arm}); precmd_functions=(_ao_cue_done $precmd_functions); }\n"
					content += "preexec_functions+=(_ao_cue_arm)\n"
				}
				if getenv("ZDOTDIR") == "" {
					content += "unset ZDOTDIR\n"
				} else {
					content += "export ZDOTDIR=" + shellSingleQuote(getenv("ZDOTDIR")) + "\n"
				}
			}
			if err := os.WriteFile(filepath.Join(wrapper, file), []byte(content), 0o600); err != nil {
				_ = os.RemoveAll(wrapper)
				result.cleanup()
				return cueShellReadiness{}, err
			}
		}
		result.env["ZDOTDIR"] = wrapper
		result.cleanup = func() { _ = os.Remove(result.file); _ = os.RemoveAll(wrapper) }
	case "pwsh.exe", "powershell.exe":
		// -Command runs after the profile; preserve the user's prompt function.
		file := powershellSingleQuote(result.file)
		script := "$global:aoCueOriginalPrompt = (Get-Command prompt -CommandType Function).ScriptBlock; " +
			"function global:prompt { if (-not $global:aoCueReadySent) { " +
			"Set-Content -LiteralPath " + file + " -Value ready -NoNewline; $global:aoCueReadySent = $true }; " +
			"& $global:aoCueOriginalPrompt }"
		result.argv = append(result.argv, "-NoExit", "-Command", script)
	case "cmd.exe":
		// /K runs a tiny script after cmd AutoRun and remains interactive. Keep
		// the redirection inside the script so go-pty need only quote its path.
		script := result.file + ".cmd"
		if err := os.WriteFile(script, []byte("@echo off\r\necho ready>\""+result.file+"\"\r\n"), 0o600); err != nil {
			result.cleanup()
			return cueShellReadiness{}, err
		}
		result.argv = append(result.argv, "/K", script)
		result.cleanup = func() { _ = os.Remove(result.file); _ = os.Remove(script) }
	}
	return result, nil
}

var cueShellReadyTimeout = 10 * time.Second

func (s *Service) waitForCueShellReady(ctx context.Context, handle ports.RuntimeHandle, file string) error {
	if file == "" {
		return apierr.Conflict("CUE_SHELL_NOT_READY", "AO cannot verify that this shell is ready; the terminal remains open for manual use", nil)
	}
	waitCtx, cancel := context.WithTimeout(ctx, cueShellReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(initialInputPollInterval)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(file); err == nil && strings.TrimSpace(string(data)) == "ready" {
			return nil
		}
		alive, err := s.runtime.IsChildAlive(waitCtx, handle)
		if err == nil && !alive {
			return apierr.Conflict("CUE_SHELL_NOT_READY", "The shell exited before it was ready; the terminal remains open for inspection", nil)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waitCtx.Done():
			// Parent cancellation also closes waitCtx; preserve the caller's error.
			if err := ctx.Err(); err != nil {
				return err
			}
			return apierr.Conflict("CUE_SHELL_NOT_READY", "The shell did not become ready; the terminal remains open for manual use", nil)
		case <-ticker.C:
		}
	}
}
