package sqlite

import (
	"database/sql"
	"testing"
)

// Existing sessions arrive with no area and no automatic-title witness, a
// chosen area publishes a session change, and the migration rolls back and
// re-applies cleanly.
func TestMigrateSessionTitleAreaRoundTrip(t *testing.T) {
	const (
		before = 20261026201227332
		area   = 20261027125731394
	)
	db := openMigratedDatabaseCopy(t, before)
	mustExec(t, db, `INSERT INTO projects (id, path, registered_at) VALUES ('project', '/tmp/project', CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO sessions (id, project_id, num, display_name, activity_last_at, created_at, updated_at)
		VALUES ('project-1', 'project', 1, 'Fix login', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)

	upTo(t, db, area)
	var titleArea sql.NullString
	var autoTitle string
	if err := db.QueryRow(`SELECT title_area, auto_title FROM sessions WHERE id = 'project-1'`).Scan(&titleArea, &autoTitle); err != nil || titleArea.Valid || autoTitle != "" {
		t.Fatalf("existing session title_area = %+v auto_title = %q, %v; want NULL and empty", titleArea, autoTitle, err)
	}
	events := countChangeLog(t, db, "session_updated")
	mustExec(t, db, `UPDATE sessions SET title_area = 'S2S' WHERE id = 'project-1'`)
	if got := countChangeLog(t, db, "session_updated"); got != events+1 {
		t.Fatalf("session_updated events after choosing an area = %d, want %d", got, events+1)
	}
	mustExec(t, db, `UPDATE sessions SET auto_title = 'Fix login' WHERE id = 'project-1'`)
	if got := countChangeLog(t, db, "session_updated"); got != events+1 {
		t.Fatalf("the internal title witness published a session change (%d)", got)
	}

	downTo(t, db, before)
	for _, column := range []string{"title_area", "auto_title"} {
		if _, err := db.Exec(`SELECT ` + column + ` FROM sessions`); err == nil {
			t.Fatalf("%s survived the rollback", column)
		}
	}
	var name string
	if err := db.QueryRow(`SELECT display_name FROM sessions WHERE id = 'project-1'`).Scan(&name); err != nil || name != "Fix login" {
		t.Fatalf("session after rollback = %q, %v; want it intact", name, err)
	}
	events = countChangeLog(t, db, "session_updated")
	mustExec(t, db, `UPDATE sessions SET display_name = 'Renamed' WHERE id = 'project-1'`)
	if got := countChangeLog(t, db, "session_updated"); got != events+1 {
		t.Fatalf("restored trigger did not publish a rename (%d)", got)
	}
	upTo(t, db, area)
}
