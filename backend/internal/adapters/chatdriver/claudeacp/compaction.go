package claudeacp

import (
	"strings"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// SetCompactAt makes the driver read where new and resumed sessions compact
// from compactAt: a token count, or 0 to leave it to the repository's and the
// user's Claude Code settings. The daemon wires the "Compact agent
// conversations at" setting here.
func (d *checkpointDriver) SetCompactAt(compactAt func() int) {
	d.compactAt.Store(&compactAt)
}

func (d *checkpointDriver) compactAtTokens() int {
	if compactAt := d.compactAt.Load(); compactAt != nil && *compactAt != nil {
		return (*compactAt)()
	}
	return domain.CompactAtRepositoryDefault
}

// sessionMeta is claudeSessionMeta plus the settings that decide where the
// session compacts.
func (d *checkpointDriver) sessionMeta(cfg acpdriver.LaunchConfig) map[string]any {
	return withCompactionSettings(claudeSessionMeta(cfg), cfg.Env, d.compactAtTokens())
}

// claudeCompactGuidance is appended to Claude Code's summary instructions when
// AO decides where a session compacts. It names what a long AO session must not
// lose. It is printed with echo in POSIX shells and PowerShell alike, so it
// holds no single quote, backslash, or line break.
const claudeCompactGuidance = "When you summarize, keep: the task goal; every open pull request with its number, " +
	"branch and state; each decision made and why; the latest user instructions, quoted verbatim; the paths of " +
	"the files being edited; and the pending next steps."

// claudeCompactGuidanceHooks run before every automatic compaction. Claude Code
// appends a PreCompact hook's output to its summary instructions.
var claudeCompactGuidanceHooks = map[string]any{
	"PreCompact": []any{map[string]any{
		"matcher": "auto",
		"hooks": []any{map[string]any{
			"type": "command", "command": "echo '" + claudeCompactGuidance + "'", "timeout": 10,
		}},
	}},
}

// claudeCompactionSettings returns the Claude Code settings that make a session
// compact where AO decided, or nil to leave it to Claude Code.
//
// A compaction variable in the launch environment is the conversation's own
// window choice or the project's AO environment; it wins over compactAt. AO
// passes them as settings because Claude Code applies a project's
// .claude/settings.json env over its process environment, while settings passed
// on its command line (--settings) override the project's.
func claudeCompactionSettings(env map[string]string, compactAt int) map[string]any {
	compaction := map[string]string{}
	for _, key := range []string{domain.ClaudeAutoCompactWindowEnv, domain.ClaudeAutoCompactPctEnv} {
		if value := strings.TrimSpace(env[key]); value != "" {
			compaction[key] = value
		}
	}
	if len(compaction) == 0 && compactAt > 0 {
		compaction = domain.ClaudeCompactAtEnv(compactAt)
	}
	if len(compaction) == 0 {
		return nil
	}
	return map[string]any{"env": compaction, "hooks": claudeCompactGuidanceHooks}
}

// withCompactionSettings adds the compaction settings to session/new _meta as
// claude-agent-acp's settings option, which the Agent SDK passes to Claude Code
// as --settings. AO's packaged adapter merges them with its model settings (see
// patchClaudeSessionSettings in frontend/scripts/build-acp-runtime-helpers.mjs).
func withCompactionSettings(meta map[string]any, env map[string]string, compactAt int) map[string]any {
	settings := claudeCompactionSettings(env, compactAt)
	if settings == nil {
		return meta
	}
	if meta == nil {
		meta = map[string]any{}
	}
	claudeCode, _ := meta["claudeCode"].(map[string]any)
	if claudeCode == nil {
		claudeCode = map[string]any{}
		meta["claudeCode"] = claudeCode
	}
	options, _ := claudeCode["options"].(map[string]any)
	if options == nil {
		options = map[string]any{}
		claudeCode["options"] = options
	}
	options["settings"] = settings
	return meta
}
