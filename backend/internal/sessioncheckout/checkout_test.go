package sessioncheckout

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// fixture is a bare "origin", the user's clone of it (the project path), and a
// second clone that pushes new upstream commits.
type fixture struct {
	origin, user, pusher, workspace string
}

func newFixture(t *testing.T) fixture {
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
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "AO Test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "ao@example.invalid")
	}
	f := fixture{
		origin:    filepath.Join(root, "origin.git"),
		user:      filepath.Join(root, "user"),
		pusher:    filepath.Join(root, "pusher"),
		workspace: filepath.Join(root, "ws"),
	}
	run(t, root, "init", "--quiet", "--bare", "-b", "master", f.origin)
	run(t, root, "clone", "--quiet", f.origin, f.pusher)
	commit(t, f.pusher, ".gitignore", "*.log\n", "ignore logs")
	commit(t, f.pusher, "README.md", "v1\n", "first")
	run(t, f.pusher, "push", "--quiet", "origin", "HEAD:master")
	run(t, root, "clone", "--quiet", f.origin, f.user)
	if err := os.MkdirAll(f.workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) dest() string { return filepath.Join(f.workspace, Dir, "proj") }

func (f fixture) push(t *testing.T, content, msg string) string {
	t.Helper()
	commit(t, f.pusher, "README.md", content, msg)
	run(t, f.pusher, "push", "--quiet", "origin", "HEAD:master")
	return strings.TrimSpace(run(t, f.pusher, "rev-parse", "HEAD"))
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func commit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", file)
	run(t, dir, "commit", "--quiet", "-m", msg)
}

func head(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(run(t, dir, "rev-parse", "HEAD"))
}

func TestCheckoutCreatesDetachedWorktreeAtFetchedDefaultBranch(t *testing.T) {
	f := newFixture(t)
	// The user's own checkout has local work that must stay exactly as is.
	commit(t, f.user, "local.txt", "mine\n", "local only")
	if err := os.WriteFile(filepath.Join(f.user, "README.md"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	userHead, userStatus := head(t, f.user), run(t, f.user, "status", "--porcelain")
	userBranches := run(t, f.user, "for-each-ref", "refs/heads")
	upstream := f.push(t, "v2\n", "second") // only reachable through a fetch

	res, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ActionCreated || res.Ref != "origin/master" || res.Commit != upstream || res.Subject != "second" || res.CommitDate.IsZero() || res.ShortCommit == "" {
		t.Fatalf("result = %+v, want created at origin/master %s", res, upstream)
	}
	if got := head(t, f.dest()); got != upstream {
		t.Fatalf("checkout HEAD = %s, want %s", got, upstream)
	}
	if _, err := exec.Command("git", "-C", f.dest(), "symbolic-ref", "-q", "HEAD").Output(); err == nil {
		t.Fatal("checkout HEAD is attached to a branch, want detached")
	}
	if head(t, f.user) != userHead || run(t, f.user, "status", "--porcelain") != userStatus || run(t, f.user, "for-each-ref", "refs/heads") != userBranches {
		t.Fatal("the user's working tree, index, or branches changed")
	}
}

func TestCheckoutStaysInvisibleToAnEnclosingWorktree(t *testing.T) {
	f := newFixture(t)
	// A project session's workspace is itself a Git worktree.
	run(t, f.workspace, "init", "--quiet", "-b", "main")
	commit(t, f.workspace, "app.txt", "app\n", "app")
	if _, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, f.workspace, "status", "--porcelain"); got != "" {
		t.Fatalf("enclosing worktree sees the checkout: %q", got)
	}
}

func TestCheckoutRefreshMovesCleanCheckoutAndKeepsDirtyOne(t *testing.T) {
	f := newFixture(t)
	first, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()})
	if err != nil || again.Action != ActionUnchanged || again.Commit != first.Commit {
		t.Fatalf("rerun = %+v, %v; want unchanged", again, err)
	}

	second := f.push(t, "v2\n", "second")
	moved, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()})
	if err != nil || moved.Action != ActionUpdated || head(t, f.dest()) != second {
		t.Fatalf("refresh = %+v, %v; want updated to %s", moved, err, second)
	}

	if err := os.WriteFile(filepath.Join(f.dest(), "README.md"), []byte("agent edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.push(t, "v3\n", "third")
	kept, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()})
	if err != nil || kept.Action != ActionKept || !strings.Contains(kept.Note, "uncommitted changes") || head(t, f.dest()) != second {
		t.Fatalf("dirty rerun = %+v, %v; want kept at %s", kept, err, second)
	}
	if data, _ := os.ReadFile(filepath.Join(f.dest(), "README.md")); string(data) != "agent edit\n" {
		t.Fatalf("dirty file changed: %q", data)
	}
	if _, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest(), Strict: true}); !errors.Is(err, ErrHasLocalWork) {
		t.Fatalf("strict dirty rerun err = %v, want ErrHasLocalWork", err)
	}
}

func TestCheckoutRefSelectsBranchTagOrCommit(t *testing.T) {
	f := newFixture(t)
	base := head(t, f.pusher)
	run(t, f.pusher, "switch", "--quiet", "-c", "feature")
	commit(t, f.pusher, "feature.txt", "f\n", "feature work")
	feature := head(t, f.pusher)
	run(t, f.pusher, "push", "--quiet", "origin", "feature")
	run(t, f.pusher, "tag", "v1", base)
	run(t, f.pusher, "push", "--quiet", "origin", "v1")

	for _, tc := range []struct{ ref, wantRef, wantCommit string }{
		{"feature", "origin/feature", feature},
		{"v1", "v1", base},
		{base[:10], base[:10], base},
	} {
		res, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest(), Ref: tc.ref})
		if err != nil {
			t.Fatalf("ref %s: %v", tc.ref, err)
		}
		if res.Ref != tc.wantRef || res.Commit != tc.wantCommit {
			t.Fatalf("ref %s: got %s@%s, want %s@%s", tc.ref, res.Ref, res.Commit, tc.wantRef, tc.wantCommit)
		}
	}
	if _, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest(), Ref: "no-such-branch"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing ref err = %v", err)
	}
	if _, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest(), Ref: "--upload-pack=evil"}); err == nil {
		t.Fatal("option-like ref accepted")
	}
}

func TestCheckoutUsesConfiguredDefaultBranch(t *testing.T) {
	f := newFixture(t)
	run(t, f.pusher, "switch", "--quiet", "-c", "develop")
	commit(t, f.pusher, "dev.txt", "d\n", "develop work")
	develop := head(t, f.pusher)
	run(t, f.pusher, "push", "--quiet", "origin", "develop")

	res, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest(), DefaultBranch: "develop"})
	if err != nil || res.Ref != "origin/develop" || res.Commit != develop {
		t.Fatalf("configured default = %+v, %v; want origin/develop@%s", res, err, develop)
	}
}

func TestCheckoutRefusesForeignDirectories(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.workspace, Dir), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()}); err == nil || !strings.Contains(err.Error(), "not created by ao checkout") {
		t.Fatalf("pre-existing repos/ err = %v", err)
	}
}

func TestRemoveAllRemovesCleanCheckoutsAndPrunes(t *testing.T) {
	f := newFixture(t)
	if _, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()}); err != nil {
		t.Fatal(err)
	}
	// Ignored build output must not block removal.
	if err := os.WriteFile(filepath.Join(f.dest(), "build.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAll(context.Background(), f.workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repos/ still present: %v", err)
	}
	if list := run(t, f.user, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("source repo still lists checkouts:\n%s", list)
	}
	entries, _ := os.ReadDir(filepath.Join(f.user, ".git", "worktrees"))
	if len(entries) != 0 {
		t.Fatalf("stale .git/worktrees entries: %v", entries)
	}
	// Idempotent and cheap on a workspace without checkouts.
	if err := RemoveAll(context.Background(), f.workspace); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveAllPreservesCheckoutsWithWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		work func(t *testing.T, dest string)
	}{
		{"uncommitted change", func(t *testing.T, dest string) {
			if err := os.WriteFile(filepath.Join(dest, "new.txt"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"detached commit", func(t *testing.T, dest string) { commit(t, dest, "new.txt", "x", "agent commit") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()}); err != nil {
				t.Fatal(err)
			}
			tc.work(t, f.dest())
			err := RemoveAll(context.Background(), f.workspace)
			if !errors.Is(err, ports.ErrWorkspaceDirty) {
				t.Fatalf("err = %v, want ErrWorkspaceDirty", err)
			}
			if _, err := os.Stat(filepath.Join(f.dest(), "new.txt")); err != nil {
				t.Fatalf("work lost: %v", err)
			}
		})
	}
}

func TestRemoveAllRemovesCheckoutOnAgentBranch(t *testing.T) {
	f := newFixture(t)
	if _, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()}); err != nil {
		t.Fatal(err)
	}
	run(t, f.dest(), "switch", "--quiet", "-c", "agent-fix")
	commit(t, f.dest(), "fix.txt", "x", "fix")
	fix := head(t, f.dest())
	f.push(t, "v2\n", "second")
	if res, err := Checkout(context.Background(), Options{RepoPath: f.user, Dest: f.dest()}); err != nil || res.Action != ActionKept || !strings.Contains(res.Note, "on branch agent-fix") || head(t, f.dest()) != fix {
		t.Fatalf("rerun on a branch = %+v, %v; want kept", res, err)
	}
	// Committed branch work lives in the source repo, so the worktree can go.
	if err := RemoveAll(context.Background(), f.workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.dest()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkout still present: %v", err)
	}
	if got := strings.TrimSpace(run(t, f.user, "rev-parse", "refs/heads/agent-fix")); got != fix {
		t.Fatalf("agent branch = %s, want %s", got, fix)
	}
}
