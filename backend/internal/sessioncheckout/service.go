package sessioncheckout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// MaxListedProjects caps the repositories a standalone prompt lists.
const MaxListedProjects = 20

// Eligible reports whether `ao checkout` can copy project: an active, local
// Git project (not an AO scratch project) whose folder is still on disk.
func Eligible(p domain.ProjectRecord) bool {
	if !p.ArchivedAt.IsZero() || p.Kind.WithDefault() == domain.ProjectKindScratch || strings.TrimSpace(p.Path) == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(p.Path, ".git"))
	return err == nil
}

// EligibleProjects filters projects to the ones Checkout accepts, keeping
// their order.
func EligibleProjects(projects []domain.ProjectRecord) []domain.ProjectRecord {
	out := make([]domain.ProjectRecord, 0, len(projects))
	for _, p := range projects {
		if Eligible(p) {
			out = append(out, p)
		}
	}
	return out
}

// DisplayName is the project's name, or its id when unnamed.
func DisplayName(p domain.ProjectRecord) string {
	if name := strings.TrimSpace(p.DisplayName); name != "" {
		return name
	}
	return p.ID
}

// ConfiguredDefaultBranch is the project's explicit base branch, or "" when it
// follows the remote's HEAD.
func ConfiguredDefaultBranch(p domain.ProjectRecord) string {
	branch := strings.TrimSpace(p.Config.DefaultBranch)
	if branch == domain.DefaultBranchAuto {
		return ""
	}
	return branch
}

// CachedDefaultBranch reads the branch origin/HEAD points at without running
// Git, for prompt text only (Checkout resolves it properly). Symbolic refs are
// never packed, so the loose file is the whole answer for the files backend.
func CachedDefaultBranch(p domain.ProjectRecord) string {
	if branch := ConfiguredDefaultBranch(p); branch != "" {
		return branch
	}
	data, err := os.ReadFile(filepath.Join(p.Path, ".git", "refs", "remotes", "origin", "HEAD"))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(data)), "ref: refs/remotes/origin/")
}

// ResolveProject finds query among the eligible projects by id, then by name,
// case-insensitively. Errors name the available projects.
func ResolveProject(projects []domain.ProjectRecord, query string) (domain.ProjectRecord, error) {
	query = strings.TrimSpace(query)
	eligible := EligibleProjects(projects)
	for _, p := range eligible {
		if strings.EqualFold(p.ID, query) {
			return p, nil
		}
	}
	var named []domain.ProjectRecord
	for _, p := range eligible {
		if strings.EqualFold(strings.TrimSpace(p.DisplayName), query) {
			named = append(named, p)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	if len(named) > 1 {
		ids := make([]string, 0, len(named))
		for _, p := range named {
			ids = append(ids, p.ID)
		}
		return domain.ProjectRecord{}, apierr.Invalid("CHECKOUT_PROJECT_AMBIGUOUS",
			fmt.Sprintf("More than one project is named %q; use its id: %s", query, strings.Join(ids, ", ")), nil)
	}
	return domain.ProjectRecord{}, apierr.NotFound("CHECKOUT_PROJECT_NOT_FOUND", notFoundMessage(query, eligible))
}

func notFoundMessage(query string, eligible []domain.ProjectRecord) string {
	if len(eligible) == 0 {
		return fmt.Sprintf("No project %q: AO has no local Git projects to check out. Add one with `ao project add --path <repo>`.", query)
	}
	names := make([]string, 0, len(eligible))
	for _, p := range eligible {
		if name := DisplayName(p); name != p.ID {
			names = append(names, fmt.Sprintf("%s (%s)", p.ID, name))
		} else {
			names = append(names, p.ID)
		}
	}
	sort.Strings(names)
	return fmt.Sprintf("No project %q. Available: %s", query, strings.Join(names, ", "))
}

// SessionReader loads the calling session.
type SessionReader interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
}

// ProjectLister lists the user's active projects.
type ProjectLister interface {
	ListProjects(ctx context.Context) ([]domain.ProjectRecord, error)
}

// Deps wires the service.
type Deps struct {
	Sessions SessionReader
	Projects ProjectLister
	// ManagedRoot is AO's worktree root (<data dir>/worktrees). A session
	// workspace outside it is refused, so checkouts always land under ~/.ao.
	ManagedRoot string
}

// Service is the daemon side of `ao checkout`.
type Service struct {
	deps  Deps
	locks sync.Map // source repo path -> *sync.Mutex
}

// New returns the checkout service.
func New(deps Deps) *Service { return &Service{deps: deps} }

// Input is one `ao checkout` request.
type Input struct {
	SessionID domain.SessionID
	Project   string
	Ref       string
	Refresh   bool
}

// Output is the checkout plus the project it came from.
type Output struct {
	Project domain.ProjectRecord
	Result
}

// Checkout creates or refreshes the session's copy of a project.
func (s *Service) Checkout(ctx context.Context, in Input) (Output, error) {
	if strings.TrimSpace(string(in.SessionID)) == "" {
		return Output{}, apierr.Invalid("CHECKOUT_SESSION_REQUIRED", "A session id is required", nil)
	}
	if strings.TrimSpace(in.Project) == "" {
		return Output{}, apierr.Invalid("CHECKOUT_PROJECT_REQUIRED", "Name the project to check out", nil)
	}
	if err := ValidateRef(strings.TrimSpace(in.Ref)); err != nil {
		return Output{}, apierr.Invalid("CHECKOUT_REF_INVALID", err.Error(), nil)
	}
	rec, ok, err := s.deps.Sessions.GetSession(ctx, in.SessionID)
	if err != nil {
		return Output{}, err
	}
	if !ok {
		return Output{}, apierr.NotFound("SESSION_NOT_FOUND", fmt.Sprintf("Session %s not found", in.SessionID))
	}
	if rec.IsTerminated {
		return Output{}, apierr.Conflict("CHECKOUT_SESSION_TERMINATED", "The session is terminated", nil)
	}
	workspace, err := s.sessionWorkspace(rec)
	if err != nil {
		return Output{}, err
	}
	projects, err := s.deps.Projects.ListProjects(ctx)
	if err != nil {
		return Output{}, err
	}
	project, err := ResolveProject(projects, in.Project)
	if err != nil {
		return Output{}, err
	}
	if strings.ContainsAny(project.ID, `/\`) || project.ID == "." || project.ID == ".." {
		return Output{}, apierr.Invalid("CHECKOUT_PROJECT_ID_INVALID", fmt.Sprintf("Project id %q cannot name a directory", project.ID), nil)
	}

	unlock := s.lock(project.Path)
	defer unlock()
	res, err := Checkout(ctx, Options{
		RepoPath:      project.Path,
		Dest:          filepath.Join(workspace, Dir, project.ID),
		DefaultBranch: ConfiguredDefaultBranch(project),
		Ref:           strings.TrimSpace(in.Ref),
		Strict:        in.Refresh,
	})
	if err != nil {
		if errors.Is(err, ErrHasLocalWork) {
			return Output{}, apierr.Conflict("CHECKOUT_HAS_LOCAL_WORK", strings.TrimPrefix(err.Error(), ErrHasLocalWork.Error()+": "), nil)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Output{}, ctxErr
		}
		return Output{}, apierr.Invalid("CHECKOUT_FAILED", err.Error(), nil)
	}
	return Output{Project: project, Result: res}, nil
}

// sessionWorkspace returns the session's workspace after checking it lies
// under AO's managed root.
func (s *Service) sessionWorkspace(rec domain.SessionRecord) (string, error) {
	path := strings.TrimSpace(rec.Metadata.WorkspacePath)
	if path == "" {
		return "", apierr.Conflict("CHECKOUT_NO_WORKSPACE", "The session has no workspace yet", nil)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", apierr.Conflict("CHECKOUT_NO_WORKSPACE", "The session's workspace is missing", nil)
	}
	root := s.deps.ManagedRoot
	if root == "" {
		return "", apierr.Internal("CHECKOUT_UNCONFIGURED", "The checkout root is not configured")
	}
	physicalRoot, rootErr := filepath.EvalSymlinks(root)
	physicalPath, pathErr := filepath.EvalSymlinks(path)
	if rootErr != nil || pathErr != nil {
		return "", apierr.Conflict("CHECKOUT_NO_WORKSPACE", "The session's workspace cannot be resolved", nil)
	}
	rel, err := filepath.Rel(physicalRoot, physicalPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", apierr.Conflict("CHECKOUT_WORKSPACE_UNMANAGED", "The session's workspace is not managed by AO", nil)
	}
	return physicalPath, nil
}

// lock serializes checkouts of one source repository: concurrent fetches and
// worktree adds on the same repo would fight over its ref and worktree locks.
func (s *Service) lock(repo string) func() {
	v, _ := s.locks.LoadOrStore(filepath.Clean(repo), &sync.Mutex{})
	mu, _ := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
