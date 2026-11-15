package claudeacp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// compactionSettings returns the settings option session/new would carry.
func compactionSettings(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()
	settings, _ := claudeNestedMap(claudeNestedMap(meta, "claudeCode"), "options")["settings"].(map[string]any)
	return settings
}

func TestSessionMetaCompactsAtTheSettingThroughFlagSettings(t *testing.T) {
	driver := &checkpointDriver{}
	driver.SetCompactAt(func() int { return 350_000 })
	meta := driver.sessionMeta(acpdriver.LaunchConfig{SystemPrompt: "AO standing instructions"})

	settings := compactionSettings(t, meta)
	want := map[string]string{
		// 350000 + min(max output, 20000) + Claude Code's 13000 buffer.
		domain.ClaudeAutoCompactWindowEnv: "383000",
		// 100 cancels a repository's percentage override, so it cannot lower the threshold.
		domain.ClaudeAutoCompactPctEnv: "100",
	}
	if !reflect.DeepEqual(settings["env"], want) {
		t.Fatalf("settings env = %#v, want %#v", settings["env"], want)
	}
	if prompt := claudeNestedMap(meta, "systemPrompt"); !strings.HasPrefix(prompt["append"].(string), "AO standing instructions") {
		t.Fatalf("compaction settings replaced the standing instructions: %#v", prompt)
	}
	options := claudeNestedMap(claudeNestedMap(meta, "claudeCode"), "options")
	if _, ok := options["env"]; ok {
		t.Fatalf("compaction still rides the process env, which a project's settings override: %#v", options)
	}
	// The SDK serializes the option as --settings JSON.
	if _, err := json.Marshal(settings); err != nil {
		t.Fatalf("settings do not serialize: %v", err)
	}
}

func TestSessionMetaGuidesTheAutomaticSummary(t *testing.T) {
	driver := &checkpointDriver{}
	driver.SetCompactAt(func() int { return 200_000 })
	settings := compactionSettings(t, driver.sessionMeta(acpdriver.LaunchConfig{}))

	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks["PreCompact"].([]any)
	if len(groups) != 1 {
		t.Fatalf("PreCompact hooks = %#v, want one group", hooks)
	}
	group := groups[0].(map[string]any)
	if group["matcher"] != "auto" {
		t.Fatalf("matcher = %v; a manual /compact keeps only the user's own instructions", group["matcher"])
	}
	hook := group["hooks"].([]any)[0].(map[string]any)
	if hook["type"] != "command" || hook["command"] != "echo '"+claudeCompactGuidance+"'" {
		t.Fatalf("hook = %#v", hook)
	}
	for _, keep := range []string{"task goal", "pull request", "decision", "verbatim", "files being edited", "next steps"} {
		if !strings.Contains(claudeCompactGuidance, keep) {
			t.Errorf("guidance does not ask to keep %q", keep)
		}
	}
	if strings.ContainsAny(claudeCompactGuidance, "'\\\n\r`$") {
		t.Fatalf("guidance %q cannot be echoed inside single quotes in every shell", claudeCompactGuidance)
	}
}

func TestSessionMetaKeepsAnExplicitCompactionChoice(t *testing.T) {
	driver := &checkpointDriver{}
	driver.SetCompactAt(func() int { return 350_000 })
	for name, env := range map[string]map[string]string{
		"conversation window":  {domain.ClaudeAutoCompactWindowEnv: "200000"},
		"project percent only": {domain.ClaudeAutoCompactPctEnv: "60"},
	} {
		t.Run(name, func(t *testing.T) {
			settings := compactionSettings(t, driver.sessionMeta(acpdriver.LaunchConfig{Env: env}))
			if !reflect.DeepEqual(settings["env"], env) {
				t.Fatalf("settings env = %#v, want the launch's own %#v", settings["env"], env)
			}
			if settings["hooks"] == nil {
				t.Fatal("an AO-set compaction lost its summary guidance")
			}
		})
	}
}

func TestSessionMetaLeavesRepositoryDefaultAlone(t *testing.T) {
	for name, driver := range map[string]*checkpointDriver{"unwired": {}, "repository default": {}} {
		if name == "repository default" {
			driver.SetCompactAt(func() int { return domain.CompactAtRepositoryDefault })
		}
		t.Run(name, func(t *testing.T) {
			meta := driver.sessionMeta(acpdriver.LaunchConfig{SystemPrompt: "AO standing instructions"})
			if settings := compactionSettings(t, meta); settings != nil {
				t.Fatalf("settings = %#v, want none so the repository's own settings apply", settings)
			}
		})
	}
}

func TestSessionMetaReadsTheSettingAtEachSession(t *testing.T) {
	tokens := 350_000
	driver := &checkpointDriver{}
	driver.SetCompactAt(func() int { return tokens })
	first := compactionSettings(t, driver.sessionMeta(acpdriver.LaunchConfig{}))["env"].(map[string]string)
	tokens = 500_000
	second := compactionSettings(t, driver.sessionMeta(acpdriver.LaunchConfig{}))["env"].(map[string]string)
	if first[domain.ClaudeAutoCompactWindowEnv] != "383000" || second[domain.ClaudeAutoCompactWindowEnv] != "533000" {
		t.Fatalf("windows = %q then %q, want 383000 then 533000", first[domain.ClaudeAutoCompactWindowEnv], second[domain.ClaudeAutoCompactWindowEnv])
	}
}

func TestNewDriverAcceptsTheCompactAtWiring(t *testing.T) {
	if _, ok := New(reconnectPlugin{}, nil, nil).(interface{ SetCompactAt(func() int) }); !ok {
		t.Fatal("the Claude driver no longer offers SetCompactAt, so the daemon cannot wire the setting")
	}
}
