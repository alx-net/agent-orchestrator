//go:build !windows

package agentaccount

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// fakeClaude writes an executable standing in for `claude`.
func fakeClaude(t *testing.T, script string) *Claude {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	return &Claude{
		Binary:       func(context.Context) (string, error) { return bin, nil },
		DeviceHome:   filepath.Join(home, ".claude"),
		DeviceConfig: filepath.Join(home, ".claude.json"),
	}
}

type loginOutput struct {
	mu   sync.Mutex
	text strings.Builder
}

func (o *loginOutput) add(chunk string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.text.WriteString(chunk)
}

func (o *loginOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

// Claude Code aborts a sign-in unless stdin is a terminal. This is the whole
// reason for the PTY, and the account's folder must be the one it signs in.
func TestMCPLoginRunsOnATerminalWithTheAccountsFolderAndDirectory(t *testing.T) {
	c := fakeClaude(t, `
if [ -t 0 ]; then echo "stdin-is-a-terminal"; else echo "stdin-is-a-pipe"; exit 1; fi
echo "args=$*"
echo "config=$CLAUDE_CONFIG_DIR"
echo "secure=$CLAUDE_SECURESTORAGE_CONFIG_DIR"
echo "cwd=$(pwd -P)"
`)
	account := t.TempDir()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var output loginOutput
	if err := c.MCPLogin(context.Background(), ports.MCPTarget{Home: account, Dir: dir}, "linear", output.add); err != nil {
		t.Fatalf("MCPLogin: %v (output %q)", err, output.String())
	}
	got := output.String()
	for _, want := range []string{"stdin-is-a-terminal", "args=mcp login -- linear", "config=" + account, "secure=" + account, "cwd=" + dir} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestMCPLoginOnTheComputersAccountSetsNoConfigFolder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	c := fakeClaude(t, `echo "config=[$CLAUDE_CONFIG_DIR]"`)
	var output loginOutput
	if err := c.MCPLogin(context.Background(), ports.MCPTarget{}, "x", output.add); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "config=[]") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestMCPLoginReportsAFailureAndItsOutput(t *testing.T) {
	c := fakeClaude(t, `echo "Couldn't complete authentication"; exit 3`)
	var output loginOutput
	err := c.MCPLogin(context.Background(), ports.MCPTarget{}, "x", output.add)
	if err == nil || !strings.Contains(output.String(), "Couldn't complete authentication") {
		t.Fatalf("err = %v, output = %q", err, output.String())
	}
}

// A sign-in nobody finishes must not outlive its time limit, nor leave anything
// running: the CLI holds a callback port.
func TestMCPLoginStopsTheProcessWhenItTimesOut(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	c := fakeClaude(t, `echo $$ > "`+pidFile+`"; echo waiting; sleep 300 & wait`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := c.MCPLogin(ctx, ports.MCPTarget{}, "x", func(string) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("took %s to stop", took)
	}
}

// The control protocol the real CLI speaks (measured on 2.1.314), scripted: it
// answers in order and never sees a prompt.
const fakeProbeScript = `
echo "$@" > "$ARGS_FILE"
count=0
while read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *'"subtype":"initialize"'*)
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"pid":1}}}\n' "$id"
      printf '{"type":"system","subtype":"noise"}\n' ;;
    *'"subtype":"mcp_reconnect"'*)
      echo "$line" >> "$RECONNECT_FILE"
      printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"Server status: needs-auth"}}\n' "$id" ;;
    *'"subtype":"mcp_status"'*)
      count=$((count+1))
      if [ "$count" = 1 ]; then state=pending; else state=connected; fi
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"mcpServers":[{"name":"linear","status":"%s","scope":"user","transport":"http","config":{"type":"http","url":"https://mcp.linear.app/mcp"},"tools":[{"name":"a"},{"name":"b"}]},{"name":"local","status":"failed","error":"spawn ENOENT","config":{"type":"stdio","command":"npx"}}]}}}\n' "$id" "$state" ;;
  esac
done
`

func TestMCPProbeConnectsWithoutAPromptAndWaitsForServersToSettle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARGS_FILE", filepath.Join(dir, "args"))
	t.Setenv("RECONNECT_FILE", filepath.Join(dir, "reconnect"))
	c := fakeClaude(t, fakeProbeScript)
	servers, err := c.MCPProbe(context.Background(), ports.MCPTarget{}, ports.MCPProbeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("servers = %+v", servers)
	}
	linear, local := servers[0], servers[1]
	if linear.Status != "connected" || linear.ToolCount == nil || *linear.ToolCount != 2 || strings.Join(linear.Tools, ",") != "a,b" || linear.URL != "https://mcp.linear.app/mcp" {
		t.Fatalf("linear = %+v", linear)
	}
	if local.Status != "failed" || local.Error != "spawn ENOENT" || local.Command != "npx" || local.Transport != "stdio" || local.ToolCount != nil {
		t.Fatalf("local = %+v", local)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "strict-mcp-config") || !strings.Contains(string(args), "--input-format stream-json") {
		t.Fatalf("args = %s", args)
	}
}

// A test starts only its server. The config goes through a private file, since
// it can hold a key and argv is readable by other processes.
func TestMCPProbeOfOneServerKeepsItsConfigOffTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	t.Setenv("ARGS_FILE", argsFile)
	t.Setenv("RECONNECT_FILE", filepath.Join(dir, "reconnect"))
	c := fakeClaude(t, fakeProbeScript)
	_, err := c.MCPProbe(context.Background(), ports.MCPTarget{}, ports.MCPProbeRequest{
		Servers: map[string]json.RawMessage{"linear": json.RawMessage(`{"type":"http","url":"https://x","headers":{"Authorization":"Bearer s3cret"}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--strict-mcp-config --mcp-config") {
		t.Fatalf("args = %s", args)
	}
	if strings.Contains(string(args), "s3cret") {
		t.Fatalf("a secret reached argv: %s", args)
	}
	fields := strings.Fields(string(args))
	if _, err := os.Stat(fields[len(fields)-1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the config file %s outlived the probe: %v", fields[len(fields)-1], err)
	}
}

func TestMCPProbeCanReconnectANamedServer(t *testing.T) {
	dir := t.TempDir()
	reconnect := filepath.Join(dir, "reconnect")
	t.Setenv("ARGS_FILE", filepath.Join(dir, "args"))
	t.Setenv("RECONNECT_FILE", reconnect)
	c := fakeClaude(t, fakeProbeScript)
	if _, err := c.MCPProbe(context.Background(), ports.MCPTarget{}, ports.MCPProbeRequest{Reconnect: "linear"}); err != nil {
		t.Fatal(err)
	}
	line, err := os.ReadFile(reconnect)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), `"serverName":"linear"`) {
		t.Fatalf("reconnect request = %s", line)
	}
}

func TestMCPProbeReportsACLIThatDiedAndWhy(t *testing.T) {
	c := fakeClaude(t, `echo "Invalid MCP configuration" >&2; exit 1`)
	_, err := c.MCPProbe(context.Background(), ports.MCPTarget{}, ports.MCPProbeRequest{})
	if err == nil || !strings.Contains(err.Error(), "Invalid MCP configuration") {
		t.Fatalf("err = %v", err)
	}
}

func TestMCPCommandTreatsANonzeroExitAsAResult(t *testing.T) {
	account := t.TempDir()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := fakeClaude(t, `echo "out $CLAUDE_CONFIG_DIR $(pwd -P) $*"; echo "already exists" >&2; exit 1`)
	result, err := c.MCPCommand(context.Background(), ports.MCPTarget{Home: account, Dir: dir}, "mcp", "add-json", "x")
	if err != nil {
		t.Fatal(err)
	}
	want := "out " + account + " " + dir + " mcp add-json x\n"
	if result.ExitCode != 1 || result.Stderr != "already exists\n" || result.Stdout != want {
		t.Fatalf("result = %+v, want stdout %q", result, want)
	}
}

func TestMCPConfigReadsEachFile(t *testing.T) {
	c := fakeClaude(t, "")
	if err := os.WriteFile(c.DeviceConfig, []byte(`{"mcpServers":{"device":{"type":"http"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	account := t.TempDir()
	if err := os.WriteFile(filepath.Join(account, ".claude.json"), []byte(`{"mcpServers":{"own":{"type":"stdio"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".mcp.json"), []byte(`{"mcpServers":{"repo":{"type":"sse"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := c.MCPUserServers(""); err != nil || got["device"] == nil {
		t.Fatalf("device = %v, %v", got, err)
	}
	if got, err := c.MCPUserServers(account); err != nil || got["own"] == nil {
		t.Fatalf("account = %v, %v", got, err)
	}
	if got, err := c.MCPProjectServers(repo); err != nil || got["repo"] == nil {
		t.Fatalf("repo = %v, %v", got, err)
	}
	if got, err := c.MCPProjectServers(t.TempDir()); err != nil || len(got) != 0 {
		t.Fatalf("no .mcp.json = %v, %v", got, err)
	}
}

// The approval is the account's own, per folder, in the spot Claude Code honors;
// nothing else in the file moves and no repo file changes.
func TestMCPApproveWritesOnlyThatAccountsProjectEntry(t *testing.T) {
	c := fakeClaude(t, "")
	account := t.TempDir()
	config := filepath.Join(account, ".claude.json")
	if err := os.WriteFile(config, []byte(`{"userID":"keep","projects":{"/other":{"lastCost":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	// On macOS a temp dir is a symlink, and Claude Code keys projects by the real path.
	key, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"posthog", "semrush", "posthog"} {
		if err := c.MCPApprove(account, repo, name); err != nil {
			t.Fatal(err)
		}
	}
	var saved struct {
		UserID   string `json:"userID"`
		Projects map[string]struct {
			Trust    bool     `json:"hasTrustDialogAccepted"`
			Enabled  []string `json:"enabledMcpjsonServers"`
			Disabled []string `json:"disabledMcpjsonServers"`
			LastCost int      `json:"lastCost"`
		} `json:"projects"`
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	project := saved.Projects[key]
	if !project.Trust || strings.Join(project.Enabled, ",") != "posthog,semrush" {
		t.Fatalf("project = %+v", project)
	}
	if saved.UserID != "keep" || saved.Projects["/other"].LastCost != 1 {
		t.Fatalf("other keys moved: %s", data)
	}
	if entries, err := os.ReadDir(repo); err != nil || len(entries) != 0 {
		t.Fatalf("the repo changed: %v %v", entries, err)
	}
}

// Claude Code reads approvals for a linked worktree from the main checkout's
// entry, so an approval under the worktree's path alone never counts.
func TestMCPApproveInAWorktreeApprovesTheMainCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	c := fakeClaude(t, "")
	account := t.TempDir()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main, worktree := filepath.Join(base, "main"), filepath.Join(base, "wt")
	if err := os.Mkdir(main, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "init"},
		{"worktree", "add", "-q", "--detach", worktree},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = main
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := c.MCPApprove(account, worktree, "mlflow"); err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Projects map[string]struct {
			Trust   bool     `json:"hasTrustDialogAccepted"`
			Enabled []string `json:"enabledMcpjsonServers"`
		} `json:"projects"`
	}
	data, err := os.ReadFile(filepath.Join(account, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{worktree, main} {
		entry := saved.Projects[filepath.ToSlash(key)]
		if !entry.Trust || len(entry.Enabled) != 1 || entry.Enabled[0] != "mlflow" {
			t.Fatalf("entry for %s = %+v", key, entry)
		}
	}
}

func TestMCPApproveLiftsAnEarlierRejection(t *testing.T) {
	c := fakeClaude(t, "")
	repo := t.TempDir()
	key, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := json.Marshal(map[string]any{"projects": map[string]any{key: map[string]any{"disabledMcpjsonServers": []string{"posthog", "other"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.DeviceConfig, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.MCPApprove("", repo, "posthog"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.DeviceConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"disabledMcpjsonServers": [`) || strings.Contains(strings.Split(string(data), "disabledMcpjsonServers")[1], `posthog",`) {
		t.Fatalf("config = %s", data)
	}
}

// Claude can leave the file mid-write: AO must refuse, not replace the user's
// whole config with one entry.
func TestMCPApproveRefusesAnUnreadableConfig(t *testing.T) {
	c := fakeClaude(t, "")
	if err := os.WriteFile(c.DeviceConfig, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.MCPApprove("", t.TempDir(), "x"); err == nil {
		t.Fatal("an unreadable config was overwritten")
	}
	data, err := os.ReadFile(c.DeviceConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("config = %s", data)
	}
}

func TestMCPForgetAuthCacheDropsOnlyWhatItIsAsked(t *testing.T) {
	c := fakeClaude(t, "")
	account := t.TempDir()
	path := filepath.Join(account, claudeMCPNeedsAuthFile)
	if err := os.WriteFile(path, []byte(`{"keep":{"timestamp":1},"drop":{"timestamp":2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.MCPForgetAuthCache(account, "drop"); err != nil {
		t.Fatal(err)
	}
	if got := c.MCPNeedsAuth(account); strings.Join(got, ",") != "keep" {
		t.Fatalf("after dropping one: %v", got)
	}
	if err := c.MCPForgetAuthCache(account); err != nil {
		t.Fatal(err)
	}
	if got := c.MCPNeedsAuth(account); len(got) != 0 {
		t.Fatalf("after dropping all: %v", got)
	}
	// No file yet: nothing to do, and it does not create one.
	empty := t.TempDir()
	if err := c.MCPForgetAuthCache(empty, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(empty, claudeMCPNeedsAuthFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file appeared: %v", err)
	}
}

func TestMCPTrustRecordsOnlyTrust(t *testing.T) {
	c := fakeClaude(t, "")
	repo := t.TempDir()
	if err := c.MCPTrust("", repo); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.DeviceConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"hasTrustDialogAccepted": true`) || strings.Contains(string(data), "enabledMcpjsonServers") {
		t.Fatalf("config = %s", data)
	}
}
