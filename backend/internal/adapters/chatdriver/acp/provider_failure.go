package acp

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Provider failures arrive in two shapes: a JSON-RPC error answering a request,
// and claude-agent-acp's AIR session failure on an end_turn prompt response.
// Claude Code rewords its messages from release to release, and each new
// wording used to fall through to a generic failure. So the structured kinds
// decide here, and today's text rules are only the fallback.

type failureClass string

const (
	// failureUnclassified is the generic bucket: no kind AO acts on, and no
	// text rule matched. A new error shape lands here, so it is logged.
	failureUnclassified failureClass = ""
	// failureAuthRequired raises the sign-in banner.
	failureAuthRequired failureClass = "auth_required"
	// failureUsageLimit is a quota refusal: another account's quota lifts it.
	failureUsageLimit failureClass = "usage_limit"
	// failureRateLimited is transient throttling: a later retry lifts it.
	failureRateLimited failureClass = "rate_limited"
	failureOverloaded  failureClass = "overloaded"
	failureBadRequest  failureClass = "bad_request"
	// failureTurnLimit is a limit of the turn itself: budget, turn count,
	// context or output size.
	failureTurnLimit  failureClass = "turn_limit"
	failureConnection failureClass = "connection"
)

// errorKindClasses maps the errorKind claude-agent-acp puts in JSON-RPC error
// data: the Claude Agent SDK's SDKAssistantMessageError values plus the
// bridge's own kinds. server_error, unknown and no_result stay unclassified.
var errorKindClasses = map[string]failureClass{
	"authentication_failed": failureAuthRequired,
	"oauth_org_not_allowed": failureAuthRequired,
	"billing_error":         failureUsageLimit,
	// The SDK also tags a usage limit's 429 rate_limit; the text fallback
	// below tells that one apart.
	"rate_limit":        failureRateLimited,
	"overloaded":        failureOverloaded,
	"invalid_request":   failureBadRequest,
	"model_not_found":   failureBadRequest,
	"max_output_tokens": failureTurnLimit,
	"transport_lost":    failureConnection,
	"worker_shutdown":   failureConnection,
}

// airKindClasses maps claude-agent-acp's AIR failure kinds. The pinned bridge
// leaves the kind out of the wire; AO's packaging patch forwards it.
// internal_error, provider_error and advisory stay unclassified.
var airKindClasses = map[string]failureClass{
	"auth_required":     failureAuthRequired,
	"quota_exhausted":   failureUsageLimit,
	"rate_limited":      failureRateLimited,
	"overloaded":        failureOverloaded,
	"bad_request":       failureBadRequest,
	"budget_exhausted":  failureTurnLimit,
	"context_exhausted": failureTurnLimit,
	"transport_lost":    failureConnection,
	"worker_shutdown":   failureConnection,
}

// apiErrorClasses maps Claude Code's typed api_error, which the SDK message
// carries and AO's bridge patch forwards. It is the one signal that tells a
// usage limit from the other refusals sharing errorKind rate_limit (a server
// throttle, an organization policy, a capacity switch-off). Other values are
// left to the kind.
var apiErrorClasses = map[string]failureClass{
	"usage_limit_reached":          failureUsageLimit,
	"model_requires_usage_credits": failureUsageLimit,
}

type providerFailure struct {
	class     failureClass
	code      int    // JSON-RPC code; 0 for an AIR failure
	errorKind string // SDK assistant error or bridge errorKind
	kind      string // AIR failure kind
	apiError  string // Claude Code's typed api_error
	category  string // AIR failure category
	title     string
	detail    string
}

// classifyRequestError reads a JSON-RPC error from the agent. ok is false when
// err is not one.
func classifyRequestError(err error) (providerFailure, bool) {
	var requestErr *acpsdk.RequestError
	if !errors.As(err, &requestErr) {
		return providerFailure{}, false
	}
	f := providerFailure{
		code:      requestErr.Code,
		errorKind: acpErrorField(requestErr.Data, "errorKind"),
		title:     requestErrorTitle(requestErr),
		detail:    acpErrorField(requestErr.Data, "details"),
	}
	if requestErr.Code == -32000 {
		f.class = failureAuthRequired
	} else {
		f.class = errorKindClasses[f.errorKind]
	}
	// Only the agent's own failure (-32603) carries provider text worth
	// reading. Auth is never inferred from text: a wrong sign-in banner is its
	// own bug.
	if requestErr.Code == -32603 && f.class != failureAuthRequired && f.class != failureUsageLimit &&
		(isUsageLimitText(f.title) || isUsageLimitText(f.detail)) {
		f.class = failureUsageLimit
	}
	return f, true
}

// requestErrorTitle is the provider's text in an ACP SDK internal error, whose
// message the SDK prefixes with "Internal error".
func requestErrorTitle(requestErr *acpsdk.RequestError) string {
	message := strings.TrimSpace(requestErr.Message)
	if requestErr.Code != -32603 {
		return message
	}
	if message == "Internal error" {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(message, "Internal error:"))
}

// classifySessionFailure reads an AIR session failure.
func classifySessionFailure(failure map[string]any) providerFailure {
	f := providerFailure{
		kind:      stringField(failure, "kind"),
		errorKind: stringField(failure, "errorKind"),
		apiError:  stringField(failure, "apiError"),
		category:  stringField(failure, "category"),
		title:     stringField(failure, "title"),
		detail:    stringField(failure, "details"),
	}
	var actions []string
	if list, ok := failure["actions"].([]any); ok {
		for _, action := range list {
			if name, ok := action.(string); ok {
				actions = append(actions, name)
			}
		}
	}
	switch {
	case slices.Contains(actions, "login"):
		// The action is what an AIR client acts on, whatever the kind.
		f.class = failureAuthRequired
	case apiErrorClasses[f.apiError] != failureUnclassified:
		f.class = apiErrorClasses[f.apiError]
	case f.kind != "":
		f.class = airKindClasses[f.kind]
	default:
		f.class = sessionFailurePolicyClass(f.category, actions)
	}
	if f.class != failureAuthRequired && f.class != failureUsageLimit && isUsageLimitText(f.title) {
		f.class = failureUsageLimit
	}
	return f
}

// sessionFailurePolicyClass recovers the kind of an error-severity failure
// from a bridge that does not forward it. claude-agent-acp's policy table gives
// each kind a category and recovery actions, and the pair tells apart every
// kind AO acts on: quota_exhausted is the only limit with no action, and a
// rate limit is the only one offering a retry. Overloaded and provider_error
// share service+retry, so service stays unclassified. Access without a login
// action (an account without Claude Code) is not a sign-in demand either.
func sessionFailurePolicyClass(category string, actions []string) failureClass {
	switch category {
	case "limit":
		switch {
		case len(actions) == 0:
			return failureUsageLimit
		case slices.Contains(actions, "retry"):
			return failureRateLimited
		case slices.Contains(actions, "new_session"):
			return failureTurnLimit
		}
	case "connection":
		return failureConnection
	case "request":
		return failureBadRequest
	}
	return failureUnclassified
}

// promptSessionFailure is the terminal AIR failure claude-agent-acp attaches to
// an end_turn prompt response. Warnings are progress notes, not failures, and
// carry no actions, so they never reach the policy inference.
func promptSessionFailure(response acpsdk.PromptResponse) (providerFailure, bool) {
	if response.StopReason != acpsdk.StopReasonEndTurn {
		return providerFailure{}, false
	}
	failure := sessionFailure(response.Meta)
	if failure == nil || failure["severity"] != "error" {
		return providerFailure{}, false
	}
	return classifySessionFailure(failure), true
}

// SessionFailurePromptError is the turn failure an AIR session failure on a
// prompt response reports, or nil when the response carries none. It marks a
// sign-in demand with ports.ErrChatAuthRequired and a quota refusal with
// ports.ErrChatUsageLimit.
func SessionFailurePromptError(response acpsdk.PromptResponse) error {
	f, ok := promptSessionFailure(response)
	if !ok {
		return nil
	}
	switch f.class {
	case failureAuthRequired:
		return ports.NewChatProviderFailure(f.title, f.detail, ports.ErrChatAuthRequired)
	case failureUsageLimit:
		return ports.NewChatUsageLimitFailure(f.title, f.detail, nil)
	}
	return ports.NewChatProviderFailure(f.title, f.detail, nil)
}

// isACPAuthRequired reports whether err is the agent telling us the credential
// was refused, so the caller can raise a reauth prompt instead of a generic
// turn failure. ACP's code for it is -32000; claude-agent-acp reports a login
// that expired mid-session as an internal error (-32603) tagged
// errorKind "authentication_failed" instead.
func isACPAuthRequired(err error) bool {
	f, ok := classifyRequestError(err)
	return ok && f.class == failureAuthRequired
}

// normalizeACPPromptError is normalizeACPError for a failed session/prompt,
// which also marks a quota refusal for the account switch.
func normalizeACPPromptError(err error) error {
	if f, ok := classifyRequestError(err); ok && f.class == failureUsageLimit {
		return ports.NewChatUsageLimitFailure(f.title, f.detail, err)
	}
	return normalizeACPError("ACP session/prompt", err)
}

// accountRefusedModelCheck reports a model change the agent could not check
// because its account cannot make API calls now: at its usage limit, rate
// limited or signed out. The model itself is fine.
func accountRefusedModelCheck(err error) bool {
	f, ok := classifyRequestError(err)
	if !ok {
		return false
	}
	switch f.class {
	case failureAuthRequired, failureUsageLimit, failureRateLimited:
		return true
	}
	// Claude Code's model check throws a plain error, which the ACP SDK sends
	// as -32603 with only its text in details.
	details := strings.ToLower(f.detail)
	if !strings.Contains(details, "model") {
		return false
	}
	for _, refusal := range []string{"api error: 429", "rate limit", "oauth", "/login"} {
		if strings.Contains(details, refusal) {
			return true
		}
	}
	return false
}

func isUsageLimitText(text string) bool {
	return strings.TrimSpace(text) != "" && domain.ParseConversationUsageLimit(text, nil) != nil
}

// logText is the provider text an unclassified failure is logged with,
// cut short: enough to recognize the shape, not a transcript.
func (f providerFailure) logText() string {
	text := f.title
	if f.detail != "" && f.detail != f.title {
		if text != "" {
			text += " | "
		}
		text += f.detail
	}
	const limit = 300
	if runes := []rune(text); len(runes) > limit {
		text = string(runes[:limit]) + "…"
	}
	return text
}

// acpErrorField is a string field of a JSON-RPC error's data.
func acpErrorField(data any, name string) string {
	var fields map[string]any
	switch value := data.(type) {
	case map[string]any:
		fields = value
	case json.RawMessage:
		_ = json.Unmarshal(value, &fields)
	case []byte:
		_ = json.Unmarshal(value, &fields)
	}
	return stringField(fields, name)
}

func stringField(fields map[string]any, name string) string {
	value, _ := fields[name].(string)
	return strings.TrimSpace(value)
}
