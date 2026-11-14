package claudeacp

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// choices builds a model catalog from value/name pairs.
func choices(pairs ...string) []ports.ChatConfigOptionChoice {
	out := make([]ports.ChatConfigOptionChoice, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, ports.ChatConfigOptionChoice{Value: pairs[i], Name: pairs[i+1]})
	}
	return out
}

// The catalog claude-agent-acp 0.70.0 reports with Claude Code 2.1.322 for a
// session running claude-fable-5-2: the "fable" alias row has become that id.
var fableSessionChoices = choices(
	"default", "Default (recommended)",
	"opus", "Opus 5.5",
	"claude-fable-5-2", "Fable 5.2",
	"sonnet", "Sonnet 5.5",
	"haiku", "Haiku 5.5",
	"claude-fable-5-1", "Fable 5.1",
	"claude-haiku-4-5-20251001", "Haiku 4.5",
	"claude-sonnet-5", "Sonnet 5",
	"claude-opus-5", "Opus 5",
	"claude-fable-5", "Fable 5",
	"claude-opus-4-8", "Opus 4.8",
	"claude-opus-4-7", "Opus 4.7",
	"claude-opus-4-6", "Opus 4.6",
	"claude-sonnet-4-6", "Sonnet 4.6",
)

func TestClaudeModelAliasFindsTheFamilysNewestListedID(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		choices []ports.ChatConfigOptionChoice
		want    string
	}{
		{"fable listed by id", "fable", fableSessionChoices, "claude-fable-5-2"},
		{"case and space", " Fable ", fableSessionChoices, "claude-fable-5-2"},
		{"newest of several ids", "opus", choices("claude-opus-4-8", "Opus 4.8", "claude-opus-5-5", "Opus 5.5", "claude-opus-5", "Opus 5"), "claude-opus-5-5"},
		{"no id of that family", "sonnet", choices("default", "Default", "opus", "Opus 5.5"), ""},
		{"context size must match", "opus[1m]", choices("claude-opus-5-5", "Opus 5.5", "claude-opus-5-5[1m]", "Opus 5.5 (1M context)"), "claude-opus-5-5[1m]"},
		{"bare alias skips 1M ids", "opus", choices("claude-opus-5-5[1m]", "Opus 5.5 (1M context)"), ""},
		{"snapshot beside its undated id", "haiku", choices("claude-haiku-4-5", "Haiku 4.5", "claude-haiku-4-5-20251001", "Haiku 4.5"), ""},
		{"not an alias", "claude-fable-5-2", fableSessionChoices, ""},
		{"default is not a family", "default", fableSessionChoices, ""},
		{"opusplan is not a family", "opusplan", fableSessionChoices, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeModelAlias(tt.model, tt.choices); got != tt.want {
				t.Fatalf("claudeModelAlias(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}
