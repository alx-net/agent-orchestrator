package domain

import "testing"

func TestClaudeCompactAtEnvSetsTheThresholdNotTheContext(t *testing.T) {
	for tokens, window := range map[int]string{200_000: "233000", 350_000: "383000", 500_000: "533000"} {
		env := ClaudeCompactAtEnv(tokens)
		if env[ClaudeAutoCompactWindowEnv] != window || env[ClaudeAutoCompactPctEnv] != "100" || len(env) != 2 {
			t.Errorf("ClaudeCompactAtEnv(%d) = %#v, want window %s and percent 100", tokens, env, window)
		}
	}
}

func TestValidCompactAtStaysInsideClaudeCodesUnclampedWindow(t *testing.T) {
	for tokens, want := range map[int]bool{
		CompactAtRepositoryDefault: true, MinCompactAt: true, DefaultCompactAt: true, MaxCompactAt: true,
		-1: false, MinCompactAt - 1: false, MaxCompactAt + 1: false,
	} {
		if got := ValidCompactAt(tokens); got != want {
			t.Errorf("ValidCompactAt(%d) = %v, want %v", tokens, got, want)
		}
	}
	// Claude Code clamps CLAUDE_CODE_AUTO_COMPACT_WINDOW to 100000-1000000.
	if window := MaxCompactAt + claudeCompactReserve; window > 1_000_000 {
		t.Fatalf("largest window %d exceeds Claude Code's 1M maximum", window)
	}
}
