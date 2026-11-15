// Package sessioncheckout gives an AO session its own up-to-date copy of one of
// the user's registered repositories (`ao checkout`): a detached linked
// worktree under the session's workspace, at <workspace>/repos/<project-id>.
//
// Only Git's own worktree metadata is written to the source repository (plus
// the remote-tracking refs a fetch updates); its working tree, branches, and
// index are never touched. RemoveAll undoes the worktrees when the session is
// torn down, and refuses to drop one that holds work.
package sessioncheckout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/gitdefault"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// Dir is the session-workspace subdirectory that holds checkouts.
const Dir = "repos"

// ignoreFile marks a repos/ directory as AO-created. Its "*" also hides every
// checkout from Git when the session workspace is itself a worktree, so an
// agent's `git add -A` there can never stage another repository.
const (
	ignoreFile    = ".gitignore"
	ignoreContent = "# Created by `ao checkout`; removed with the session.\n*\n"
)

// fetchBudget bounds the network fetch. A slow or offline remote falls back to
// the last fetched refs instead of failing the checkout.
const fetchBudget = 3 * time.Minute

// Action says what Checkout did.
type Action string

const (
	// ActionCreated means a new worktree was added.
	ActionCreated Action = "created"
	// ActionUpdated means an existing clean checkout moved to the target.
	ActionUpdated Action = "updated"
	// ActionUnchanged means the existing checkout was already at the target.
	ActionUnchanged Action = "unchanged"
	// ActionKept means the existing checkout holds work and was left alone.
	ActionKept Action = "kept"
)

// ErrHasLocalWork means an existing checkout could not be moved because it
// holds uncommitted changes, a branch, or commits no ref contains.
var ErrHasLocalWork = errors.New("checkout has local work")

// Options describes one checkout.
type Options struct {
	// RepoPath is the user's repository (the project path).
	RepoPath string
	// Dest is the worktree path, <workspace>/repos/<project-id>.
	Dest string
	// DefaultBranch is the project's configured base branch; empty or "auto"
	// uses the remote's HEAD.
	DefaultBranch string
	// Ref overrides the default branch: a remote branch, a tag, or a commit.
	Ref string
	// Strict turns ActionKept into ErrHasLocalWork (`--refresh`).
	Strict bool
}

// Result describes the checkout after the call.
type Result struct {
	Path        string
	Ref         string
	Commit      string
	ShortCommit string
	CommitDate  time.Time
	Subject     string
	Action      Action
	// Note explains ActionKept.
	Note string
	// FetchWarning is set when the fetch failed and cached refs were used.
	FetchWarning string
	// Submodules reports that the repository declares submodules, which are
	// not initialized.
	Submodules bool
}

// Checkout fetches the repository's remote and creates or refreshes the
// detached worktree at opts.Dest.
func Checkout(ctx context.Context, opts Options) (Result, error) {
	repo, dest := opts.RepoPath, opts.Dest
	if err := ValidateRef(opts.Ref); err != nil {
		return Result{}, err
	}
	remote, err := primaryRemote(ctx, repo)
	if err != nil {
		return Result{}, err
	}
	res := Result{Path: dest}
	if remote != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, fetchBudget)
		_, fetchErr := git(fetchCtx, repo, "fetch", "--quiet", "--no-recurse-submodules", remote)
		cancel()
		if fetchErr != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			res.FetchWarning = fmt.Sprintf("git fetch %s failed, so this uses the last fetched refs: %s", remote, firstLine(fetchErr.Error()))
		}
	}
	ref, commit, err := resolveTarget(ctx, repo, remote, opts.DefaultBranch, opts.Ref)
	if err != nil {
		return Result{}, err
	}
	res.Ref = ref

	exists, err := prepareDest(ctx, repo, dest)
	if err != nil {
		return Result{}, err
	}
	if !exists {
		// --force only re-registers a path whose old registration outlived
		// its directory (a crash between add and cleanup). It still refuses an
		// existing non-empty path or a locked registration; see gitworktree's
		// worktreeAddForce.
		if _, err := git(ctx, repo, "worktree", "add", "--force", "--detach", dest, commit); err != nil {
			return Result{}, fmt.Errorf("create checkout: %w", err)
		}
		res.Action = ActionCreated
	} else {
		res.Action, res.Note, err = refresh(ctx, dest, commit)
		if err != nil {
			return Result{}, err
		}
		if res.Action == ActionKept && opts.Strict {
			return Result{}, fmt.Errorf("%w: %s was not updated to %s because %s; commit, discard, or keep using it as is",
				ErrHasLocalWork, dest, ref, res.Note)
		}
	}
	if err := describeHead(ctx, dest, &res); err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(filepath.Join(dest, ".gitmodules")); err == nil {
		res.Submodules = true
	}
	return res, nil
}

// ValidateRef rejects refs that git could read as options or rev expressions.
func ValidateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || strings.ContainsAny(ref, " \t\n\r~^:?*[\\") {
		return fmt.Errorf("invalid ref %q: use a branch, tag, or commit SHA", ref)
	}
	return nil
}

// primaryRemote picks origin, or the only remote; "" means a local-only repo.
func primaryRemote(ctx context.Context, repo string) (string, error) {
	out, err := git(ctx, repo, "remote")
	if err != nil {
		return "", fmt.Errorf("read repository remotes: %w", err)
	}
	remotes := strings.Fields(string(out))
	for _, r := range remotes {
		if r == "origin" {
			return r, nil
		}
	}
	if len(remotes) == 1 {
		return remotes[0], nil
	}
	return "", nil
}

// resolveTarget returns the display ref and commit to check out.
func resolveTarget(ctx context.Context, repo, remote, configured, ref string) (string, string, error) {
	type candidate struct{ ref, display string }
	var candidates []candidate
	switch {
	case ref != "":
		if remote != "" {
			candidates = append(candidates, candidate{"refs/remotes/" + remote + "/" + ref, remote + "/" + ref})
		}
		candidates = append(candidates, candidate{"refs/tags/" + ref, ref}, candidate{ref, ref})
	case configured != "" && configured != "auto":
		if remote != "" && !strings.Contains(configured, "/") {
			candidates = append(candidates, candidate{"refs/remotes/" + remote + "/" + configured, remote + "/" + configured})
		}
		candidates = append(candidates, candidate{configured, configured})
	default:
		resolution, err := defaultBranch(ctx, repo)
		if err != nil {
			return "", "", fmt.Errorf("resolve the default branch: %w; pass --ref <branch>", err)
		}
		display := resolution.Branch
		if resolution.Remote != "" {
			display = resolution.Remote + "/" + resolution.Branch
		}
		candidates = append(candidates, candidate{resolution.Ref, display})
	}
	for _, c := range candidates {
		if sha, ok := commitOf(ctx, repo, c.ref); ok {
			return c.display, sha, nil
		}
	}
	want := ref
	if want == "" {
		want = configured
	}
	return "", "", fmt.Errorf("ref %q was not found in %s after fetching; pass a remote branch, tag, or commit SHA", want, repo)
}

func defaultBranch(ctx context.Context, repo string) (gitdefault.Resolution, error) {
	resolver := gitdefault.New("", nil)
	resolution, err := resolver.Inspect(ctx, repo)
	if err == nil || !errors.Is(err, gitdefault.ErrUnresolved) {
		return resolution, err
	}
	// No cached origin/HEAD (a remote added by hand): ask the remote once.
	remoteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return resolver.Resolve(ctx, remoteCtx, repo)
}

func commitOf(ctx context.Context, repo, ref string) (string, bool) {
	out, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// prepareDest reports whether dest already holds this repository's checkout.
// It creates the AO-owned repos/ directory and refuses a path AO did not make.
func prepareDest(ctx context.Context, repo, dest string) (bool, error) {
	parent := filepath.Dir(dest)
	marker := filepath.Join(parent, ignoreFile)
	if _, err := os.Lstat(parent); err == nil {
		if !hasMarker(parent) {
			return false, fmt.Errorf("%s already exists and was not created by ao checkout", parent)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(parent, 0o750); err != nil {
			return false, fmt.Errorf("create %s: %w", parent, err)
		}
		if err := os.WriteFile(marker, []byte(ignoreContent), 0o600); err != nil {
			return false, fmt.Errorf("mark %s: %w", parent, err)
		}
	} else {
		return false, err
	}

	entries, err := os.ReadDir(dest)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case len(entries) == 0:
		return false, nil
	}
	if !isLinkedWorktree(dest) {
		return false, fmt.Errorf("%s exists and is not an ao checkout; move it away and retry", dest)
	}
	have, err := commonDir(ctx, dest)
	if err != nil {
		return false, err
	}
	want, err := commonDir(ctx, repo)
	if err != nil {
		return false, err
	}
	if !samePath(have, want) {
		return false, fmt.Errorf("%s is a checkout of another repository; move it away and retry", dest)
	}
	return true, nil
}

// refresh moves a clean, detached checkout to commit. Anything that could be
// work (changes, a branch, commits no ref contains) is left alone.
func refresh(ctx context.Context, dest, commit string) (Action, string, error) {
	reason, err := localWork(ctx, dest)
	if err != nil {
		return "", "", err
	}
	if reason == "" {
		// Moving a branch's worktree would mean resetting the agent's branch.
		if branch, err := git(ctx, dest, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			reason = "it is on branch " + strings.TrimSpace(string(branch))
		}
	}
	if reason != "" {
		return ActionKept, reason, nil
	}
	head, err := git(ctx, dest, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("read checkout HEAD: %w", err)
	}
	if strings.TrimSpace(string(head)) == commit {
		return ActionUnchanged, "", nil
	}
	if _, err := git(ctx, dest, "checkout", "--quiet", "--detach", commit); err != nil {
		return "", "", fmt.Errorf("update checkout: %w", err)
	}
	return ActionUpdated, "", nil
}

// localWork returns why removing the checkout at path would lose work
// (uncommitted changes, or commits no branch, tag, or remote ref contains), or
// "" when it would not. Commits on a branch survive in the source repository.
func localWork(ctx context.Context, path string) (string, error) {
	status, err := git(ctx, path, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("read checkout status: %w", err)
	}
	if len(bytes.TrimSpace(status)) > 0 {
		return "it has uncommitted changes", nil
	}
	unreachable, err := git(ctx, path, "rev-list", "-n", "1", "HEAD", "--not", "--branches", "--tags", "--remotes")
	if err != nil {
		return "", fmt.Errorf("check checkout commits: %w", err)
	}
	if len(bytes.TrimSpace(unreachable)) > 0 {
		return "it has commits no branch or tag contains", nil
	}
	return "", nil
}

func describeHead(ctx context.Context, dest string, res *Result) error {
	out, err := git(ctx, dest, "log", "-1", "--format=%H%x1f%h%x1f%cI%x1f%s", "HEAD")
	if err != nil {
		return fmt.Errorf("read checkout commit: %w", err)
	}
	fields := strings.SplitN(strings.TrimRight(string(out), "\n"), "\x1f", 4)
	if len(fields) != 4 {
		return fmt.Errorf("read checkout commit: unexpected output %q", out)
	}
	res.Commit, res.ShortCommit, res.Subject = fields[0], fields[1], fields[3]
	if at, err := time.Parse(time.RFC3339, fields[2]); err == nil {
		res.CommitDate = at
	}
	return nil
}

// RemoveAll removes every checkout under <workspacePath>/repos and prunes the
// source repositories' worktree lists. A checkout holding work, and anything
// else found there, is preserved and reported as ports.ErrWorkspaceDirty so
// the caller keeps the session workspace too. A workspace without checkouts
// costs one stat.
func RemoveAll(ctx context.Context, workspacePath string) error {
	if strings.TrimSpace(workspacePath) == "" || !hasMarker(filepath.Join(workspacePath, Dir)) {
		// No AO-created repos/ directory: nothing of ours to remove.
		return nil
	}
	dir := filepath.Join(workspacePath, Dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list checkouts: %w", err)
	}
	var kept []string
	var errs []error
	for _, entry := range entries {
		if entry.Name() == ignoreFile {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if !entry.IsDir() || !isLinkedWorktree(path) {
			kept = append(kept, entry.Name()+" (not an ao checkout)")
			continue
		}
		reason, err := removeCheckout(ctx, path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if reason != "" {
			kept = append(kept, entry.Name()+" ("+reason+")")
		}
	}
	if len(kept) == 0 && len(errs) == 0 {
		_ = os.Remove(filepath.Join(dir, ignoreFile))
		_ = os.Remove(dir)
		return nil
	}
	if len(kept) > 0 {
		errs = append(errs, fmt.Errorf("preserve checkouts %s: %w", strings.Join(kept, ", "), ports.ErrWorkspaceDirty))
	}
	return errors.Join(errs...)
}

// removeCheckout removes one checkout, or returns why it was preserved.
func removeCheckout(ctx context.Context, path string) (string, error) {
	common, err := commonDir(ctx, path)
	if err != nil {
		return "", fmt.Errorf("checkout %s: %w", path, err)
	}
	reason, err := localWork(ctx, path)
	if err != nil {
		return "", fmt.Errorf("checkout %s: %w", path, err)
	}
	if reason != "" {
		return reason, nil
	}
	// Run from the shared git dir, not from inside the worktree being removed.
	// --force covers ignored build output; localWork already ruled out work.
	if _, err := gitDir(ctx, common, "worktree", "remove", "--force", path); err != nil {
		return "", fmt.Errorf("remove checkout %s: %w", path, err)
	}
	if _, err := gitDir(ctx, common, "worktree", "prune"); err != nil {
		return "", fmt.Errorf("prune worktrees after removing %s: %w", path, err)
	}
	return "", nil
}

// hasMarker reports whether dir is a repos/ directory ao checkout created. Any
// failure to see the marker (missing workspace, a plain file named repos)
// means there is nothing of ours there.
func hasMarker(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ignoreFile))
	return err == nil && info.Mode().IsRegular()
}

// isLinkedWorktree reports whether path's .git is a gitdir pointer file.
func isLinkedWorktree(path string) bool {
	data, err := os.ReadFile(filepath.Join(path, ".git"))
	return err == nil && bytes.HasPrefix(data, []byte("gitdir:"))
}

func commonDir(ctx context.Context, path string) (string, error) {
	out, err := git(ctx, path, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve git dir: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(path, dir)
	}
	return filepath.Clean(dir), nil
}

func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return runGit(ctx, append([]string{"-C", dir}, args...))
}

func gitDir(ctx context.Context, gitDirPath string, args ...string) ([]byte, error) {
	return runGit(ctx, append([]string{"--git-dir=" + gitDirPath}, args...))
}

func runGit(ctx context.Context, args []string) ([]byte, error) {
	cmd := aoprocess.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
