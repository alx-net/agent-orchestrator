package store_test

import (
	"context"
	"testing"
	"time"
)

func TestCompactAtDefaultsTo350KAndPersists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	initial, err := s.GetAppSettings(ctx)
	if err != nil || initial.CompactAt != 350_000 {
		t.Fatalf("initial settings = %+v, %v; want compaction at 350000", initial, err)
	}
	for _, tokens := range []int{500_000, 0} {
		if err := s.SetCompactAt(ctx, tokens, time.Now()); err != nil {
			t.Fatal(err)
		}
		saved, err := s.GetAppSettings(ctx)
		if err != nil || saved.CompactAt != tokens || !saved.AccountAutoSwitch {
			t.Fatalf("saved settings = %+v, %v; want only compaction at %d", saved, err, tokens)
		}
	}
	// The column refuses a size Claude Code would clamp or ignore.
	if err := s.SetCompactAt(ctx, 50_000, time.Now()); err == nil {
		t.Fatal("SetCompactAt(50000) succeeded; want the CHECK constraint to refuse it")
	}
}
