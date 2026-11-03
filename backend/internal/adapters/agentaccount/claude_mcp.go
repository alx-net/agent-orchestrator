package agentaccount

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// mcpCommandLimit bounds `claude mcp list`, which connects to every server.
	mcpCommandLimit = 90 * time.Second
	// mcpProbeLimit bounds one whole probe. A probe sends no prompt.
	mcpProbeLimit = 75 * time.Second
	// mcpProbeSettle is how long a probe waits for servers still starting.
	mcpProbeSettle = 25 * time.Second
	mcpProbePoll   = 500 * time.Millisecond
	// mcpProbeLineLimit fits a status listing every tool of many servers.
	mcpProbeLineLimit = 16 << 20
)

var _ ports.MCPCLI = (*Claude)(nil)

// mcpConfigMu keeps AO's own read-then-write of a global config from racing
// itself. Claude Code rewrites the file too, as it does for AO's other keys.
var mcpConfigMu sync.Mutex

func (c *Claude) mcpCommand(ctx context.Context, bin string, target ports.MCPTarget, args ...string) *exec.Cmd {
	cmd := c.command(ctx, bin, target.Home, args...)
	if target.Dir != "" {
		cmd.Dir = target.Dir
	}
	return cmd
}

// MCPCommand runs `claude <args...>` under target.
func (c *Claude) MCPCommand(ctx context.Context, target ports.MCPTarget, args ...string) (ports.MCPCommandResult, error) {
	bin, err := c.Binary(ctx)
	if err != nil {
		return ports.MCPCommandResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, mcpCommandLimit)
	defer cancel()
	cmd := c.mcpCommand(ctx, bin, target, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	result := ports.MCPCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) || ctx.Err() != nil {
			return result, errors.Join(ctx.Err(), runErr)
		}
		result.ExitCode = exit.ExitCode()
	}
	return result, nil
}

// MCPUserServers reads the mcpServers of home's global config; "" is the
// computer's own.
func (c *Claude) MCPUserServers(home string) (map[string]json.RawMessage, error) {
	config, err := readObject(c.mcpConfigPath(home))
	if err != nil {
		return nil, err
	}
	return object(config["mcpServers"]), nil
}

// MCPProjectServers reads the mcpServers of dir's .mcp.json.
func (c *Claude) MCPProjectServers(dir string) (map[string]json.RawMessage, error) {
	if dir == "" {
		return map[string]json.RawMessage{}, nil
	}
	config, err := readObject(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		return nil, err
	}
	return object(config["mcpServers"]), nil
}

func (c *Claude) mcpConfigPath(home string) string {
	if home == "" {
		return c.DeviceConfig
	}
	return filepath.Join(home, ".claude.json")
}

// MCPTrust records in home's config that the folder trusts dir, as the launch
// of an agent there does. Project settings and project headersHelper servers
// are only read in a trusted folder.
func (c *Claude) MCPTrust(home, dir string) error {
	return c.updateMCPProject(home, dir, func(map[string]json.RawMessage) {})
}

// MCPApprove records in home's config that the folder trusts dir and approves
// server name from dir's .mcp.json. It is the per-account, per-folder spot
// Claude Code honors (projects[dir].enabledMcpjsonServers, measured on 2.1.314),
// so no file in the repo changes and another account's choice stays its own.
func (c *Claude) MCPApprove(home, dir, name string) error {
	return c.updateMCPProject(home, dir, func(project map[string]json.RawMessage) {
		enabled := stringList(project["enabledMcpjsonServers"])
		found := false
		for _, existing := range enabled {
			found = found || existing == name
		}
		if !found {
			project["enabledMcpjsonServers"] = encodeStrings(append(enabled, name))
		}
		disabled := stringList(project["disabledMcpjsonServers"])
		kept := make([]string, 0, len(disabled))
		for _, existing := range disabled {
			if existing != name {
				kept = append(kept, existing)
			}
		}
		if len(kept) != len(disabled) {
			project["disabledMcpjsonServers"] = encodeStrings(kept)
		}
	})
}

func (c *Claude) updateMCPProject(home, dir string, change func(map[string]json.RawMessage)) error {
	if dir == "" {
		return errors.New("no folder to approve servers in")
	}
	keys, err := mcpProjectKeys(dir)
	if err != nil {
		return err
	}
	mcpConfigMu.Lock()
	defer mcpConfigMu.Unlock()
	path := c.mcpConfigPath(home)
	config, err := readObject(path)
	if err != nil {
		return err // Claude's to repair; never overwrite it
	}
	projects := object(config["projects"])
	for _, key := range keys {
		project := object(projects[key])
		project["hasTrustDialogAccepted"] = json.RawMessage("true")
		change(project)
		projects[key] = encode(project)
	}
	config["projects"] = encode(projects)
	return writeObject(path, config)
}

// mcpProjectKeys names the entries Claude Code consults for dir, by real path
// with forward slashes. In a linked git worktree it reads MCP approvals from the
// main checkout's entry, not the worktree's (measured on 2.1.314: an approval
// under the worktree's path alone leaves `mcp login` refusing), so approving in
// one worktree of a repository approves it for all of them.
func mcpProjectKeys(dir string) ([]string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	key := filepath.ToSlash(resolved)
	if root, ok := mainCheckout(resolved); ok && root != key {
		return []string{key, root}, nil
	}
	return []string{key}, nil
}

// mainCheckout finds the repository's main checkout. Not a repository, no git, or
// a bare repository (which has no main checkout) all report false.
func mainCheckout(dir string) (string, bool) {
	out, err := aoprocess.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", false
	}
	common := strings.TrimSpace(string(out))
	if filepath.Base(common) != ".git" {
		return "", false
	}
	root, err := filepath.EvalSymlinks(filepath.Dir(common))
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(root), true
}

// MCPForgetAuthCache drops entries from the file where Claude Code remembers a
// server wanted a sign-in. While an entry stands the CLI reports needs-auth
// without connecting (measured on 2.1.314), so a server whose key was just fixed
// would keep reading as refused. Claude Code writes it again on the next refusal.
func (c *Claude) MCPForgetAuthCache(home string, names ...string) error {
	dir := home
	if dir == "" {
		dir = c.DeviceHome
	}
	if dir == "" {
		return nil
	}
	mcpConfigMu.Lock()
	defer mcpConfigMu.Unlock()
	path := filepath.Join(dir, claudeMCPNeedsAuthFile)
	cache, err := readObject(path)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		cache = map[string]json.RawMessage{}
	}
	for _, name := range names {
		delete(cache, name)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return writeObject(path, cache)
}

func stringList(raw json.RawMessage) []string {
	var out []string
	if len(raw) != 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func encodeStrings(values []string) json.RawMessage {
	out, err := json.Marshal(values)
	if err != nil {
		return json.RawMessage("[]")
	}
	return out
}

type mcpProbeResponse struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Error     string          `json:"error"`
		Response  json.RawMessage `json:"response"`
	} `json:"response"`
}

type mcpProbeStatus struct {
	MCPServers []struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		Scope     string `json:"scope"`
		Transport string `json:"transport"`
		Error     string `json:"error"`
		Config    struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Command string `json:"command"`
		} `json:"config"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	} `json:"mcpServers"`
}

// MCPProbe starts Claude Code on its stream-json control protocol, never sends
// a prompt, and asks it to connect and report. It is the check an agent's own
// startup runs, so a pass means an agent would really have the server's tools.
func (c *Claude) MCPProbe(ctx context.Context, target ports.MCPTarget, request ports.MCPProbeRequest) ([]ports.MCPProbeServer, error) {
	bin, err := c.Binary(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, mcpProbeLimit)
	defer cancel()
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--setting-sources", "user,project,local"}
	if request.Servers != nil {
		path, cleanup, err := writeMCPProbeConfig(request.Servers)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		args = append(args, "--strict-mcp-config", "--mcp-config", path)
	}
	cmd := c.mcpCommand(ctx, bin, target, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	session := newMCPProbeSession(stdin, stdout)
	defer func() {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
	}()
	if _, err := session.request(ctx, map[string]any{"subtype": "initialize", "hooks": map[string]any{}}); err != nil {
		return nil, mcpProbeError(err, &stderr)
	}
	settle := request.Settle
	if settle <= 0 {
		settle = mcpProbeSettle
	}
	servers, err := session.settled(ctx, settle)
	if err != nil {
		return nil, mcpProbeError(err, &stderr)
	}
	if request.Reconnect != "" {
		// A refused reconnect shows in the status read after it.
		if _, err := session.request(ctx, map[string]any{"subtype": "mcp_reconnect", "serverName": request.Reconnect}); err != nil && ctx.Err() != nil {
			return nil, mcpProbeError(err, &stderr)
		}
		if servers, err = session.settled(ctx, settle); err != nil {
			return nil, mcpProbeError(err, &stderr)
		}
	}
	return servers, nil
}

func mcpProbeError(err error, stderr *tailBuffer) error {
	if tail := strings.TrimSpace(stderr.String()); tail != "" {
		return fmt.Errorf("%w: %s", err, tail)
	}
	return err
}

// writeMCPProbeConfig keeps server configs, which can hold secrets, off argv:
// a private file in a private folder, gone when the probe ends.
func writeMCPProbeConfig(servers map[string]json.RawMessage) (string, func(), error) {
	dir, err := os.MkdirTemp("", "ao-mcp-probe-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		cleanup()
		return "", nil, err
	}
	path := filepath.Join(dir, "servers.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// mcpProbeSession speaks the control protocol over one process's pipes.
type mcpProbeSession struct {
	stdin     io.Writer
	nextID    int
	mu        sync.Mutex
	responses map[string]chan mcpProbeResponse
	done      chan struct{}
}

func newMCPProbeSession(stdin io.Writer, stdout io.Reader) *mcpProbeSession {
	s := &mcpProbeSession{stdin: stdin, responses: map[string]chan mcpProbeResponse{}, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), mcpProbeLineLimit)
		for scanner.Scan() {
			var line mcpProbeResponse
			if json.Unmarshal(scanner.Bytes(), &line) != nil || line.Type != "control_response" {
				continue
			}
			s.mu.Lock()
			waiter := s.responses[line.Response.RequestID]
			delete(s.responses, line.Response.RequestID)
			s.mu.Unlock()
			if waiter != nil {
				waiter <- line
			}
		}
	}()
	return s
}

func (s *mcpProbeSession) request(ctx context.Context, body map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("ao-%d", s.nextID)
	waiter := make(chan mcpProbeResponse, 1)
	s.responses[id] = waiter
	s.mu.Unlock()
	line, err := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": body})
	if err != nil {
		return nil, err
	}
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("the CLI stopped: %w", err)
	}
	select {
	case response := <-waiter:
		if response.Response.Subtype == "error" {
			return nil, errors.New(response.Response.Error)
		}
		return response.Response.Response, nil
	case <-s.done:
		return nil, errors.New("the CLI stopped before it answered")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// settled reads status until no server is still starting, or settle runs out,
// in which case a slow server reports as still starting: an honest answer.
func (s *mcpProbeSession) settled(ctx context.Context, settle time.Duration) ([]ports.MCPProbeServer, error) {
	deadline := time.Now().Add(settle)
	for {
		raw, err := s.request(ctx, map[string]any{"subtype": "mcp_status"})
		if err != nil {
			return nil, err
		}
		var status mcpProbeStatus
		if err := json.Unmarshal(raw, &status); err != nil {
			return nil, fmt.Errorf("read MCP status: %w", err)
		}
		out := make([]ports.MCPProbeServer, 0, len(status.MCPServers))
		starting := false
		for _, server := range status.MCPServers {
			starting = starting || server.Status == "pending"
			entry := ports.MCPProbeServer{
				Name: server.Name, Status: server.Status, Scope: server.Scope, Transport: server.Transport,
				URL: server.Config.URL, Command: server.Config.Command, Error: server.Error,
			}
			if entry.Transport == "" {
				entry.Transport = server.Config.Type
			}
			if server.Status == "connected" {
				count := len(server.Tools)
				entry.ToolCount = &count
				entry.Tools = make([]string, 0, count)
				for _, tool := range server.Tools {
					entry.Tools = append(entry.Tools, tool.Name)
				}
			}
			out = append(out, entry)
		}
		if !starting || time.Now().After(deadline) {
			return out, nil
		}
		select {
		case <-time.After(mcpProbePoll):
		case <-ctx.Done():
			return out, nil
		}
	}
}

// tailBuffer keeps the end of a process's stderr for an error message.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	const limit = 2048
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > limit {
		b.buf = b.buf[len(b.buf)-limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// mcpLoginCommand builds the sign-in's command with no context: MCPLogin stops
// it itself, because a terminal child leads its own session.
func (c *Claude) mcpLoginCommand(bin string, target ports.MCPTarget, name string) *exec.Cmd {
	cmd := aoprocess.Command(bin, "mcp", "login", "--", name)
	cmd.Env = os.Environ()
	for key, value := range c.LaunchEnv(target.Home) {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Dir = target.Dir
	if cmd.Dir == "" {
		cmd.Dir = os.TempDir()
	}
	return cmd
}
