package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The transcript lookup must not read every file in every project folder (that
// allocated hundreds of MB), yet still find the newest copy and notice a
// transcript that appears after a miss.
func TestDiscoverClaudePathFindsNewestAndForgetsAMiss(t *testing.T) {
	root := t.TempDir()
	id := "0a34cd2b-09aa-4bbb-8ccc-123456789abc"
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	c := NewCollector(nil, SourceRoots{ClaudeProjects: root}, nil)
	c.now = func() time.Time { return now }

	if got, err := c.discoverClaudePath(context.Background(), id); err != nil || got != "" {
		t.Fatalf("before any transcript: %q, %v", got, err)
	}
	older := filepath.Join(root, "-old-project", id+".jsonl")
	newer := filepath.Join(root, "-new-project", id+".jsonl")
	for i, path := range []string{older, newer} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	// The miss is remembered briefly, then looked up again.
	if got, _ := c.discoverClaudePath(context.Background(), id); got != "" {
		t.Fatalf("within the miss window: %q", got)
	}
	now = now.Add(2 * claudePathMissTTL)
	if got, err := c.discoverClaudePath(context.Background(), id); err != nil || got != newer {
		t.Fatalf("after the miss window: %q, %v; want %q", got, err, newer)
	}
	// A found path that disappears is looked up again.
	if err := os.Remove(newer); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.discoverClaudePath(context.Background(), id); got != older {
		t.Fatalf("after the newest was removed: %q, want %q", got, older)
	}
}
