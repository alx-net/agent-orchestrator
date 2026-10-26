package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// explainRecorder explains each query sqlc issues before running it, so the
// assertions follow the compiled statement rather than a hand-copied duplicate.
type explainRecorder struct {
	gen.DBTX
	t    *testing.T
	plan []string
}

func (r *explainRecorder) record(ctx context.Context, query string, args []any) {
	r.t.Helper()
	rows, err := r.DBTX.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		r.t.Fatalf("explain query plan: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			r.t.Fatal(err)
		}
		r.plan = append(r.plan, detail)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *explainRecorder) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	r.record(ctx, query, args)
	return r.DBTX.QueryContext(ctx, query, args...)
}

func (r *explainRecorder) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.record(ctx, query, args)
	return r.DBTX.QueryRowContext(ctx, query, args...)
}

func (r *explainRecorder) requireIndex(t *testing.T, query, index string) {
	t.Helper()
	plan := strings.Join(r.plan, "\n")
	if !strings.Contains(plan, "INDEX "+index) {
		t.Fatalf("%s does not use %s:\n%s", query, index, plan)
	}
	r.plan = nil
}

// Break caught: the board's batch review-run read probed review_run for a
// newer same-head pass once per row with a full scan, quadratic in the table;
// PR facts matched review runs by github_review_id with full scans; and the
// handoff queue poll scanned every conversation message to find a turn's.
func TestHotPathQueriesUseIndexes(t *testing.T) {
	ctx := context.Background()
	rec := &explainRecorder{DBTX: openMigratedTestDB(t), t: t}
	q := gen.New(rec)

	if _, err := q.ListCurrentHeadReviewRunsBySessions(ctx, `["s-1"]`); err != nil {
		t.Fatal(err)
	}
	rec.requireIndex(t, "ListCurrentHeadReviewRunsBySessions", "idx_review_run_session_head (session_id=? AND pr_url=? AND target_sha=? AND harness=?)")

	if _, err := q.ListPRFactsBySessions(ctx, `["s-1"]`); err != nil {
		t.Fatal(err)
	}
	rec.requireIndex(t, "ListPRFactsBySessions", "idx_review_run_github_review")

	if _, err := q.SelectNextQueuedConversationTurn(ctx, "c-1"); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	rec.requireIndex(t, "SelectNextQueuedConversationTurn", "idx_conversation_messages_turn")
}
