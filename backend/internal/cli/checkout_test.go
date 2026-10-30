package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type capturedCheckoutCall struct {
	method, path string
	body         checkoutAPIRequest
}

func checkoutDaemon(t *testing.T, status int, response string) (*[]capturedCheckoutCall, Deps) {
	t.Helper()
	cfg := setConfigEnv(t)
	var calls []capturedCheckoutCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		call := capturedCheckoutCall{method: r.Method, path: r.URL.Path}
		_ = json.NewDecoder(r.Body).Decode(&call.body)
		calls = append(calls, call)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	return &calls, Deps{ProcessAlive: func(int) bool { return true }}
}

const checkoutCreated = `{"projectId":"kugelaudio","projectName":"KugelAudio","path":"/home/u/.ao/data/worktrees/standalone/sessions/sa-1/repos/kugelaudio",
	"ref":"origin/master","commit":"1a2b3c4d5e","shortCommit":"1a2b3c4","commitDate":"2026-10-29T14:02:00+01:00","subject":"feat: curb","action":"created"}`

func TestCheckoutRequiresAnAOSessionAndOneProject(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "")
	if _, _, err := executeCLI(t, Deps{}, "checkout", "kugelaudio"); ExitCode(err) != 2 || !strings.Contains(err.Error(), "inside an AO session") {
		t.Fatalf("err=%v exit=%d", err, ExitCode(err))
	}
	t.Setenv("AO_SESSION_ID", "sa-1")
	for _, args := range [][]string{{"checkout"}, {"checkout", "a", "b"}, {"checkout", " "}, {"checkout", "a", "--ref", "-x"}} {
		if _, _, err := executeCLI(t, Deps{}, args...); ExitCode(err) != 2 {
			t.Fatalf("%v: err=%v exit=%d", args, err, ExitCode(err))
		}
	}
}

func TestCheckoutPostsAndPrintsWhereToWork(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "sa-1")
	calls, deps := checkoutDaemon(t, http.StatusCreated, checkoutCreated)
	out, _, err := executeCLI(t, deps, "checkout", "KugelAudio", "--ref", "release/1.2", "--refresh")
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].method != http.MethodPost || (*calls)[0].path != "/api/v1/sessions/sa-1/checkouts" ||
		(*calls)[0].body != (checkoutAPIRequest{Project: "KugelAudio", Ref: "release/1.2", Refresh: true}) {
		t.Fatalf("calls = %+v", *calls)
	}
	for _, want := range []string{
		"Checked out KugelAudio (kugelaudio) at origin/master.",
		"path:   /home/u/.ao/data/worktrees/standalone/sessions/sa-1/repos/kugelaudio",
		"commit: 1a2b3c4 2026-10-29 14:02 +0100 feat: curb",
		"git switch -c <branch>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCheckoutReportsKeptCopyAndWarnings(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "sa-1")
	kept := strings.Replace(checkoutCreated, `"action":"created"`,
		`"action":"kept","note":"it has uncommitted changes","fetchWarning":"git fetch origin failed","submodules":true`, 1)
	_, deps := checkoutDaemon(t, http.StatusOK, kept)
	out, _, err := executeCLI(t, deps, "checkout", "kugelaudio")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Left your copy of KugelAudio (kugelaudio) untouched because it has uncommitted changes; it is not at the latest origin/master.", "warning: git fetch origin failed", "submodules are not initialized"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	_, deps = checkoutDaemon(t, http.StatusOK, checkoutCreated)
	out, _, err = executeCLI(t, deps, "checkout", "kugelaudio", "--json")
	if err != nil || !strings.Contains(out, `"path": "/home/u/.ao/data/worktrees/standalone/sessions/sa-1/repos/kugelaudio"`) {
		t.Fatalf("json out=%s err=%v", out, err)
	}
}

func TestCheckoutPreservesDaemonErrorEnvelope(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "sa-1")
	_, deps := checkoutDaemon(t, http.StatusNotFound, `{"message":"No project \"curb\". Available: kugelaudio (KugelAudio)","code":"CHECKOUT_PROJECT_NOT_FOUND","requestId":"req-7"}`)
	_, _, err := executeCLI(t, deps, "checkout", "curb")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), `Available: kugelaudio (KugelAudio) (CHECKOUT_PROJECT_NOT_FOUND) [request req-7]`) {
		t.Fatalf("err=%v exit=%d", err, ExitCode(err))
	}
}
