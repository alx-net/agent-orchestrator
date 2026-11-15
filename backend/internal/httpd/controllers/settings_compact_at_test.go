package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	settingssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/settings"
)

type compactAtSettingsService struct {
	hibernationSettingsService
	tokens int
}

func (s *compactAtSettingsService) SetCompactAt(_ context.Context, tokens int) (settingssvc.Snapshot, error) {
	if tokens == 1 {
		return settingssvc.Snapshot{}, fmt.Errorf("%w: out of range", settingssvc.ErrInvalidCompactAt)
	}
	s.tokens = tokens
	s.calls++
	return settingssvc.Snapshot{CompactAt: tokens}, nil
}

func TestSettingsCompactAtRequiresTokenCount(t *testing.T) {
	svc := &compactAtSettingsService{tokens: 350_000}
	c := &SettingsController{Svc: svc}
	for _, body := range []string{`{}`, `{"tokens":null}`, `{"tokens":"350000"}`, `{"tokens":1}`} {
		w := httptest.NewRecorder()
		c.setCompactAt(w, httptest.NewRequest(http.MethodPatch, "/api/v1/settings/compact-at", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest || svc.calls != 0 {
			t.Fatalf("body=%s status=%d calls=%d response=%s", body, w.Code, svc.calls, w.Body.String())
		}
		if body == `{"tokens":1}` && !strings.Contains(w.Body.String(), "COMPACT_AT_INVALID") {
			t.Fatalf("out-of-range size answered %s; want COMPACT_AT_INVALID", w.Body.String())
		}
	}
	for _, tokens := range []int{0, 500_000} {
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"tokens":%d}`, tokens)
		c.setCompactAt(w, httptest.NewRequest(http.MethodPatch, "/api/v1/settings/compact-at", strings.NewReader(body)))
		var got SettingsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || got.CompactAt != tokens || svc.tokens != tokens {
			t.Fatalf("body=%s status=%d response=%+v err=%v", body, w.Code, got, err)
		}
	}
}
