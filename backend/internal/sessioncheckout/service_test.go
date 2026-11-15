package sessioncheckout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

func gitProject(t *testing.T, id, name string) domain.ProjectRecord {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	return domain.ProjectRecord{ID: id, DisplayName: name, Path: dir, Kind: domain.ProjectKindSingleRepo}
}

func apiCode(err error) string {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestResolveProjectMatchesIDThenNameCaseInsensitively(t *testing.T) {
	kugel := gitProject(t, "kugelaudio", "KugelAudio")
	web := gitProject(t, "web", "Website")
	scratch := domain.ProjectRecord{ID: "notes", DisplayName: "Notes", Path: t.TempDir(), Kind: domain.ProjectKindScratch}
	archived := gitProject(t, "old", "Old")
	archived.ArchivedAt = time.Now()
	gone := domain.ProjectRecord{ID: "gone", Path: filepath.Join(t.TempDir(), "missing")}
	projects := []domain.ProjectRecord{kugel, web, scratch, archived, gone}

	for _, query := range []string{"kugelaudio", "KUGELAUDIO", "KugelAudio", " kugelaudio "} {
		got, err := ResolveProject(projects, query)
		if err != nil || got.ID != "kugelaudio" {
			t.Fatalf("%q -> %q, %v", query, got.ID, err)
		}
	}
	if got, err := ResolveProject(projects, "website"); err != nil || got.ID != "web" {
		t.Fatalf("by name -> %q, %v", got.ID, err)
	}
	for _, query := range []string{"notes", "old", "gone", "curb"} {
		_, err := ResolveProject(projects, query)
		if apiCode(err) != "CHECKOUT_PROJECT_NOT_FOUND" {
			t.Fatalf("%q: err = %v", query, err)
		}
		if msg := err.Error(); !strings.HasSuffix(msg, "Available: kugelaudio (KugelAudio), web (Website)") {
			t.Fatalf("%q: message %q does not list exactly the eligible projects", query, msg)
		}
	}
}

func TestResolveProjectRejectsAmbiguousNames(t *testing.T) {
	a, b := gitProject(t, "api-1", "API"), gitProject(t, "api-2", "api")
	_, err := ResolveProject([]domain.ProjectRecord{a, b}, "Api")
	if apiCode(err) != "CHECKOUT_PROJECT_AMBIGUOUS" || !strings.Contains(err.Error(), "api-1, api-2") {
		t.Fatalf("err = %v", err)
	}
}

type fakeSessions map[domain.SessionID]domain.SessionRecord

func (f fakeSessions) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok := f[id]
	return rec, ok, nil
}

type fakeProjects []domain.ProjectRecord

func (f fakeProjects) ListProjects(context.Context) ([]domain.ProjectRecord, error) { return f, nil }

func TestServiceCheckoutIntoSessionWorkspace(t *testing.T) {
	f := newFixture(t)
	root := filepath.Dir(f.workspace)
	outside := t.TempDir()
	project := domain.ProjectRecord{ID: "proj", DisplayName: "Proj", Path: f.user, Kind: domain.ProjectKindSingleRepo}
	svc := New(Deps{
		Sessions: fakeSessions{
			"sa-1":   {ID: "sa-1", Metadata: domain.SessionMetadata{WorkspacePath: f.workspace}},
			"dead":   {ID: "dead", IsTerminated: true, Metadata: domain.SessionMetadata{WorkspacePath: f.workspace}},
			"rogue":  {ID: "rogue", Metadata: domain.SessionMetadata{WorkspacePath: outside}},
			"nowork": {ID: "nowork"},
		},
		Projects:    fakeProjects{project},
		ManagedRoot: root,
	})

	out, err := svc.Checkout(context.Background(), Input{SessionID: "sa-1", Project: "PROJ"})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(f.workspace)
	if out.Project.ID != "proj" || out.Path != filepath.Join(want, Dir, "proj") || out.Action != ActionCreated {
		t.Fatalf("output = %+v", out)
	}

	for _, tc := range []struct {
		in   Input
		code string
	}{
		{Input{SessionID: "sa-1"}, "CHECKOUT_PROJECT_REQUIRED"},
		{Input{SessionID: "sa-1", Project: "proj", Ref: "-x"}, "CHECKOUT_REF_INVALID"},
		{Input{SessionID: "missing", Project: "proj"}, "SESSION_NOT_FOUND"},
		{Input{SessionID: "dead", Project: "proj"}, "CHECKOUT_SESSION_TERMINATED"},
		{Input{SessionID: "rogue", Project: "proj"}, "CHECKOUT_WORKSPACE_UNMANAGED"},
		{Input{SessionID: "nowork", Project: "proj"}, "CHECKOUT_NO_WORKSPACE"},
		{Input{SessionID: "sa-1", Project: "curb"}, "CHECKOUT_PROJECT_NOT_FOUND"},
	} {
		if _, err := svc.Checkout(context.Background(), tc.in); apiCode(err) != tc.code {
			t.Fatalf("%+v: err = %v, want %s", tc.in, err, tc.code)
		}
	}

	if err := os.WriteFile(filepath.Join(out.Path, "README.md"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Checkout(context.Background(), Input{SessionID: "sa-1", Project: "proj", Refresh: true}); apiCode(err) != "CHECKOUT_HAS_LOCAL_WORK" {
		t.Fatalf("refresh of dirty checkout err = %v", err)
	}
}

func TestCachedDefaultBranchReadsOriginHead(t *testing.T) {
	f := newFixture(t)
	p := domain.ProjectRecord{ID: "proj", Path: f.user}
	if got := CachedDefaultBranch(p); got != "master" {
		t.Fatalf("cached default = %q, want master", got)
	}
	p.Config.DefaultBranch = "develop"
	if got := CachedDefaultBranch(p); got != "develop" {
		t.Fatalf("configured default = %q, want develop", got)
	}
}
