package sheetsql

import (
	"database/sql"
	"errors"
	"os"
	"testing"
)

func ridDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SHEETSQL_DSN")
	if dsn == "" {
		t.Skip("set SHEETSQL_DSN to run live tests")
	}
	db, err := sql.Open("sheets", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		afterScan = nil
		db.Close()
	})
	return db
}

// TestLiveRIDHiddenFromStar checks that the identity column does not leak into
// query results.
func TestLiveRIDHiddenFromStar(t *testing.T) {
	db := ridDB(t)
	rows, err := db.Query("SELECT * FROM employees LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		if c == RIDColumn {
			t.Fatalf("SELECT * exposed %s: %v", RIDColumn, cols)
		}
	}
	if len(cols) != 6 {
		t.Errorf("got %d columns %v, want the 6 visible ones", len(cols), cols)
	}
}

// TestLiveRIDAutoAssigned checks that INSERT mints an id without being asked.
func TestLiveRIDAutoAssigned(t *testing.T) {
	db := ridDB(t)
	const probe = "Zz Rid Probe"
	db.Exec("DELETE FROM employees WHERE name = ?", probe)

	var before int64
	if err := db.QueryRow("SELECT max(_rid) FROM employees").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"INSERT INTO employees (id, name, dept) VALUES (900, ?, 'qa')", probe); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM employees WHERE name = ?", probe) })

	var got int64
	if err := db.QueryRow("SELECT _rid FROM employees WHERE name = ?", probe).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != before+1 {
		t.Errorf("_rid = %d, want %d", got, before+1)
	}
}

// TestLiveRIDRejectsExplicitAssignment guards the managed column.
func TestLiveRIDRejectsExplicitAssignment(t *testing.T) {
	db := ridDB(t)
	if _, err := db.Exec("UPDATE employees SET _rid = 1 WHERE id = 1"); err == nil {
		t.Error("expected UPDATE of _rid to be rejected")
	}
	if _, err := db.Exec("INSERT INTO employees (name, _rid) VALUES ('x', 1)"); err == nil {
		t.Error("expected INSERT of _rid to be rejected")
	}
}

// TestLiveRowShiftRetargets is the core identity guarantee: a row that moves
// between the scan and the write must still be written correctly.
func TestLiveRowShiftRetargets(t *testing.T) {
	db := ridDB(t)
	const target = "Zz Shift Target"
	const filler = "Zz Shift Filler"
	db.Exec("DELETE FROM employees WHERE name = ? OR name = ?", target, filler)

	// filler sits above target, so removing it shifts target up one row.
	if _, err := db.Exec("INSERT INTO employees (id, name, dept) VALUES (901, ?, 'qa')", filler); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO employees (id, name, dept) VALUES (902, ?, 'qa')", target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM employees WHERE name = ? OR name = ?", target, filler) })

	once := false
	afterScan = func() {
		if once {
			return
		}
		once = true
		afterScan = nil
		if _, err := db.Exec("DELETE FROM employees WHERE name = ?", filler); err != nil {
			t.Errorf("concurrent delete: %v", err)
		}
	}

	if _, err := db.Exec("UPDATE employees SET dept = 'moved' WHERE name = ?", target); err != nil {
		t.Fatalf("update after shift: %v", err)
	}

	var dept string
	if err := db.QueryRow("SELECT dept FROM employees WHERE name = ?", target).Scan(&dept); err != nil {
		t.Fatal(err)
	}
	if dept != "moved" {
		t.Errorf("target dept = %q, want \"moved\" (the write landed on the wrong row)", dept)
	}
	// The row that shifted into the old position must be untouched.
	var stray int
	if err := db.QueryRow("SELECT count(*) FROM employees WHERE dept = 'moved'").Scan(&stray); err != nil {
		t.Fatal(err)
	}
	if stray != 1 {
		t.Errorf("%d rows have dept='moved', want exactly 1", stray)
	}
}

// TestLiveConflictOnConcurrentModify checks that a genuine conflict aborts
// rather than silently overwriting.
func TestLiveConflictOnConcurrentModify(t *testing.T) {
	db := ridDB(t)
	const probe = "Zz Conflict Probe"
	db.Exec("DELETE FROM employees WHERE name = ?", probe)
	if _, err := db.Exec("INSERT INTO employees (id, name, dept, salary) VALUES (903, ?, 'qa', 1000)", probe); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM employees WHERE name = ?", probe) })

	once := false
	afterScan = func() {
		if once {
			return
		}
		once = true
		afterScan = nil
		if _, err := db.Exec("UPDATE employees SET salary = 4242 WHERE name = ?", probe); err != nil {
			t.Errorf("concurrent update: %v", err)
		}
	}

	_, err := db.Exec("UPDATE employees SET salary = 9999 WHERE name = ?", probe)
	if err == nil {
		t.Fatal("expected a conflict, got success")
	}
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ConflictError, got %T: %v", err, err)
	}

	// The losing write must not have been applied.
	var salary float64
	if err := db.QueryRow("SELECT salary FROM employees WHERE name = ?", probe).Scan(&salary); err != nil {
		t.Fatal(err)
	}
	if salary != 4242 {
		t.Errorf("salary = %v, want 4242 (the conflicting write was applied anyway)", salary)
	}
}
