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

func TestCommandOutputIsTheTextBetweenItsMarkers(t *testing.T) {
	start, end := sessionCommandMarker("start", "h1"), sessionCommandMarker("end", "h1")
	raw := "banner\r\n➜ repo . '/data/h1.run'\r\n$ terraform plan\r\n" + start + "No changes.\r\n" + end + "\r\n➜ repo \r\n"
	if got := commandOutput(raw, "h1"); got != "No changes." {
		t.Fatalf("output = %q", got)
	}
	// Output with no final newline keeps its last line.
	if got := commandOutput(start+"no-newline"+end+"\r\n", "h1"); got != "no-newline" {
		t.Fatalf("unterminated output = %q", got)
	}
	// A rolling buffer that dropped the start keeps what is left.
	if got := commandOutput("line 900\r\nline 901\r\n"+end+"\r\n", "h1"); got != "line 900\nline 901" {
		t.Fatalf("scrolled output = %q", got)
	}
	// A run that never ended keeps everything after its start.
	if got := commandOutput(start+"so far\r\n", "h1"); got != "so far" {
		t.Fatalf("unfinished output = %q", got)
	}
}

// What the agent reads must be the screen's final state, not every frame drawn.
func TestPlainTerminalTextCollapsesRedraws(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{
			name: "alternate screen is left out",
			raw:  "before\r\n\x1b[?1049h\x1b[2J\x1b[1;1HFRAME1\x1b[1;1HFRAME2\x1b[?1049lafter\r\n",
			want: "before\n" + fullScreenNote + "\nafter\n",
		},
		{
			name: "alternate screen left open when the program was killed",
			raw:  "before\r\n\x1b[?1049h\x1b[2JFRAME",
			want: "before\n" + fullScreenNote + "\n",
		},
		{
			name: "inline TUI that erases and redraws its lines",
			raw:  "kept\r\nframe 1 a\r\nframe 1 b\r\n\x1b[2K\x1b[1A\x1b[2K\x1b[1A\x1b[2K\x1b[Gframe 2 a\r\nframe 2 b\r\n",
			want: "kept\nframe 2 a\nframe 2 b\n",
		},
		{
			name: "cursor up over lines that are overwritten",
			raw:  "one\r\ntwo\r\n\x1b[2A\rONE\x1b[K\r\nTWO\x1b[K\r\n",
			want: "ONE\nTWO\n",
		},
		{
			name: "shorter redraw erases the rest of the line",
			raw:  "downloading 100%\r\x1b[Kdone\r\n",
			want: "done\n",
		},
		{
			name: "clear screen starts over",
			raw:  "old\r\n\x1b[H\x1b[2Jnew\r\n",
			want: "new\n",
		},
		{
			name: "hyperlinks and titles print nothing",
			raw:  "\x1b]8;;http://x\x07link\x1b]8;;\x07\r\n",
			want: "link\n",
		},
		{
			name: "stray moves on an empty screen are harmless",
			raw:  "\x1b[5A\x1b[9B\x1b[K\x1b[J\x1b[999999Cx",
			want: strings.Repeat(" ", textScreenMaxCol) + "x",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := plainTerminalText(tc.raw); got != tc.want {
				t.Fatalf("plain = %q, want %q", got, tc.want)
			}
		})
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
