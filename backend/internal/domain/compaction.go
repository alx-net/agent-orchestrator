package domain

import "strconv"

// The Claude Code environment variables that decide when a conversation
// compacts on its own.
const (
	ClaudeAutoCompactWindowEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	ClaudeAutoCompactPctEnv    = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"
)

// The "Compact agent conversations at" setting: the token count at which the
// Claude Code agents AO launches compact. CompactAtRepositoryDefault (0) leaves
// it to the repository's and the user's own Claude Code settings.
const (
	DefaultCompactAt           = 350_000
	CompactAtRepositoryDefault = 0
	// MinCompactAt and MaxCompactAt keep the window ClaudeCompactAtEnv sets
	// inside the 100k-1M range Claude Code honors without clamping.
	MinCompactAt = 100_000
	MaxCompactAt = 900_000
)

// ValidCompactAt reports whether tokens is a compaction size AO can set.
func ValidCompactAt(tokens int) bool {
	return tokens == CompactAtRepositoryDefault || (tokens >= MinCompactAt && tokens <= MaxCompactAt)
}

// claudeCompactReserve is what Claude Code keeps free below its compaction
// window: min(max output tokens, 20000) for the reply plus a 13000 buffer.
const claudeCompactReserve = 20_000 + 13_000

// ClaudeCompactAtEnv is the environment that makes Claude Code compact a
// conversation once its context reaches tokens.
//
// Claude Code (checked against 2.1.312) compacts at
// min(window, context) - 33000, where window is CLAUDE_CODE_AUTO_COMPACT_WINDOW.
// CLAUDE_AUTOCOMPACT_PCT_OVERRIDE lowers that to a percentage of the window less
// 20000; 100 is the one value that leaves it unchanged. So a window 33000 above
// tokens and a percentage of 100 compact at exactly tokens on a model whose
// context holds the window. A model with a smaller context compacts where
// Claude Code would for its full context. The model's context window is not
// changed.
func ClaudeCompactAtEnv(tokens int) map[string]string {
	return map[string]string{
		ClaudeAutoCompactWindowEnv: strconv.Itoa(tokens + claudeCompactReserve),
		ClaudeAutoCompactPctEnv:    "100",
	}
}
