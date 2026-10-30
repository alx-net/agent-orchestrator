package shellterm

import (
	"strings"
	"testing"
	"time"
)

func TestPlainTerminalTextShowsWhatTheReaderSaw(t *testing.T) {
	raw := "\x1b[32mPlan:\x1b[0m 1 to add\r\n\x1b]0;title\x07loading 10%\rloading 100%\r\nab\bc\r\n"
	if got, want := plainTerminalText(raw), "Plan: 1 to add\nloading 100%\nac\n"; got != want {
		t.Fatalf("plain = %q, want %q", got, want)
	}
}

func TestCommandOutputDropsStartupAndEcho(t *testing.T) {
	before := "banner\r\n"
	after := before + "➜ repo git:(main) terraform plan\r\nNo changes.\r\n"
	if got := commandOutput(before, after, "terraform plan"); got != "No changes." {
		t.Fatalf("output = %q", got)
	}
	// When the rolling buffer dropped the startup lines, keep everything.
	if got := commandOutput("gone\n", "No changes.\n", "terraform plan"); got != "No changes." {
		t.Fatalf("output = %q", got)
	}
}

func TestAgentMessageKeepsTheTailInsideASafeFence(t *testing.T) {
	var lines []string
	for i := range 100 {
		lines = append(lines, "line "+strings.Repeat("x", i%3))
	}
	lines = append(lines, "```", "Plan: 3 to add")
	msg := SessionCommandResult{
		Command: "scripts/plan", Finished: true, ExitCode: 0, Duration: 74 * time.Second,
		Output: strings.Join(lines, "\n"), LogPath: "/data/command-runs/s/h.log",
	}.AgentMessage()
	for _, want := range []string{
		"exited with code 0 after 1m14s",
		"```sh\nscripts/plan\n```",
		"Last 60 lines of output (full output: `/data/command-runs/s/h.log`)",
		"````text\n",
		"Plan: 3 to add\n````",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message lacks %q:\n%s", want, msg)
		}
	}
	closed := SessionCommandResult{Command: "ls", Duration: 2 * time.Second}.AgentMessage()
	if !strings.Contains(closed, "terminal closed after 2s, before the command finished") || !strings.Contains(closed, "no output") {
		t.Fatalf("closed message:\n%s", closed)
	}
}
