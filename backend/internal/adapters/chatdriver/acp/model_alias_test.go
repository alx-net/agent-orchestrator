package acp

import (
	"context"
	"io"
	"log/slog"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Claude Code lists the "fable" alias as claude-fable-5-2 once the session runs
// that model. A conversation saved with the alias compared unequal to the
// current model on every send and asked for a change each time, which Claude
// Code checks against the API and an account at its limit refuses.
func TestACPSendTurnSendsNoModelChangeForAnAliasOfTheCurrentModel(t *testing.T) {
	catalog := func(current string) []acpsdk.SessionConfigOption {
		return []acpsdk.SessionConfigOption{selectConfigOption("model", "Model", "model", current,
			"default", "opus", "claude-fable-5-2", "claude-fable-5-1")}
	}
	tests := []struct {
		name      string
		current   string
		alias     string
		wantSet   string
		wantModel string
	}{
		{name: "alias of the current model", current: "claude-fable-5-2", alias: "claude-fable-5-2", wantModel: "claude-fable-5-2"},
		// AO's answer is only trusted to skip a change; a real switch is the agent's
		// to resolve, as before.
		{name: "alias of another model", current: "opus", alias: "claude-fable-5-2", wantSet: "fable"},
		{name: "no single choice", current: "claude-fable-5-2", wantSet: "fable"},
		{name: "a choice not offered", current: "claude-fable-5-2", alias: "claude-fable-9", wantSet: "fable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &fakeAgent{newConfig: catalog(tt.current), setConfig: catalog(tt.alias)}
			driver := New(Config{
				Harness: domain.HarnessClaudeCode,
				Probe:   func(context.Context) error { return nil },
				Launch:  func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
				SessionOptions: func(settings ports.ChatTurnSettings) []SessionOption {
					if settings.Model == "" {
						return nil
					}
					return []SessionOption{{ID: "model", Value: settings.Model}}
				},
				ModelAlias: func(model string, _ []ports.ChatConfigOptionChoice) string {
					if model != "fable" {
						t.Errorf("ModelAlias(%q), want only the unlisted alias", model)
					}
					return tt.alias
				},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			driver.useTestProcess(fakeSpawn(agent))
			opened, err := driver.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer opened.Close()
			_ = nextEvent(t, opened.Events())

			ref, err := opened.(*conversation).SendTurn(context.Background(), ports.ChatUserMessage{
				Text: "hi", Settings: ports.ChatTurnSettings{Model: "fable"},
			})
			if err != nil {
				t.Fatalf("SendTurn: %v", err)
			}
			agent.mu.Lock()
			calls, sent := agent.setCalls, agent.options["model"]
			agent.mu.Unlock()
			if tt.wantSet == "" && calls != 0 {
				t.Fatalf("set_config_option calls = %d (model %q), want none for the current model", calls, sent)
			}
			if tt.wantSet != "" && (calls != 1 || sent != tt.wantSet) {
				t.Fatalf("set_config_option calls = %d with model %q, want one with %q", calls, sent, tt.wantSet)
			}
			if ref.Model != tt.wantModel {
				t.Fatalf("turn model = %q, want %q", ref.Model, tt.wantModel)
			}
		})
	}
}

// Resume applies the saved settings too, so an alias of the running model must
// not be sent there either.
func TestACPResumeSendsNoModelChangeForAnAliasOfTheCurrentModel(t *testing.T) {
	model := selectConfigOption("model", "Model", "model", "claude-fable-5-2", "default", "opus", "claude-fable-5-2")
	agent := &fakeAgent{resumeConfig: []acpsdk.SessionConfigOption{model}}
	driver := New(Config{
		Harness:      domain.HarnessClaudeCode,
		Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true, ports.ChatCapabilityResume: true},
		Probe:        func(context.Context) error { return nil },
		Launch:       func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
		SessionOptions: func(settings ports.ChatTurnSettings) []SessionOption {
			if settings.Model == "" {
				return nil
			}
			return []SessionOption{{ID: "model", Value: settings.Model}}
		},
		ModelAlias: func(string, []ports.ChatConfigOptionChoice) string { return "claude-fable-5-2" },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))
	resumed, err := driver.Resume(context.Background(), ports.ChatResumeConfig{
		SessionID: domain.SessionID("sess1"), WorkspacePath: t.TempDir(),
		ProviderConversationID: "claude-session-1", Model: "fable",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer resumed.Close()
	agent.mu.Lock()
	calls := agent.setCalls
	agent.mu.Unlock()
	if calls != 0 {
		t.Fatalf("set_config_option calls on resume = %d (%v), want none for the current model", calls, agent.options)
	}
}
