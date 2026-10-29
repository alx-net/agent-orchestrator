package sessionmanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessioncheckout"
)

// checkoutIntoWorkspace gives a fresh workspace an `ao checkout` of a real
// repository (cloned from a bare origin) and returns workspace and source repo.
func checkoutIntoWorkspace(t *testing.T) (workspace, repo string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	empty := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "AO Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "ao@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "AO Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "ao@example.invalid")
	origin, seed, repo := filepath.Join(root, "origin.git"), filepath.Join(root, "seed"), filepath.Join(root, "repo")
	gitRun := func(dir string, args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitRun(root, "init", "--quiet", "--bare", "-b", "main", origin)
	gitRun(root, "clone", "--quiet", origin, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(seed, "add", "README.md")
	gitRun(seed, "commit", "--quiet", "-m", "first")
	gitRun(seed, "push", "--quiet", "origin", "HEAD:main")
	gitRun(root, "clone", "--quiet", origin, repo)
	workspace = filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := sessioncheckout.Checkout(context.Background(), sessioncheckout.Options{RepoPath: repo, Dest: filepath.Join(workspace, sessioncheckout.Dir, "proj")}); err != nil {
		t.Fatal(err)
	}
	return workspace, repo
}

func registeredWorktrees(t *testing.T, repo string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repo, ".git", "worktrees"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(entries)
}

func TestKill_RemovesSessionCheckoutsBeforeWorkspace(t *testing.T) {
	m, st, _, ws := newManager()
	workspace, repo := checkoutIntoWorkspace(t)
	rec := mkLive("mer-1")
	rec.Metadata.WorkspacePath = workspace
	st.sessions["mer-1"] = rec

	freed, err := m.Kill(ctx, "mer-1")
	if err != nil || !freed || ws.destroyed != 1 {
		t.Fatalf("freed=%v err=%v destroyed=%d", freed, err, ws.destroyed)
	}
	if _, err := os.Stat(filepath.Join(workspace, sessioncheckout.Dir)); !os.IsNotExist(err) {
		t.Fatalf("checkouts survived kill: %v", err)
	}
	if n := registeredWorktrees(t, repo); n != 0 {
		t.Fatalf("source repo keeps %d worktree entries", n)
	}
}

func TestKill_DirtyCheckoutPreservesWorkspaceAndTerminates(t *testing.T) {
	m, st, _, ws := newManager()
	workspace, repo := checkoutIntoWorkspace(t)
	scratch := filepath.Join(workspace, sessioncheckout.Dir, "proj", "notes.txt")
	if err := os.WriteFile(scratch, []byte("agent work"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := mkLive("mer-1")
	rec.Metadata.WorkspacePath = workspace
	st.sessions["mer-1"] = rec

	freed, err := m.Kill(ctx, "mer-1")
	if err != nil || freed || ws.destroyed != 0 || !st.sessions["mer-1"].IsTerminated {
		t.Fatalf("freed=%v err=%v destroyed=%d terminated=%v", freed, err, ws.destroyed, st.sessions["mer-1"].IsTerminated)
	}
	if _, err := os.Stat(scratch); err != nil || registeredWorktrees(t, repo) != 1 {
		t.Fatalf("dirty checkout was not preserved: %v", err)
	}

	// Cleanup retries, reports why it skipped, and reclaims once the work is gone.
	res, err := m.Cleanup(ctx, "mer")
	if err != nil || len(res.Skipped) != 1 || res.Skipped[0].Reason != "workspace has uncommitted changes" {
		t.Fatalf("cleanup = %+v, %v", res, err)
	}
	if err := os.Remove(scratch); err != nil {
		t.Fatal(err)
	}
	if res, err = m.Cleanup(ctx, "mer"); err != nil || len(res.Cleaned) != 1 || registeredWorktrees(t, repo) != 0 {
		t.Fatalf("cleanup after removing work = %+v, %v", res, err)
	}
}

const checkoutRule = "When the task needs code from one of these repositories, run `ao checkout <project-id>` and work in the printed path; it is your own up-to-date copy. " +
	"Do not edit the user's checkouts at the listed paths directly; read-only lookups there are fine. " +
	"Don't search the disk, package caches, or the network for this code."

func TestStandalonePromptListsRepositoriesAndCheckoutRule(t *testing.T) {
	got := buildSystemPromptText(systemPromptConfig{
		Role: sessionPromptRoleWorker, Standalone: true,
		Repositories: []promptRepository{
			{ID: "kugelaudio", Name: "KugelAudio", Path: "/Users/alex/conductor/repos/kugelaudio", Remote: "git@github.com:kugelaudio/kugelaudio.git", DefaultBranch: "master"},
			{ID: "notes", Name: "notes", Path: "/tmp/notes"},
		},
		MoreRepositories: 3,
	})
	for _, want := range []string{
		"## Your Repositories",
		checkoutRule,
		"- KugelAudio (id `kugelaudio`): /Users/alex/conductor/repos/kugelaudio, remote git@github.com:kugelaudio/kugelaudio.git, default branch master",
		"- notes (id `notes`): /tmp/notes\n",
		"- 3 more: `ao project ls` lists them.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("standalone prompt missing %q:\n%s", want, got)
		}
	}
	// The standalone framing must not contradict the repository list.
	for _, stale := range []string{"not attached to a project, repository", "Do not invent project context"} {
		if strings.Contains(got, stale) {
			t.Fatalf("standalone prompt still says %q:\n%s", stale, got)
		}
	}
	if !strings.Contains(got, "no orchestrator, assigned branch, issue tracker, PR/MR workflow") {
		t.Fatalf("standalone prompt lost its no-workflow framing:\n%s", got)
	}

	if bare := buildSystemPromptText(systemPromptConfig{Role: sessionPromptRoleWorker, Standalone: true}); strings.Contains(bare, "## Your Repositories") {
		t.Fatalf("prompt without projects lists repositories:\n%s", bare)
	}
	project := buildSystemPromptText(systemPromptConfig{Role: sessionPromptRoleWorker, Project: promptProject{ID: "mer"}, Repositories: []promptRepository{{ID: "x", Name: "x", Path: "/x"}}})
	if strings.Contains(project, "## Your Repositories") {
		t.Fatalf("project worker lists standalone repositories:\n%s", project)
	}
}

// listingStore adds the real store's ListProjects to the fake.
type listingStore struct {
	*fakeStore
	list []domain.ProjectRecord
}

func (s listingStore) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	return s.list, nil
}

func gitProjectDir(t *testing.T, originHead string) string {
	t.Helper()
	dir := t.TempDir()
	refs := filepath.Join(dir, ".git", "refs", "remotes", "origin")
	if err := os.MkdirAll(refs, 0o750); err != nil {
		t.Fatal(err)
	}
	if originHead != "" {
		if err := os.WriteFile(filepath.Join(refs, "HEAD"), []byte("ref: refs/remotes/origin/"+originHead+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSystemPrompt_StandaloneListsEligibleProjectsCapped(t *testing.T) {
	kugel := gitProjectDir(t, "master")
	list := []domain.ProjectRecord{
		{ID: "kugelaudio", DisplayName: "KugelAudio", Path: kugel, RepoOriginURL: "https://github.com/kugelaudio/kugelaudio", Kind: domain.ProjectKindSingleRepo},
		{ID: "scratchy", DisplayName: "Scratchy", Path: gitProjectDir(t, ""), Kind: domain.ProjectKindScratch},
		{ID: "gone", DisplayName: "Gone", Path: filepath.Join(t.TempDir(), "missing")},
	}
	for i := range 25 {
		list = append(list, domain.ProjectRecord{ID: fmt.Sprintf("p%02d", i), Path: gitProjectDir(t, "")})
	}
	st := listingStore{fakeStore: newFakeStore(), list: list}
	m := New(Deps{Runtime: &fakeRuntime{}, Agents: singleAgent{agent: &recordingAgent{}}, Workspace: &fakeWorkspace{}, Store: st, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: st.fakeStore}, DataDir: t.TempDir()})

	sp, err := m.buildSystemPrompt(ctx, domain.KindWorker, "", "standalone-1", "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- KugelAudio (id `kugelaudio`): " + kugel + ", remote https://github.com/kugelaudio/kugelaudio, default branch master",
		"- p18 (id `p18`)",
		"- 6 more: `ao project ls` lists them.", // 26 eligible, 20 listed
	} {
		if !strings.Contains(sp, want) {
			t.Fatalf("standalone prompt missing %q:\n%s", want, sp)
		}
	}
	for _, unwanted := range []string{"Scratchy", "`gone`", "`p19`"} {
		if strings.Contains(sp, unwanted) {
			t.Fatalf("standalone prompt lists %s:\n%s", unwanted, sp)
		}
	}

	project := listingStore{fakeStore: newFakeStore(), list: list}
	project.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: t.TempDir(), Config: testRoleAgents()}
	m = New(Deps{Runtime: &fakeRuntime{}, Agents: singleAgent{agent: &recordingAgent{}}, Workspace: &fakeWorkspace{}, Store: project, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: project.fakeStore}, DataDir: t.TempDir(), LookPath: func(string) (string, error) { return "/bin/true", nil }})
	if sp, err := m.buildSystemPrompt(ctx, domain.KindWorker, "mer", "mer-1", "", false); err != nil || strings.Contains(sp, "## Your Repositories") {
		t.Fatalf("project worker prompt lists repositories (err=%v):\n%s", err, sp)
	}
}
