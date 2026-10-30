package agentaccount

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// ErrKeychainItemNotFound is a keychain lookup of an item that does not exist.
var ErrKeychainItemNotFound = errors.New("keychain item not found")

var errKeychainMismatch = errors.New("keychain data differs from the write")

// Keychain reads and writes generic passwords in the user's login keychain.
type Keychain interface {
	Find(ctx context.Context, service, account string) ([]byte, error)
	Store(ctx context.Context, service, account string, secret []byte) error
}

// mcpOAuthLedgerName records, per MCP server, a digest of the sign-in AO last
// copied into an account's credentials. Digests only: no secret is written to
// disk.
const mcpOAuthLedgerName = ".ao-mcp-oauth.json"

// ClaudeCredentialsService is the keychain item Claude Code keeps a config
// folder's credentials in: its sign-in and its MCP servers' OAuth tokens. The
// default folder uses the plain name; a CLAUDE_CONFIG_DIR folder adds the
// first eight hex digits of the SHA-256 of the folder path's exact text.
func ClaudeCredentialsService(configDir string) string {
	const base = "Claude Code-credentials"
	if configDir == "" {
		return base
	}
	sum := sha256.Sum256([]byte(configDir))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// ShareMCPAuth copies the computer's MCP OAuth sign-ins into home's
// credentials, so a session on the account has every MCP server the computer
// has without signing in to each again. The user asked for this.
//
// Only the "mcpOAuth" entries move; the account's own Claude sign-in
// ("claudeAiOauth") is never touched. A server the account signed in to itself
// keeps its own entry; one AO copied earlier and the account has not changed
// since follows the computer's. The caller must not run this while an agent
// uses the account: that agent could refresh a token between the read and the
// write, and the write would undo the refresh.
//
// Refresh-token rotation: a copied entry holds the same refresh token as the
// computer's. When one side refreshes and the server rotates the token, the
// other copy's refresh token stops working and that side must sign in to the
// server again. AO keeps the account's refreshed entry as its own from then on.
func (c *Claude) ShareMCPAuth(ctx context.Context, home string) error {
	if home == "" || c.Keychain == nil {
		return nil
	}
	owner := c.keychainOwner()
	if owner == "" {
		return errors.New("share MCP sign-ins: unknown keychain account")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	deviceService := c.DeviceCredentials
	if deviceService == "" {
		deviceService = ClaudeCredentialsService("")
	}
	device, err := c.Keychain.Find(ctx, deviceService, owner)
	if errors.Is(err, ErrKeychainItemNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the computer's credentials: %w", err)
	}
	var deviceCreds map[string]json.RawMessage
	if err := json.Unmarshal(device, &deviceCreds); err != nil {
		return fmt.Errorf("read the computer's credentials: %w", err)
	}
	source := object(deviceCreds["mcpOAuth"])
	if len(source) == 0 {
		return nil
	}
	service := ClaudeCredentialsService(home)
	current, err := c.Keychain.Find(ctx, service, owner)
	if errors.Is(err, ErrKeychainItemNotFound) {
		// Not signed in yet: the account's sign-in creates the item.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the account's credentials: %w", err)
	}
	var creds map[string]json.RawMessage
	if err := json.Unmarshal(current, &creds); err != nil || creds == nil {
		return fmt.Errorf("the account's credentials are not JSON; leaving them alone")
	}
	target := object(creds["mcpOAuth"])
	copied := readDigests(filepath.Join(home, mcpOAuthLedgerName))
	changed := false
	for server, value := range source {
		mine, has := target[server]
		switch {
		case has && digest(mine) == digest(value):
			copied[server] = digest(value) // already the computer's
		case has && digest(mine) != copied[server]:
			// The account's own sign-in, or a token it refreshed: it wins.
		default:
			target[server] = value
			copied[server] = digest(value)
			changed = true
		}
	}
	if changed {
		creds["mcpOAuth"] = encode(target)
		out, err := json.Marshal(creds)
		if err != nil {
			return err
		}
		if err := c.Keychain.Store(ctx, service, owner, out); err != nil {
			return fmt.Errorf("write the account's credentials: %w", err)
		}
		// A write the keychain cut short would leave the account signed out, so
		// put the original back before reporting it.
		back, err := c.Keychain.Find(ctx, service, owner)
		if err != nil || !bytes.Equal(bytes.TrimSpace(back), out) {
			restoreErr := c.Keychain.Store(ctx, service, owner, current)
			return fmt.Errorf("the account's credentials did not read back as written: %w", errors.Join(err, restoreErr, errKeychainMismatch))
		}
	}
	return writeDigests(filepath.Join(home, mcpOAuthLedgerName), copied)
}

func (c *Claude) keychainOwner() string {
	if c.KeychainOwner != "" {
		return c.KeychainOwner
	}
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func digest(raw json.RawMessage) string {
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		compact.Reset()
		compact.Write(raw)
	}
	sum := sha256.Sum256(compact.Bytes())
	return hex.EncodeToString(sum[:])
}

func readDigests(path string) map[string]string {
	out := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &out)
	}
	if out == nil {
		out = map[string]string{}
	}
	return out
}

func writeDigests(path string, digests map[string]string) error {
	out, err := json.MarshalIndent(digests, "", "  ")
	if err != nil {
		return err
	}
	return writeIfChanged(path, append(out, '\n'))
}

// SecurityKeychain is the macOS login keychain through /usr/bin/security, the
// tool Claude Code itself keeps its credentials with.
type SecurityKeychain struct{}

// Find reads a generic password's data.
func (SecurityKeychain) Find(ctx context.Context, service, account string) ([]byte, error) {
	out, err := aoprocess.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 44 {
		return nil, ErrKeychainItemNotFound
	}
	if err != nil {
		return nil, err
	}
	out = bytes.TrimRight(out, "\r\n")
	// security prints data it cannot show as text in hex.
	if len(out) > 0 && out[0] != '{' {
		if decoded, decodeErr := hex.DecodeString(strings.TrimSpace(string(out))); decodeErr == nil {
			return decoded, nil
		}
	}
	return out, nil
}

// Store writes a generic password, replacing the item's data. The secret is
// an argument, not stdin: `security -i` cuts long input lines (about 4 KB)
// and would store a truncated credential.
func (SecurityKeychain) Store(ctx context.Context, service, account string, secret []byte) error {
	cmd := aoprocess.CommandContext(ctx, "/usr/bin/security", "add-generic-password", "-U", "-a", account, "-s", service, "-w", string(secret))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
