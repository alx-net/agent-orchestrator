package acp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Every errorKind claude-agent-acp can put in JSON-RPC error data: the Claude
// Agent SDK's SDKAssistantMessageError values and the bridge's own kinds.
func TestClassifyRequestErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want failureClass
	}{
		{"authentication_failed", failureAuthRequired},
		{"oauth_org_not_allowed", failureAuthRequired},
		{"billing_error", failureUsageLimit},
		{"rate_limit", failureRateLimited},
		{"overloaded", failureOverloaded},
		{"invalid_request", failureBadRequest},
		{"model_not_found", failureBadRequest},
		{"max_output_tokens", failureTurnLimit},
		{"transport_lost", failureConnection},
		{"worker_shutdown", failureConnection},
		// The generic bucket, and kinds newer Claude Code releases added.
		{"server_error", failureUnclassified},
		{"unknown", failureUnclassified},
		{"no_result", failureUnclassified},
		{"account_on_hold", failureUnclassified},
		{"", failureUnclassified},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			err := &acpsdk.RequestError{Code: -32603, Message: "Internal error", Data: map[string]any{"errorKind": tc.kind}}
			got, ok := classifyRequestError(err)
			if !ok || got.class != tc.want || got.errorKind != tc.kind || got.code != -32603 {
				t.Fatalf("classify = %+v ok=%v, want class %q", got, ok, tc.want)
			}
		})
	}
}

func TestClassifyRequestErrorCodesAndTextFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want failureClass
	}{
		{"acp auth code", &acpsdk.RequestError{Code: -32000, Message: "Authentication required"}, failureAuthRequired},
		{"raw-json data", &acpsdk.RequestError{Code: -32603, Message: "Internal error",
			Data: []byte(`{"errorKind":"billing_error"}`)}, failureUsageLimit},
		// The SDK tags a usage limit's 429 rate_limit too; the known wording
		// still makes it a usage limit.
		{"usage limit tagged rate_limit", &acpsdk.RequestError{Code: -32603,
			Message: "Internal error: You've hit your weekly limit · resets 3:31pm (Asia/Calcutta)",
			Data:    map[string]any{"errorKind": "rate_limit"}}, failureUsageLimit},
		{"usage limit in details", &acpsdk.RequestError{Code: -32603, Message: "Internal error",
			Data: map[string]any{"details": "You've reached your Fable limit."}}, failureUsageLimit},
		{"transient throttle", &acpsdk.RequestError{Code: -32603,
			Message: "Internal error: API Error: Request rejected (429) · this may be a temporary capacity issue.",
			Data:    map[string]any{"errorKind": "rate_limit"}}, failureRateLimited},
		// Auth is never read from text.
		{"auth prose", &acpsdk.RequestError{Code: -32603, Message: "OAuth access token is invalid."}, failureUnclassified},
		// Only the agent's own failure carries provider text.
		{"usage wording on another code", &acpsdk.RequestError{Code: -32602, Message: "You've hit your limit"}, failureUnclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyRequestError(tc.err)
			if !ok || got.class != tc.want {
				t.Fatalf("classify = %+v ok=%v, want %q", got, ok, tc.want)
			}
		})
	}
	if _, ok := classifyRequestError(errors.New("peer disconnected")); ok {
		t.Fatal("a transport error is not a provider failure")
	}
}

func TestIsACPAuthRequiredRecognizesOAuthOrgRefusal(t *testing.T) {
	err := &acpsdk.RequestError{Code: -32603, Message: "Internal error", Data: map[string]any{"errorKind": "oauth_org_not_allowed"}}
	if !isACPAuthRequired(err) {
		t.Fatal("claude-agent-acp maps oauth_org_not_allowed to a sign-in demand; AO must too")
	}
}

// Every AIR kind claude-agent-acp defines, as AO's bridge patch forwards it.
func TestClassifySessionFailureKinds(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want failureClass
	}{
		{"auth_required", failureAuthRequired},
		{"quota_exhausted", failureUsageLimit},
		{"rate_limited", failureRateLimited},
		{"overloaded", failureOverloaded},
		{"bad_request", failureBadRequest},
		{"budget_exhausted", failureTurnLimit},
		{"context_exhausted", failureTurnLimit},
		{"transport_lost", failureConnection},
		{"worker_shutdown", failureConnection},
		{"internal_error", failureUnclassified},
		{"provider_error", failureUnclassified},
		{"advisory", failureUnclassified},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			// The category and actions disagree on purpose: the kind decides.
			got := classifySessionFailure(map[string]any{
				"kind": tc.kind, "category": "service", "title": "Something failed", "actions": []any{},
			})
			if got.class != tc.want {
				t.Fatalf("class = %q, want %q", got.class, tc.want)
			}
		})
	}
}

// The pinned bridge sends no kind; its policy table's category and actions
// recover the kinds AO acts on.
func TestClassifySessionFailureWithoutKind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		category string
		actions  []any
		want     failureClass
	}{
		{"auth_required", "access", []any{"login"}, failureAuthRequired},
		{"quota_exhausted", "limit", []any{}, failureUsageLimit},
		{"rate_limited", "limit", []any{"retry"}, failureRateLimited},
		{"budget or context", "limit", []any{"new_session"}, failureTurnLimit},
		{"transport_lost", "connection", []any{"new_session"}, failureConnection},
		{"bad_request", "request", []any{}, failureBadRequest},
		{"overloaded or provider_error", "service", []any{"retry"}, failureUnclassified},
		{"internal_error", "service", []any{"retry", "new_session"}, failureUnclassified},
		{"access without login", "access", []any{}, failureUnclassified},
		{"future category", "future", []any{}, failureUnclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySessionFailure(map[string]any{
				"category": tc.category, "title": "Something failed", "actions": tc.actions,
			})
			if got.class != tc.want {
				t.Fatalf("class = %q, want %q", got.class, tc.want)
			}
		})
	}
}

func TestClassifySessionFailureAPIErrorAndText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure map[string]any
		want    failureClass
	}{
		// A wording the bundled SDK does not know leaves the bridge's kind at
		// rate_limited; Claude Code's typed api_error still names the usage limit.
		{"usage_limit_reached", map[string]any{"kind": "rate_limited", "apiError": "usage_limit_reached",
			"category": "limit", "actions": []any{"retry"}, "title": "Your plan is paused until Monday."}, failureUsageLimit},
		{"model_requires_usage_credits", map[string]any{"kind": "provider_error", "apiError": "model_requires_usage_credits",
			"category": "service", "actions": []any{"retry"}, "title": "Fable needs usage credits."}, failureUsageLimit},
		{"other api_error", map[string]any{"kind": "rate_limited", "apiError": "network_edge_blocked",
			"category": "limit", "actions": []any{"retry"}, "title": "Blocked"}, failureRateLimited},
		{"login wins", map[string]any{"kind": "quota_exhausted", "apiError": "usage_limit_reached",
			"category": "limit", "actions": []any{"login"}, "title": "Sign in"}, failureAuthRequired},
		// Today's text rule still catches a usage limit the structure missed.
		{"text fallback", map[string]any{"category": "limit", "actions": []any{"retry"},
			"title": "You've hit your weekly limit · resets 3:31pm (Asia/Calcutta)"}, failureUsageLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySessionFailure(tc.failure); got.class != tc.want {
				t.Fatalf("class = %q, want %q", got.class, tc.want)
			}
		})
	}
}

func TestSessionFailurePromptError(t *testing.T) {
	failedAt := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		failure         map[string]any
		auth, usage     bool
		wantMessage     string
		wantResetParsed bool
	}{
		{"known wording keeps its text", map[string]any{"category": "limit", "actions": []any{},
			"title": "You've hit your session limit · resets 3:31pm (Asia/Calcutta)"},
			false, true, "You've hit your session limit · resets 3:31pm (Asia/Calcutta)", true},
		{"new wording is titled for the store", map[string]any{"kind": "quota_exhausted", "category": "limit", "actions": []any{},
			"title": "Your seat type doesn't include usage · resets 3:31pm (Asia/Calcutta)"},
			false, true, "Usage limit reached\n\nYour seat type doesn't include usage · resets 3:31pm (Asia/Calcutta)", true},
		{"sign in", map[string]any{"category": "access", "actions": []any{"login"}, "title": "Sign in to continue using Claude."},
			true, false, "Sign in to continue using Claude.", false},
		{"transient rate limit", map[string]any{"kind": "rate_limited", "category": "limit", "actions": []any{"retry"},
			"title": "Claude is temporarily rate limited."}, false, false, "Claude is temporarily rate limited.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := map[string]any{"id": "turn:error", "revision": float64(1), "severity": "error"}
			for key, value := range tc.failure {
				failure[key] = value
			}
			err := SessionFailurePromptError(acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn, Meta: testPromptFailureMeta(failure)})
			if err == nil {
				t.Fatal("no failure")
			}
			if errors.Is(err, ports.ErrChatAuthRequired) != tc.auth || errors.Is(err, ports.ErrChatUsageLimit) != tc.usage {
				t.Fatalf("err = %v: auth=%v usage=%v, want auth=%v usage=%v", err,
					errors.Is(err, ports.ErrChatAuthRequired), errors.Is(err, ports.ErrChatUsageLimit), tc.auth, tc.usage)
			}
			if err.Error() != tc.wantMessage {
				t.Fatalf("message = %q, want %q", err.Error(), tc.wantMessage)
			}
			// The store reads the usage limit, and its reset, back from the message.
			limit := domain.ParseConversationUsageLimit(err.Error(), &failedAt)
			if (limit != nil) != tc.usage || (limit != nil && (limit.ResetsAt != nil) != tc.wantResetParsed) {
				t.Fatalf("stored usage limit = %+v, want usage=%v reset=%v", limit, tc.usage, tc.wantResetParsed)
			}
		})
	}

	warning := testPromptFailureMeta(map[string]any{"id": "w", "revision": float64(1), "severity": "warning",
		"category": "limit", "title": "Retrying Claude", "actions": []any{}})
	if err := SessionFailurePromptError(acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn, Meta: warning}); err != nil {
		t.Fatalf("a warning is not a failure: %v", err)
	}
}

func TestNormalizeACPPromptError(t *testing.T) {
	usage := normalizeACPPromptError(&acpsdk.RequestError{Code: -32603, Message: "Internal error",
		Data: map[string]any{"errorKind": "billing_error", "details": "Credit balance is too low"}})
	if !errors.Is(usage, ports.ErrChatUsageLimit) || domain.ParseConversationUsageLimit(usage.Error(), nil) == nil {
		t.Fatalf("billing error = %v, want a usage limit the store recognizes", usage)
	}
	var requestErr *acpsdk.RequestError
	if !errors.As(usage, &requestErr) {
		t.Fatal("the usage limit must keep the agent's error reachable")
	}
	auth := normalizeACPPromptError(&acpsdk.RequestError{Code: -32603, Message: "Internal error",
		Data: map[string]any{"errorKind": "authentication_failed"}})
	if !errors.Is(auth, ports.ErrChatAuthRequired) || errors.Is(auth, ports.ErrChatUsageLimit) {
		t.Fatalf("auth = %v, want only ErrChatAuthRequired", auth)
	}
	throttle := normalizeACPPromptError(&acpsdk.RequestError{Code: -32603, Message: "Internal error",
		Data: map[string]any{"errorKind": "rate_limit"}})
	if errors.Is(throttle, ports.ErrChatUsageLimit) || !strings.HasPrefix(throttle.Error(), "ACP session/prompt: ") {
		t.Fatalf("throttle = %v, want an ordinary prompt failure", throttle)
	}
}

func TestAccountRefusedModelCheckUsesStructuredKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"acp auth code", &acpsdk.RequestError{Code: -32000, Message: "Authentication required"}, true},
		{"oauth org", &acpsdk.RequestError{Code: -32603, Data: map[string]any{"errorKind": "oauth_org_not_allowed"}}, true},
		{"billing", &acpsdk.RequestError{Code: -32603, Data: map[string]any{"errorKind": "billing_error"}}, true},
		{"rate limit", &acpsdk.RequestError{Code: -32603, Data: map[string]any{"errorKind": "rate_limit"}}, true},
		{"model not found", &acpsdk.RequestError{Code: -32603, Data: map[string]any{"errorKind": "model_not_found"}}, false},
		{"usage wording", &acpsdk.RequestError{Code: -32603, Message: "Internal error",
			Data: map[string]any{"details": "You've hit your weekly limit"}}, true},
		{"not an rpc error", errors.New("model check failed: api error: 429"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountRefusedModelCheck(tc.err); got != tc.want {
				t.Fatalf("accountRefusedModelCheck = %v, want %v", got, tc.want)
			}
		})
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A failure no class matched is the next new error shape: it must reach the
// daemon log, once per turn, with what identifies it.
func TestUnclassifiedTurnFailureIsLoggedOnce(t *testing.T) {
	longText := strings.Repeat("x", 400)
	for _, tc := range []struct {
		name     string
		agent    *fakeAgent
		wantLog  bool
		wantAttr []string
	}{
		{"unclassified rpc error", &fakeAgent{promptErr: &acpsdk.RequestError{Code: -32603, Message: "Internal error: " + longText,
			Data: map[string]any{"errorKind": "server_error"}}}, true, []string{"code=-32603", "errorKind=server_error", strings.Repeat("x", 300) + "…"}},
		{"unclassified session failure", &fakeAgent{promptResponse: &acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn,
			Meta: testPromptFailureMeta(map[string]any{"id": "t:error", "revision": float64(1), "severity": "error",
				"category": "service", "kind": "provider_error", "errorKind": "account_on_hold", "title": "Your account is on hold", "actions": []any{"retry"}})}},
			true, []string{"kind=provider_error", "errorKind=account_on_hold", "category=service", "Your account is on hold"}},
		{"usage limit", &fakeAgent{promptResponse: &acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn,
			Meta: testPromptFailureMeta(map[string]any{"id": "t:error", "revision": float64(1), "severity": "error",
				"category": "limit", "title": "Your org is out of usage · add funds to continue", "actions": []any{}})}},
			false, nil},
		{"rate limit", &fakeAgent{promptErr: &acpsdk.RequestError{Code: -32603, Message: "Internal error",
			Data: map[string]any{"errorKind": "rate_limit"}}}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			driver := New(Config{
				Harness:               domain.HarnessClaudeCode,
				Capabilities:          ports.ChatCapabilities{ports.ChatCapabilityStreaming: true},
				Launch:                func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
				PromptResponseFailure: SessionFailurePromptError,
			}, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			driver.useTestProcess(fakeSpawn(tc.agent))
			opened, err := driver.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			_ = nextEvent(t, opened.Events())
			ref, err := opened.SendTurn(context.Background(), ports.ChatUserMessage{Text: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
				t.Fatal(err)
			}
			var completed ports.ChatEvent
			for completed.Kind != ports.ChatEventTurnCompleted {
				completed = nextEvent(t, opened.Events())
			}
			if completed.TurnState != domain.TurnStateFailed {
				t.Fatalf("completion = %+v, want a failed turn", completed)
			}
			if tc.name == "usage limit" && !errors.Is(completed.Err, ports.ErrChatUsageLimit) {
				t.Fatalf("usage limit turn error = %v, want ErrChatUsageLimit", completed.Err)
			}
			got := strings.Count(logs.String(), "unclassified provider turn failure")
			if want := map[bool]int{true: 1, false: 0}[tc.wantLog]; got != want {
				t.Fatalf("logged %d times, want %d:\n%s", got, want, logs.String())
			}
			for _, attr := range tc.wantAttr {
				if !strings.Contains(logs.String(), attr) {
					t.Fatalf("log lacks %q:\n%s", attr, logs.String())
				}
			}
			if tc.wantLog && strings.Contains(logs.String(), strings.Repeat("x", 301)) {
				t.Fatal("the logged message must be truncated")
			}
		})
	}
}
