package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

type mcpDaemonCall struct {
	method, path string
	body         map[string]any
}

// mcpDaemon answers the MCP routes from a script keyed by "METHOD path".
type mcpDaemon struct {
	mu        sync.Mutex
	calls     []mcpDaemonCall
	responses map[string][]string // answered in order; the last repeats
	status    map[string]int
}

func newMCPDaemon(t *testing.T) (*mcpDaemon, Deps) {
	t.Helper()
	cfg := setConfigEnv(t)
	daemon := &mcpDaemon{responses: map[string][]string{}, status: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") || strings.Contains(r.URL.Path, "telemetry") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		key := r.Method + " " + r.URL.RequestURI()
		call := mcpDaemonCall{method: r.Method, path: r.URL.RequestURI()}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&call.body)
		}
		daemon.mu.Lock()
		daemon.calls = append(daemon.calls, call)
		queue := daemon.responses[key]
		response := `{}`
		if len(queue) > 0 {
			response = queue[0]
			if len(queue) > 1 {
				daemon.responses[key] = queue[1:]
			}
		}
		status, ok := daemon.status[key]
		daemon.mu.Unlock()
		if !ok {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	previous := mcpLoginPoll
	mcpLoginPoll = time.Millisecond
	t.Cleanup(func() { mcpLoginPoll = previous })
	return daemon, Deps{ProcessAlive: func(int) bool { return true }}
}

func (d *mcpDaemon) answer(key string, responses ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.responses[key] = responses
}

func (d *mcpDaemon) call(t *testing.T, method, path string) mcpDaemonCall {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, call := range d.calls {
		if call.method == method && call.path == path {
			return call
		}
	}
	t.Fatalf("no %s %s in %+v", method, path, d.calls)
	return mcpDaemonCall{}
}

func TestMCPLoginInASessionBlocksUntilTheBrowserFinishes(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "s1")
	daemon, deps := newMCPDaemon(t)
	daemon.answer("POST /api/v1/mcp/connect", `{"operation":{"id":"op1","name":"linear","accountId":"a","state":"waiting"}}`)
	daemon.answer("GET /api/v1/mcp/connect/op1",
		`{"operation":{"id":"op1","name":"linear","accountId":"a","state":"waiting","authUrl":"https://auth.test/x"}}`,
		`{"operation":{"id":"op1","name":"linear","accountId":"a","state":"waiting","authUrl":"https://auth.test/x"}}`,
		`{"operation":{"id":"op1","name":"linear","accountId":"a","state":"connected","applied":{"inPlace":["s1"]}}}`,
	)
	stdout, _, err := executeCLI(t, deps, "mcp", "login", "linear")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Approve it in the browser that just opened", "https://auth.test/x", "Connected linear. Its tools are available to you now."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Count(stdout, "https://auth.test/x") != 1 {
		t.Errorf("the link printed more than once:\n%s", stdout)
	}
	body := daemon.call(t, http.MethodPost, "/api/v1/mcp/connect").body
	if body["name"] != "linear" || body["sessionId"] != "s1" {
		t.Fatalf("body = %v", body)
	}
}

func TestMCPLoginSaysWhenTheAgentRestarts(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "s1")
	daemon, deps := newMCPDaemon(t)
	daemon.answer("POST /api/v1/mcp/connect", `{"operation":{"id":"op1","name":"linear","state":"connected","applied":{"restarting":["s1"]}}}`)
	stdout, _, err := executeCLI(t, deps, "mcp", "login", "linear")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "restarts this agent, conversation kept") {
		t.Fatalf("stdout = %s", stdout)
	}
}

func TestMCPLoginFailsWithClaudeCodesReason(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "")
	daemon, deps := newMCPDaemon(t)
	daemon.answer("POST /api/v1/mcp/connect", `{"operation":{"id":"op1","name":"x","state":"failed","error":"Couldn't complete authentication"}}`)
	_, _, err := executeCLI(t, deps, "mcp", "login", "x")
	if err == nil || !strings.Contains(err.Error(), "Couldn't complete authentication") || ExitCode(err) != 1 {
		t.Fatalf("err = %v", err)
	}
	if body := daemon.call(t, http.MethodPost, "/api/v1/mcp/connect").body; body["sessionId"] != nil {
		t.Fatalf("no session should be sent: %v", body)
	}
}

func TestMCPLoginOutsideASessionCanNameAnAccount(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "s1")
	daemon, deps := newMCPDaemon(t)
	daemon.answer("POST /api/v1/mcp/connect", `{"operation":{"id":"op1","name":"x","state":"connected"}}`)
	if _, _, err := executeCLI(t, deps, "mcp", "login", "x", "--account", "acct-2"); err != nil {
		t.Fatal(err)
	}
	body := daemon.call(t, http.MethodPost, "/api/v1/mcp/connect").body
	if body["accountId"] != "acct-2" || body["sessionId"] != nil {
		t.Fatalf("body = %v", body)
	}
}

func TestMCPTestPassesAndFailsWithAnExitCode(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "s1")
	daemon, deps := newMCPDaemon(t)
	daemon.answer("POST /api/v1/mcp/test",
		`{"ok":true,"name":"linear","reason":"Connected. It exposes 2 tools.","tools":["a","b"],"live":{"status":"connected","toolCount":2}}`,
		`{"ok":false,"name":"linear","reason":"It needs a sign-in on this subscription. Run: ao mcp login linear"}`,
	)
	stdout, _, err := executeCLI(t, deps, "mcp", "test", "linear")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PASS linear: Connected. It exposes 2 tools.", "Tools: a, b", "This agent has it: connected, 2 tools"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	stdout, _, err = executeCLI(t, deps, "mcp", "test", "linear")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("a failed test must exit 1: %v", err)
	}
	if !strings.Contains(stdout, "FAIL linear: It needs a sign-in") {
		t.Fatalf("stdout = %s", stdout)
	}
}

func TestMCPLsInASessionAsksItsRunningAgent(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "s1")
	daemon, deps := newMCPDaemon(t)
	daemon.answer("GET /api/v1/sessions/s1/mcp-servers", `{"live":true,"accountId":"a","servers":[
		{"name":"linear","scope":"global","auth":"oauth","accounts":[{"accountId":"a","status":"needs_sign_in","action":"connect","live":true}]},
		{"name":"sentry","scope":"global","auth":"helper","accounts":[{"accountId":"a","status":"connected","toolCount":12,"live":true}]},
		{"name":"spec","scope":"global","auth":"none","accounts":[{"accountId":"a","status":"failed","error":"spawn ENOENT\nmore","live":true}]},
		{"name":"odd","scope":"other","auth":"unknown","accounts":[{"accountId":"a","status":"unknown","rawStatus":"hibernating","live":true}]}
	]}`)
	stdout, _, err := executeCLI(t, deps, "mcp", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"needs sign-in", "ao mcp login linear", "connected", "12", "spawn ENOENT", "hibernating"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "more") {
		t.Errorf("a multi-line error broke the table:\n%s", stdout)
	}
}

func TestMCPLsOutsideASessionCoversEverySubscription(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "")
	daemon, deps := newMCPDaemon(t)
	daemon.answer("GET /api/v1/mcp/servers?wait=true", `{"accounts":[{"id":"d","device":true},{"id":"a","email":"a@example.test"}],"servers":[
		{"name":"linear","scope":"repo","projectName":"App","auth":"oauth","accounts":[{"accountId":"d","status":"connected"},{"accountId":"a","status":"needs_approval","action":"approve"}]}
	]}`)
	stdout, _, err := executeCLI(t, deps, "mcp", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"this computer", "a@example.test", "repo:App", "needs approval", "approve in Settings"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestMCPAddSendsAURLWithHeaders(t *testing.T) {
	daemon, deps := newMCPDaemon(t)
	daemon.answer("POST /api/v1/mcp/servers", `{"applied":{"inPlace":["s1","s2"]}}`)
	stdout, _, err := executeCLI(t, deps, "mcp", "add", "acme", "https://acme.test/mcp", "--header", "X-Api-Key: s3cret", "-H", "X-Other: a:b")
	if err != nil {
		t.Fatal(err)
	}
	body := daemon.call(t, http.MethodPost, "/api/v1/mcp/servers").body
	headers, _ := body["headers"].(map[string]any)
	if body["url"] != "https://acme.test/mcp" || body["command"] != nil || headers["X-Api-Key"] != "s3cret" || headers["X-Other"] != "a:b" {
		t.Fatalf("body = %v", body)
	}
	if strings.Contains(stdout, "s3cret") {
		t.Fatalf("a secret reached stdout: %s", stdout)
	}
	if !strings.Contains(stdout, "Running agents have the change now (2)") {
		t.Fatalf("stdout = %s", stdout)
	}
}

func TestMCPAddSendsACommandWithArgsAndEnv(t *testing.T) {
	daemon, deps := newMCPDaemon(t)
	if _, _, err := executeCLI(t, deps, "mcp", "add", "files", "--env", "ROOT=/data", "--", "npx", "-y", "@acme/files"); err != nil {
		t.Fatal(err)
	}
	body := daemon.call(t, http.MethodPost, "/api/v1/mcp/servers").body
	args, _ := body["args"].([]any)
	env, _ := body["env"].(map[string]any)
	if body["command"] != "npx" || len(args) != 2 || args[0] != "-y" || args[1] != "@acme/files" || env["ROOT"] != "/data" || body["url"] != nil {
		t.Fatalf("body = %v", body)
	}
}

func TestMCPAddUsageErrorsNeverRepeatASecret(t *testing.T) {
	_, deps := newMCPDaemon(t)
	for _, args := range [][]string{
		{"mcp", "add", "only-a-name"},
		{"mcp", "add", "x", "https://x", "extra"},
		{"mcp", "add", "x", "npx", "--header", "A: b"},
		{"mcp", "add", "x", "https://x", "--header", "no-separator-s3cret"},
		{"mcp", "add", "x", "--env", "no-separator-s3cret", "--", "npx"},
	} {
		_, _, err := executeCLI(t, deps, args...)
		if err == nil || ExitCode(err) != 2 {
			t.Errorf("%v: err = %v, want a usage error", args, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%v: the message repeats a secret: %v", args, err)
		}
	}
}

func TestMCPRemovePreservesTheDaemonsError(t *testing.T) {
	daemon, deps := newMCPDaemon(t)
	daemon.answer("DELETE /api/v1/mcp/servers/linear", `{"applied":{"restarting":["s1"]}}`)
	stdout, _, err := executeCLI(t, deps, "mcp", "rm", "linear")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "restart with their conversation kept") {
		t.Fatalf("stdout = %s", stdout)
	}
	key := "DELETE /api/v1/mcp/servers/nope"
	daemon.answer(key, `{"code":"MCP_SERVER_NOT_FOUND","message":"no MCP server with that name"}`)
	daemon.mu.Lock()
	daemon.status[key] = http.StatusNotFound
	daemon.mu.Unlock()
	if _, _, err := executeCLI(t, deps, "mcp", "rm", "nope"); err == nil || !strings.Contains(err.Error(), "no MCP server with that name") {
		t.Fatalf("err = %v", err)
	}
}

// Bare `ao mcp` is still the stdio server a harness spawns, with piped stdin; only
// a person at a terminal gets help instead.
func TestBareMCPServesOnAPipeAndHelpsAtATerminal(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	piped := &cobra.Command{}
	piped.SetIn(reader)
	if stdinIsTerminal(piped) {
		t.Fatal("a pipe read as a terminal: the harness's stdio server would print help")
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	terminal := &cobra.Command{}
	terminal.SetIn(devNull) // a character device, as a terminal is
	if !stdinIsTerminal(terminal) {
		t.Fatal("a character device did not read as a terminal")
	}
	notAFile := &cobra.Command{}
	notAFile.SetIn(strings.NewReader(""))
	if stdinIsTerminal(notAFile) {
		t.Fatal("an in-memory reader read as a terminal")
	}
}
