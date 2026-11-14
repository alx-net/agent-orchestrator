package session

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type titleAreaCall struct {
	id                domain.SessionID
	area, provisional string
}

var testTitleAreas = []domain.TitleArea{
	{Name: "S2S", Hint: "S2S v1, speech-to-speech"},
	{Name: "Clarity"},
	{Name: "Web"},
}

func TestTitleAreaSystemPromptListsAreasAndHints(t *testing.T) {
	prompt := titleAreaSystemPrompt(testTitleAreas)
	for _, want := range []string{`"S2S": S2S v1, speech-to-speech` + "\n", `"Clarity"` + "\n", `"Web"` + "\n", "NONE", "Do not use tools"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// A new task in a project with title areas asks the worker's harness for one
// area, once, and records only an answer that names a configured area.
func TestDelegateTaskChoosesTitleArea(t *testing.T) {
	brief := "Cut S2S latency"
	tests := []struct {
		name      string
		areas     []domain.TitleArea
		answer    string
		wantCalls int
		wantArea  string
		wantName  string
	}{
		{name: "exact", areas: testTitleAreas, answer: "S2S", wantCalls: 1, wantArea: "S2S", wantName: "S2S: Cut S2S latency"},
		{name: "case-insensitive", areas: testTitleAreas, answer: " s2s\n", wantCalls: 1, wantArea: "S2S", wantName: "S2S: Cut S2S latency"},
		{name: "none", areas: testTitleAreas, answer: "NONE", wantCalls: 1, wantName: brief},
		{name: "garbage", areas: testTitleAreas, answer: "It is the speech product", wantCalls: 1, wantName: brief},
		{name: "project without areas", answer: "S2S", wantName: brief},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore()
			st.projects["ka"] = domain.ProjectRecord{ID: "ka", Config: domain.ProjectConfig{TitleAreas: tt.areas}}
			cmd := &fakeCommander{backgroundResult: tt.answer}
			cmd.spawnFunc = func(cfg ports.SpawnConfig) domain.SessionRecord {
				rec := domain.SessionRecord{ID: "ka-1", ProjectID: cfg.ProjectID, Kind: cfg.Kind, DisplayName: cfg.DisplayName}
				st.sessions[rec.ID] = rec
				return rec
			}
			svc := &Service{store: st, manager: cmd, runBackground: runInline}

			if _, err := svc.DelegateTask(context.Background(), DelegateTaskInput{ProjectID: "ka", Brief: brief}); err != nil {
				t.Fatalf("DelegateTask: %v", err)
			}
			if len(cmd.backgroundCalls) != tt.wantCalls {
				t.Fatalf("background calls = %#v, want %d", cmd.backgroundCalls, tt.wantCalls)
			}
			if tt.wantCalls > 0 {
				call := cmd.backgroundCalls[0]
				if call.id != "ka-1" || call.prompt != brief || call.systemPrompt != titleAreaSystemPrompt(tt.areas) {
					t.Fatalf("background call = %#v", call)
				}
			}
			got := st.sessions["ka-1"]
			if got.TitleArea != tt.wantArea || got.DisplayName != tt.wantName {
				t.Fatalf("session area = %q name = %q, want %q / %q", got.TitleArea, got.DisplayName, tt.wantArea, tt.wantName)
			}
			if tt.wantArea != "" && !reflect.DeepEqual(st.titleAreaCalls, []titleAreaCall{{id: "ka-1", area: tt.wantArea, provisional: brief}}) {
				t.Fatalf("title area calls = %#v", st.titleAreaCalls)
			}
			if tt.wantArea == "" && len(st.titleAreaCalls) != 0 {
				t.Fatalf("title area recorded for %q: %#v", tt.answer, st.titleAreaCalls)
			}
		})
	}
}

// A Scratch task refines its title first, then chooses the area, in the same
// background slot.
func TestDelegateTaskScratchRefinesTitleThenChoosesArea(t *testing.T) {
	st := newFakeStore()
	st.projects["ka"] = domain.ProjectRecord{ID: "ka", Kind: domain.ProjectKindScratch, Config: domain.ProjectConfig{TitleAreas: testTitleAreas}}
	cmd := &fakeCommander{backgroundFunc: func(call backgroundTaskCall) (string, error) {
		if call.systemPrompt == delegatedTaskTitleSystemPrompt {
			return "Fix renderer", nil
		}
		return "web", nil
	}}
	cmd.spawnFunc = func(cfg ports.SpawnConfig) domain.SessionRecord {
		rec := domain.SessionRecord{ID: "ka-1", ProjectID: cfg.ProjectID, Kind: cfg.Kind, DisplayName: cfg.DisplayName}
		st.sessions[rec.ID] = rec
		return rec
	}
	svc := &Service{store: st, manager: cmd, runBackground: runInline}

	if _, err := svc.DelegateTask(context.Background(), DelegateTaskInput{ProjectID: "ka", Brief: "Fix the renderer"}); err != nil {
		t.Fatalf("DelegateTask: %v", err)
	}
	if len(cmd.backgroundCalls) != 2 || cmd.backgroundCalls[0].systemPrompt != delegatedTaskTitleSystemPrompt || cmd.backgroundCalls[1].systemPrompt != titleAreaSystemPrompt(testTitleAreas) {
		t.Fatalf("background calls = %#v, want title then area", cmd.backgroundCalls)
	}
	if !reflect.DeepEqual(st.titleAreaCalls, []titleAreaCall{{id: "ka-1", area: "Web", provisional: "Fix the renderer"}}) {
		t.Fatalf("title area calls = %#v", st.titleAreaCalls)
	}
	if got := st.sessions["ka-1"]; got.TitleArea != "Web" || got.DisplayName != "Fix renderer" {
		t.Fatalf("session = %q / %q; the store, not the service, prefixes a generated title", got.TitleArea, got.DisplayName)
	}
}

// Generic spawns (`ao spawn`, automations, intake) choose an area for workers
// with a task; the spawn-time name is the provisional title.
func TestSpawnChoosesTitleAreaForWorkersWithTasks(t *testing.T) {
	tests := []struct {
		name      string
		cfg       ports.SpawnConfig
		wantCalls []titleAreaCall
	}{
		{
			name:      "named worker",
			cfg:       ports.SpawnConfig{ProjectID: "ka", Kind: domain.KindWorker, DisplayName: "Review login", Prompt: "Review the web login"},
			wantCalls: []titleAreaCall{{id: "ka-1", area: "Web", provisional: "Review login"}},
		},
		{name: "no task", cfg: ports.SpawnConfig{ProjectID: "ka", Kind: domain.KindWorker, DisplayName: "Idle", Prompt: "  "}},
		{name: "orchestrator", cfg: ports.SpawnConfig{ProjectID: "ka", Kind: domain.KindOrchestrator, Prompt: "Coordinate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore()
			st.projects["ka"] = domain.ProjectRecord{ID: "ka", Config: domain.ProjectConfig{TitleAreas: testTitleAreas}}
			cmd := &fakeCommander{backgroundResult: "Web"}
			cmd.spawnFunc = func(cfg ports.SpawnConfig) domain.SessionRecord {
				rec := domain.SessionRecord{ID: "ka-1", ProjectID: cfg.ProjectID, Kind: cfg.Kind, DisplayName: cfg.DisplayName}
				st.sessions[rec.ID] = rec
				return rec
			}
			svc := &Service{store: st, manager: cmd, runBackground: runInline}

			if _, _, _, err := svc.Spawn(context.Background(), tt.cfg); err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if !reflect.DeepEqual(st.titleAreaCalls, tt.wantCalls) {
				t.Fatalf("title area calls = %#v, want %#v", st.titleAreaCalls, tt.wantCalls)
			}
			if len(tt.wantCalls) == 0 && len(cmd.backgroundCalls) != 0 {
				t.Fatalf("background calls = %#v, want none", cmd.backgroundCalls)
			}
		})
	}
}
