package agentaccount

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memKeychain struct {
	items    map[string][]byte
	writes   int
	truncate bool // the first write keeps only part of the data
}

func (k *memKeychain) Find(_ context.Context, service, account string) ([]byte, error) {
	item, ok := k.items[service+"/"+account]
	if !ok {
		return nil, ErrKeychainItemNotFound
	}
	return item, nil
}

func (k *memKeychain) Store(_ context.Context, service, account string, secret []byte) error {
	k.writes++
	if k.truncate && k.writes == 1 {
		secret = secret[:len(secret)/2]
	}
	k.items[service+"/"+account] = append([]byte(nil), secret...)
	return nil
}

func mcpEntries(t *testing.T, k *memKeychain, service string) (map[string]json.RawMessage, json.RawMessage) {
	t.Helper()
	var creds map[string]json.RawMessage
	if err := json.Unmarshal(k.items[service+"/alex"], &creds); err != nil {
		t.Fatalf("credentials of %s: %v", service, err)
	}
	return object(creds["mcpOAuth"]), creds["claudeAiOauth"]
}

func TestShareMCPAuthCopiesTheComputersSignInsAndKeepsTheAccountsOwn(t *testing.T) {
	home := filepath.Join(t.TempDir(), "acct")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	account := ClaudeCredentialsService(home)
	k := &memKeychain{items: map[string][]byte{
		"Claude Code-credentials/alex": []byte(`{"claudeAiOauth":{"accessToken":"device-login"},"mcpOAuth":{"linear":{"accessToken":"device-linear"},"sentry":{"accessToken":"device-sentry"}}}`),
		account + "/alex":              []byte(`{"claudeAiOauth":{"accessToken":"account-login"},"mcpOAuth":{"sentry":{"accessToken":"account-sentry"}}}`),
	}}
	c := &Claude{Keychain: k, KeychainOwner: "alex"}
	ctx := context.Background()

	if err := c.ShareMCPAuth(ctx, home); err != nil {
		t.Fatal(err)
	}
	mcp, login := mcpEntries(t, k, account)
	if string(login) != `{"accessToken":"account-login"}` {
		t.Fatalf("the account's own sign-in changed: %s", login)
	}
	if string(mcp["linear"]) != `{"accessToken":"device-linear"}` || string(mcp["sentry"]) != `{"accessToken":"account-sentry"}` {
		t.Fatalf("mcpOAuth = %v; want linear copied, the account's sentry kept", mcp)
	}
	ledger, _ := os.ReadFile(filepath.Join(home, mcpOAuthLedgerName))
	if strings.Contains(string(ledger), "device-linear") {
		t.Fatal("the ledger holds a secret, not a digest")
	}

	// The computer refreshes linear: AO's copy follows. Nothing else is written.
	k.items["Claude Code-credentials/alex"] = []byte(`{"mcpOAuth":{"linear":{"accessToken":"device-linear-2"},"sentry":{"accessToken":"device-sentry"}}}`)
	if err := c.ShareMCPAuth(ctx, home); err != nil {
		t.Fatal(err)
	}
	if mcp, _ = mcpEntries(t, k, account); string(mcp["linear"]) != `{"accessToken":"device-linear-2"}` {
		t.Fatalf("copied linear did not follow the computer: %s", mcp["linear"])
	}

	// The account refreshes linear itself: its token wins from then on.
	var creds map[string]json.RawMessage
	_ = json.Unmarshal(k.items[account+"/alex"], &creds)
	mcp["linear"] = json.RawMessage(`{"accessToken":"account-linear"}`)
	creds["mcpOAuth"] = encode(mcp)
	k.items[account+"/alex"], _ = json.Marshal(creds)
	k.items["Claude Code-credentials/alex"] = []byte(`{"mcpOAuth":{"linear":{"accessToken":"device-linear-3"}}}`)
	writes := k.writes
	if err := c.ShareMCPAuth(ctx, home); err != nil {
		t.Fatal(err)
	}
	if mcp, _ = mcpEntries(t, k, account); string(mcp["linear"]) != `{"accessToken":"account-linear"}` || k.writes != writes {
		t.Fatalf("the account's refreshed linear was overwritten: %s (writes %d -> %d)", mcp["linear"], writes, k.writes)
	}
}

// Linear rotates refresh tokens: a copy dies the moment the other holder
// refreshes, so a rotating sign-in stays with whoever made it.
func TestShareMCPAuthNeverCopiesARotatingSignIn(t *testing.T) {
	home := t.TempDir()
	account := ClaudeCredentialsService(home)
	k := &memKeychain{items: map[string][]byte{
		"Claude Code-credentials/alex": []byte(`{"mcpOAuth":{"linear|a":{"accessToken":"device-at","refreshToken":"device-rt"},"static|b":{"accessToken":"static-at"}}}`),
		account + "/alex":              []byte(`{"claudeAiOauth":{"accessToken":"account-login"},"mcpOAuth":{"mine|c":{"accessToken":"mine-at","refreshToken":"mine-rt"}}}`),
	}}
	c := &Claude{Keychain: k, KeychainOwner: "alex"}
	if err := c.ShareMCPAuth(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	mcp, _ := mcpEntries(t, k, account)
	if _, copied := mcp["linear|a"]; copied {
		t.Fatalf("a rotating sign-in was copied: %s", mcp["linear|a"])
	}
	if string(mcp["static|b"]) != `{"accessToken":"static-at"}` {
		t.Fatalf("a non-rotating sign-in was not shared: %v", mcp)
	}
	if string(mcp["mine|c"]) != `{"accessToken":"mine-at","refreshToken":"mine-rt"}` {
		t.Fatalf("the account's own sign-in changed: %s", mcp["mine|c"])
	}
	// The account's stale copy of a server that now rotates stays as it is:
	// the computer's entry must not overwrite it either.
	k.items[account+"/alex"] = []byte(`{"mcpOAuth":{"linear|a":{"accessToken":"old-copy"}}}`)
	writes := k.writes
	k.items["Claude Code-credentials/alex"] = []byte(`{"mcpOAuth":{"linear|a":{"accessToken":"device-at-2","refreshToken":"device-rt-2"}}}`)
	if err := c.ShareMCPAuth(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	if mcp, _ = mcpEntries(t, k, account); string(mcp["linear|a"]) != `{"accessToken":"old-copy"}` || k.writes != writes {
		t.Fatalf("a rotating sign-in overwrote the account's entry: %s (writes %d -> %d)", mcp["linear|a"], writes, k.writes)
	}
}

// An entry with no access token (a server the computer is signed out of) holds
// nothing to share and must not replace a sign-in that works.
func TestShareMCPAuthNeverCopiesAnEmptyEntry(t *testing.T) {
	home := t.TempDir()
	account := ClaudeCredentialsService(home)
	k := &memKeychain{items: map[string][]byte{
		"Claude Code-credentials/alex": []byte(`{"mcpOAuth":{"linear|a":{"accessToken":""},"gone|b":{"clientId":"only-a-client"}}}`),
		account + "/alex":              []byte(`{"mcpOAuth":{"linear|a":{"accessToken":"works"}}}`),
	}}
	c := &Claude{Keychain: k, KeychainOwner: "alex"}
	if err := c.ShareMCPAuth(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	mcp, _ := mcpEntries(t, k, account)
	if string(mcp["linear|a"]) != `{"accessToken":"works"}` || k.writes != 0 {
		t.Fatalf("an empty entry overwrote a working sign-in: %s (writes %d)", mcp["linear|a"], k.writes)
	}
	if _, copied := mcp["gone|b"]; copied {
		t.Fatalf("an entry with no access token was copied: %s", mcp["gone|b"])
	}
}

func TestShareMCPAuthLeavesAnAccountWithoutCredentialsAlone(t *testing.T) {
	home := t.TempDir()
	k := &memKeychain{items: map[string][]byte{
		"Claude Code-credentials/alex": []byte(`{"mcpOAuth":{"linear":{"accessToken":"device-linear"}}}`),
	}}
	c := &Claude{Keychain: k, KeychainOwner: "alex"}
	if err := c.ShareMCPAuth(context.Background(), home); err != nil || k.writes != 0 {
		t.Fatalf("signed-out account: err=%v writes=%d; its sign-in creates the item", err, k.writes)
	}
	k.items[ClaudeCredentialsService(home)+"/alex"] = []byte("not json")
	if err := c.ShareMCPAuth(context.Background(), home); err == nil || k.writes != 0 {
		t.Fatalf("unreadable credentials: err=%v writes=%d; want refused untouched", err, k.writes)
	}
}

func TestShareMCPAuthReportsAWriteThatDidNotReadBack(t *testing.T) {
	home := t.TempDir()
	k := &memKeychain{truncate: true, items: map[string][]byte{
		"Claude Code-credentials/alex":           []byte(`{"mcpOAuth":{"linear":{"accessToken":"device-linear"}}}`),
		ClaudeCredentialsService(home) + "/alex": []byte(`{"claudeAiOauth":{"accessToken":"account-login"}}`),
	}}
	c := &Claude{Keychain: k, KeychainOwner: "alex"}
	if err := c.ShareMCPAuth(context.Background(), home); err == nil {
		t.Fatal("a truncated keychain write passed as shared")
	}
	// The cut-short write must not leave the account signed out.
	if got := string(k.items[ClaudeCredentialsService(home)+"/alex"]); got != `{"claudeAiOauth":{"accessToken":"account-login"}}` {
		t.Fatalf("account credentials after a failed write = %q", got)
	}
}

func TestClaudeCredentialsServiceNamesTheFolder(t *testing.T) {
	if got := ClaudeCredentialsService(""); got != "Claude Code-credentials" {
		t.Fatalf("default folder = %q", got)
	}
	got := ClaudeCredentialsService("/Users/alex/.ao/data/agent-accounts/claude/acct")
	if !strings.HasPrefix(got, "Claude Code-credentials-") || len(got) != len("Claude Code-credentials-")+8 {
		t.Fatalf("account folder = %q, want an 8-digit hash suffix", got)
	}
}

func TestClaudeMCPNeedsAuthNamesTheServers(t *testing.T) {
	home := t.TempDir()
	c := &Claude{DeviceHome: t.TempDir()}
	if got := c.MCPNeedsAuth(home); len(got) != 0 {
		t.Fatalf("no cache file = %v", got)
	}
	if err := os.WriteFile(filepath.Join(home, claudeMCPNeedsAuthFile), []byte(`{"posthog":{"timestamp":1},"linear":{"timestamp":2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := c.MCPNeedsAuth(home); strings.Join(got, ",") != "linear,posthog" {
		t.Fatalf("needs auth = %v", got)
	}
}
