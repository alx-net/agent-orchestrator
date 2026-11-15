package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type capturedWaitCall struct {
	method string
	path   string
	body   waitAPIRequest
}

// waitDaemon answers the wait routes with response and records each call.
func waitDaemon(t *testing.T, status int, response string) (*[]capturedWaitCall, Deps) {
	t.Helper()
	cfg := setConfigEnv(t)
	var calls []capturedWaitCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			// The CLI's own invocation telemetry is not part of this contract.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		call := capturedWaitCall{method: r.Method, path: r.URL.Path}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&call.body)
		}
		calls = append(calls, call)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	return &calls, Deps{ProcessAlive: func(int) bool { return true }}
}

func TestWaitRequiresAnAOSession(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "")
	for _, args := range [][]string{{"wait", "pr", "5807"}, {"wait", "time", "20m"}, {"wait", "ls"}, {"wait", "cancel", "w-1"}} {
		_, _, err := executeCLI(t, Deps{}, args...)
		if ExitCode(err) != 2 || !strings.Contains(err.Error(), "inside an AO session") {
			t.Fatalf("%v: err=%v exit=%d", args, err, ExitCode(err))
		}
	}
}

func TestWaitUsageErrors(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "app-1")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bare wait", []string{"wait"}},
		{"pr without number", []string{"wait", "pr"}},
		{"pr not a number", []string{"wait", "pr", "abc"}},
		{"pr zero", []string{"wait", "pr", "0"}},
		{"pr two args", []string{"wait", "pr", "1", "2"}},
		{"bad for", []string{"wait", "pr", "1", "--for", "deploy"}},
		{"bad timeout", []string{"wait", "pr", "1", "--timeout", "soon"}},
		{"time without duration", []string{"wait", "time"}},
		{"time bad duration", []string{"wait", "time", "20"}},
		{"time negative", []string{"wait", "time", "-5m"}},
		{"time fractional seconds", []string{"wait", "time", "1.5s"}},
		{"ls with args", []string{"wait", "ls", "x"}},
		{"cancel without id", []string{"wait", "cancel"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := executeCLI(t, Deps{}, tc.args...)
			if ExitCode(err) != 2 {
				t.Fatalf("err=%v exit=%d", err, ExitCode(err))
			}
		})
	}
}

func TestWaitPRRegistersAndTellsTheAgentToEndItsTurn(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "app-1")
	created := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	expires := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	for _, tc := range []struct {
		name     string
		args     []string
		existing bool
		forValue string
		want     waitAPIRequest
		output   string
	}{
		{
			name: "default checks", args: []string{"wait", "pr", "5807"}, forValue: "checks",
			want:   waitAPIRequest{Kind: "pr", PRNumber: 5807, For: "checks"},
			output: "Waiting (w-abc123): AO will message this session when CI on #5807 finishes. End your turn now.\n",
		},
		{
			name: "merge with note and timeout", args: []string{"wait", "pr", "#5807", "--for", "merge", "--note", "then rebase #5810", "--timeout", "6h"}, forValue: "merge",
			want:   waitAPIRequest{Kind: "pr", PRNumber: 5807, For: "merge", Note: "then rebase #5810", TimeoutSeconds: 6 * 3600},
			output: "Waiting (w-abc123): AO will message this session when #5807 is merged or closed. End your turn now.\n",
		},
		{
			name: "review by URL with day timeout", args: []string{"wait", "pr", "https://github.com/acme/app/pull/5807", "--for", "REVIEW", "--timeout", "2d"}, forValue: "review",
			want:   waitAPIRequest{Kind: "pr", PRURL: "https://github.com/acme/app/pull/5807", For: "review", TimeoutSeconds: 2 * 24 * 3600},
			output: "Waiting (w-abc123): AO will message this session when a new review or review comment arrives on #5807. End your turn now.\n",
		},
		{
			name: "any, already registered", args: []string{"wait", "pr", "5807", "--for", "any"}, forValue: "any", existing: true,
			want:   waitAPIRequest{Kind: "pr", PRNumber: 5807, For: "any"},
			output: "Already waiting (w-abc123): AO will message this session when CI on #5807 finishes, a review arrives, or it is merged or closed. End your turn now.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, _ := json.Marshal(map[string]any{
				"wait": map[string]any{
					"id": "w-abc123", "sessionId": "app-1", "kind": "pr", "for": tc.forValue, "prNumber": 5807,
					"state": "pending", "createdAt": created, "expiresAt": expires,
				},
				"existing": tc.existing,
			})
			calls, deps := waitDaemon(t, http.StatusCreated, string(response))
			out, _, err := executeCLI(t, deps, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 || (*calls)[0].method != http.MethodPost || (*calls)[0].path != "/api/v1/sessions/app-1/waits" {
				t.Fatalf("calls = %+v", *calls)
			}
			if got := (*calls)[0].body; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("request = %+v, want %+v", got, tc.want)
			}
			if out != tc.output {
				t.Fatalf("output = %q, want %q", out, tc.output)
			}
		})
	}
}

func TestWaitTimeRegisters(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "app-1")
	calls, deps := waitDaemon(t, http.StatusCreated, `{"wait":{"id":"w-t1","sessionId":"app-1","kind":"time","state":"pending",
		"createdAt":"2026-10-28T12:00:00Z","fireAt":"2026-10-28T13:30:00Z","expiresAt":"2026-10-28T13:30:00Z"}}`)
	out, _, err := executeCLI(t, deps, "wait", "time", "1h30m", "--note", "check the canary")
	if err != nil {
		t.Fatal(err)
	}
	want := waitAPIRequest{Kind: "time", DurationSeconds: 5400, Note: "check the canary"}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0].body, want) {
		t.Fatalf("calls = %+v", *calls)
	}
	if out != "Waiting (w-t1): AO will message this session in 1h30m. End your turn now.\n" {
		t.Fatalf("output = %q", out)
	}
}

func TestWaitListAndCancel(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "app-1")
	now := time.Now().UTC()
	list, _ := json.Marshal(listWaitsAPIResponse{Waits: []waitDTO{
		{ID: "w-1", Kind: "pr", For: "checks", PRNumber: 5807, Note: "flaky fix", State: "pending", CreatedAt: now.Add(-5 * time.Minute), ExpiresAt: now.Add(23 * time.Hour)},
	}})
	calls, deps := waitDaemon(t, http.StatusOK, string(list))
	deps.Now = func() time.Time { return now }
	out, _, err := executeCLI(t, deps, "wait", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if (*calls)[0].method != http.MethodGet || (*calls)[0].path != "/api/v1/sessions/app-1/waits" {
		t.Fatalf("calls = %+v", *calls)
	}
	for _, want := range []string{"ID", "WAITING FOR", "w-1", "CI on #5807", "5m ago", "in 23h", "flaky fix"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls output missing %q:\n%s", want, out)
		}
	}

	_, deps = waitDaemon(t, http.StatusOK, `{"waits":[]}`)
	if out, _, err := executeCLI(t, deps, "wait", "ls"); err != nil || out != "No pending waits.\n" {
		t.Fatalf("empty ls = %q err=%v", out, err)
	}

	calls, deps = waitDaemon(t, http.StatusOK, `{"wait":{"id":"w-1","kind":"pr","for":"merge","prNumber":5807,"state":"cancelled"}}`)
	out, _, err = executeCLI(t, deps, "wait", "cancel", "w-1")
	if err != nil {
		t.Fatal(err)
	}
	if (*calls)[0].method != http.MethodDelete || (*calls)[0].path != "/api/v1/sessions/app-1/waits/w-1" {
		t.Fatalf("calls = %+v", *calls)
	}
	if out != "Cancelled w-1 (merge of #5807).\n" {
		t.Fatalf("cancel output = %q", out)
	}
}

func TestWaitPreservesDaemonErrorEnvelope(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "app-1")
	_, deps := waitDaemon(t, http.StatusBadRequest, `{"message":"AO cannot observe PR #77","code":"WAIT_PR_UNOBSERVABLE","requestId":"req-9"}`)
	_, _, err := executeCLI(t, deps, "wait", "pr", "77")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "AO cannot observe PR #77 (WAIT_PR_UNOBSERVABLE) [request req-9]") {
		t.Fatalf("err=%v exit=%d", err, ExitCode(err))
	}
}
