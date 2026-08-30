package sheetsql_test

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/danielloader/google-sheets-sql"
)

// Live tests run against a real spreadsheet. Set:
//
//	SHEETSQL_DSN="sheets://<id>?credentials=/path/key.json"
//
// The sheet must hold the fixtures loaded by cmd/bootstrap.
func liveDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SHEETSQL_DSN")
	if dsn == "" {
		t.Skip("set SHEETSQL_DSN to run live tests")
	}
	db, err := sql.Open("sheets", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestLivePing(t *testing.T) {
	if err := liveDB(t).Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveTypedScan(t *testing.T) {
	db := liveDB(t)
	var (
		id     int64
		name   string
		salary float64
		hired  time.Time
		active bool
	)
	err := db.QueryRow(
		"SELECT id, name, salary, hired, active FROM employees WHERE name = ?",
		"Grace Hopper").Scan(&id, &name, &salary, &hired, &active)
	if err != nil {
		t.Fatal(err)
	}
	if id != 2 || name != "Grace Hopper" || salary != 172000 || !active {
		t.Errorf("got id=%d name=%q salary=%v active=%v", id, name, salary, active)
	}
	if hired.Format("2006-01-02") != "2017-07-15" {
		t.Errorf("hired = %s, want 2017-07-15", hired.Format("2006-01-02"))
	}
}

func TestLiveNullScan(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query("SELECT name, salary FROM employees WHERE salary IS NULL")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var salary sql.NullFloat64
		if err := rows.Scan(&name, &salary); err != nil {
			t.Fatal(err)
		}
		if salary.Valid {
			t.Errorf("%s: expected NULL salary", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestLivePushdown(t *testing.T) {
	db := liveDB(t)

	var n int
	if err := db.QueryRow("SELECT count(*) FROM employees WHERE dept = 'eng'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("count(eng) = %d, want 4", n)
	}

	rows, err := db.Query(`SELECT dept, avg(salary) FROM employees
		GROUP BY dept ORDER BY avg(salary) DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var depts []string
	for rows.Next() {
		var d string
		var avg float64
		if err := rows.Scan(&d, &avg); err != nil {
			t.Fatal(err)
		}
		depts = append(depts, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(depts) != 3 {
		t.Fatalf("got %d depts, want 3: %v", len(depts), depts)
	}
}

func TestLiveDateFilter(t *testing.T) {
	db := liveDB(t)
	var n int
	err := db.QueryRow("SELECT count(*) FROM employees WHERE hired >= ?",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("hired since 2020 = %d, want 3", n)
	}
}

func TestLiveInAndBetween(t *testing.T) {
	db := liveDB(t)
	var n int
	if err := db.QueryRow(
		"SELECT count(*) FROM employees WHERE dept IN ('eng','networking')").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("IN = %d, want 5", n)
	}
	if err := db.QueryRow(
		"SELECT count(*) FROM employees WHERE salary BETWEEN 160000 AND 175000").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("BETWEEN = %d, want 4", n)
	}
}

// TestLiveWriteRoundTrip inserts, updates and deletes a row, leaving the sheet
// as it found it.
func TestLiveWriteRoundTrip(t *testing.T) {
	db := liveDB(t)

	const probe = "Zz Probe Row"
	// Clean up any residue from a failed earlier run.
	if _, err := db.Exec("DELETE FROM employees WHERE name = ?", probe); err != nil {
		t.Fatal(err)
	}

	res, err := db.Exec(`INSERT INTO employees (id, name, dept, salary, hired, active)
		VALUES (?, ?, 'qa', 99000, '2024-01-15', true)`, 999, probe)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("insert affected %d rows", n)
	}

	var salary float64
	var dept string
	if err := db.QueryRow("SELECT dept, salary FROM employees WHERE name = ?", probe).
		Scan(&dept, &salary); err != nil {
		t.Fatalf("read back after insert: %v", err)
	}
	if dept != "qa" || salary != 99000 {
		t.Errorf("after insert: dept=%q salary=%v", dept, salary)
	}

	res, err = db.Exec("UPDATE employees SET salary = ?, dept = ? WHERE name = ?",
		123456, "sre", probe)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("update affected %d rows", n)
	}
	if err := db.QueryRow("SELECT dept, salary FROM employees WHERE name = ?", probe).
		Scan(&dept, &salary); err != nil {
		t.Fatalf("read back after update: %v", err)
	}
	if dept != "sre" || salary != 123456 {
		t.Errorf("after update: dept=%q salary=%v, want sre/123456", dept, salary)
	}

	res, err = db.Exec("DELETE FROM employees WHERE name = ?", probe)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("delete affected %d rows", n)
	}

	var count int
	if err := db.QueryRow("SELECT count(*) FROM employees WHERE name = ?", probe).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("row survived delete")
	}
}

func TestLiveUnknownTabErrors(t *testing.T) {
	db := liveDB(t)
	_, err := db.Query("SELECT * FROM no_such_tab")
	if err == nil {
		t.Fatal("expected an error for an unknown tab")
	}
}
