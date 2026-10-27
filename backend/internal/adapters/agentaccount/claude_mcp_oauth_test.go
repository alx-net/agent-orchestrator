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
