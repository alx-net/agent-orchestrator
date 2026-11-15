package sqlite

import (
	"database/sql"
	"testing"
)

// The parent column arrives NULL for existing sessions with its index, and the
// migration rolls back and re-applies cleanly.
func TestMigrateSessionParentRoundTrip(t *testing.T) {
	const (
		before = 20261026184509503
		parent = 20261026201227332
	)
	db := openMigratedDatabaseCopy(t, before)
	mustExec(t, db, `INSERT INTO projects (id, path, registered_at) VALUES ('project', '/tmp/project', CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO sessions (id, project_id, num, activity_last_at, created_at, updated_at)
		VALUES ('project-1', 'project', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)

	upTo(t, db, parent)
	var existing sql.NullString
	if err := db.QueryRow(`SELECT parent_session_id FROM sessions WHERE id = 'project-1'`).Scan(&existing); err != nil || existing.Valid {
		t.Fatalf("existing session parent = %+v, %v; want NULL", existing, err)
	}
	if got := schemaObjects(t, db, "index", "idx_sessions_parent"); got == "" {
		t.Fatal("idx_sessions_parent missing after migration")
	}
	mustExec(t, db, `INSERT INTO sessions (id, project_id, num, activity_last_at, created_at, updated_at, parent_session_id)
		VALUES ('project-2', 'project', 2, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'project-1')`)

	downTo(t, db, before)
	if _, err := db.Exec(`SELECT parent_session_id FROM sessions`); err == nil {
		t.Fatal("parent_session_id survived the rollback")
	}
	if got := schemaObjects(t, db, "index", "idx_sessions_parent"); got != "" {
		t.Fatalf("idx_sessions_parent survived the rollback: %s", got)
	}
	var sessions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 2 {
		t.Fatalf("sessions after rollback = %d, %v; want 2", sessions, err)
	}
	upTo(t, db, parent)
}
