package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func titledSession(t *testing.T, s *sqlite.Store, displayName string) domain.SessionID {
	t.Helper()
	seedProject(t, s, "mer")
	rec := sampleRecord("mer")
	rec.DisplayName = displayName
	created, err := s.CreateSession(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func sessionTitle(t *testing.T, s *sqlite.Store, id domain.SessionID) (string, string) {
	t.Helper()
	rec, ok, err := s.GetSession(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("get session %s: ok=%v err=%v", id, ok, err)
	}
	return rec.DisplayName, rec.TitleArea
}

// An area chosen after spawn prefixes the provisional name once, and the area
// survives every session read path.
func TestSetSessionTitleAreaPrefixesProvisionalNameOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := titledSession(t, s, "Fix the login redirect")

	if ok, err := s.SetSessionTitleArea(ctx, id, "S2S", "Fix the login redirect", histClock); err != nil || !ok {
		t.Fatalf("set area = %v, %v", ok, err)
	}
	if name, area := sessionTitle(t, s, id); name != "S2S: Fix the login redirect" || area != "S2S" {
		t.Fatalf("title = %q area = %q, want prefixed provisional name", name, area)
	}
	if ok, err := s.SetSessionTitleArea(ctx, id, "Web", "S2S: Fix the login redirect", histClock); err != nil || ok {
		t.Fatalf("second area = %v, %v; want the first choice kept", ok, err)
	}
	if name, area := sessionTitle(t, s, id); name != "S2S: Fix the login redirect" || area != "S2S" {
		t.Fatalf("after second choice: title = %q area = %q", name, area)
	}
	all, err := s.ListAllSessions(ctx)
	if err != nil || len(all) != 1 || all[0].TitleArea != "S2S" {
		t.Fatalf("list all = %+v, %v; want the area", all, err)
	}
}

// A generated title applied before the area is still AO's, so the late area
// prefixes it; a title applied after the area carries the prefix directly.
func TestSessionTitleAreaPrefixesGeneratedTitlesInEitherOrder(t *testing.T) {
	ctx := context.Background()
	t.Run("title first", func(t *testing.T) {
		s := newTestStore(t)
		id := titledSession(t, s, "")
		if ok, err := s.RenameSessionIfDisplayName(ctx, id, "", "Login redirect", histClock); err != nil || !ok {
			t.Fatalf("generated title = %v, %v", ok, err)
		}
		if ok, err := s.SetSessionTitleArea(ctx, id, "Web", "", histClock); err != nil || !ok {
			t.Fatalf("set area = %v, %v", ok, err)
		}
		if name, _ := sessionTitle(t, s, id); name != "Web: Login redirect" {
			t.Fatalf("title = %q, want the late area on the generated title", name)
		}
	})
	t.Run("area first", func(t *testing.T) {
		s := newTestStore(t)
		id := titledSession(t, s, "fix login")
		if _, err := s.SetSessionTitleArea(ctx, id, "Web", "fix login", histClock); err != nil {
			t.Fatal(err)
		}
		// The provisional name still matches with the prefix it gained.
		if ok, err := s.RenameSessionIfDisplayName(ctx, id, "fix login", "Login redirect", histClock); err != nil || !ok {
			t.Fatalf("generated title = %v, %v", ok, err)
		}
		if name, _ := sessionTitle(t, s, id); name != "Web: Login redirect" {
			t.Fatalf("title = %q, want the prefixed generated title", name)
		}
		// A title that already names the area is not prefixed twice.
		if ok, err := s.RenameSessionIfDisplayName(ctx, id, "Web: Login redirect", "web: Login redirect v2", histClock); err != nil || !ok {
			t.Fatalf("refined title = %v, %v", ok, err)
		}
		if name, _ := sessionTitle(t, s, id); name != "web: Login redirect v2" {
			t.Fatalf("title = %q, want no double prefix", name)
		}
	})
}

// A name a person chose is never prefixed, whether it came before the area or
// before the generated title.
func TestSessionTitleAreaLeavesManualRenameAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := titledSession(t, s, "fix login")
	if _, err := s.RenameSessionIfDisplayName(ctx, id, "fix login", "Login redirect", histClock); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RenameSession(ctx, id, "Mine now", histClock); err != nil || !ok {
		t.Fatalf("rename = %v, %v", ok, err)
	}
	if ok, err := s.SetSessionTitleArea(ctx, id, "Web", "fix login", histClock); err != nil || !ok {
		t.Fatalf("set area = %v, %v; want the area recorded", ok, err)
	}
	if name, area := sessionTitle(t, s, id); name != "Mine now" || area != "Web" {
		t.Fatalf("title = %q area = %q, want the manual name kept", name, area)
	}
	if ok, err := s.RenameSessionIfDisplayName(ctx, id, "fix login", "Late title", histClock); err != nil || ok {
		t.Fatalf("late generated title = %v, %v; want the manual name to win", ok, err)
	}
	if name, _ := sessionTitle(t, s, id); name != "Mine now" {
		t.Fatalf("title = %q, want the manual name kept", name)
	}
}

// A session without an area keeps today's titles.
func TestSessionWithoutTitleAreaKeepsPlainTitles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := titledSession(t, s, "fix login")
	if ok, err := s.RenameSessionIfDisplayName(ctx, id, "fix login", "Login redirect", histClock); err != nil || !ok {
		t.Fatalf("generated title = %v, %v", ok, err)
	}
	if ok, err := s.SetSessionTitleArea(ctx, id, "", "fix login", histClock); err != nil || ok {
		t.Fatalf("empty area = %v, %v; want nothing recorded", ok, err)
	}
	if name, area := sessionTitle(t, s, id); name != "Login redirect" || area != "" {
		t.Fatalf("title = %q area = %q, want plain title", name, area)
	}
}

// Provider titles take the area prefix, and a provider title the late area
// prefixed still counts as AO's own, so the next provider title replaces it.
func TestApplyProviderTitleCarriesTitleArea(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()

	if _, err := s.ApplyProviderTitle(ctx, conversation, session, "First Guess", histClock); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetSessionTitleArea(ctx, session, "TTS", "", histClock); err != nil {
		t.Fatal(err)
	}
	if name, _ := sessionTitle(t, s, session); name != "TTS: First Guess" {
		t.Fatalf("title = %q, want the late area on the provider title", name)
	}
	applied, err := s.ApplyProviderTitle(ctx, conversation, session, "Better Guess", histClock.Add(time.Minute))
	if err != nil || !applied {
		t.Fatalf("second provider title = %v, %v; want it applied", applied, err)
	}
	if name, _ := sessionTitle(t, s, session); name != "TTS: Better Guess" {
		t.Fatalf("title = %q, want the prefixed provider title", name)
	}
	if ok, err := s.RenameSession(ctx, session, "Mine", histClock.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("rename = %v, %v", ok, err)
	}
	if applied, err := s.ApplyProviderTitle(ctx, conversation, session, "Third Guess", histClock.Add(3*time.Minute)); err != nil || applied {
		t.Fatalf("provider title after rename = %v, %v; want the manual name to win", applied, err)
	}
}
