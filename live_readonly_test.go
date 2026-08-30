package sheetsql_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"
)

// readOnlyDB opens the live spreadsheet with readonly=1, which requests
// read-only OAuth scopes rather than merely refusing writes in the driver.
func readOnlyDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SHEETSQL_DSN")
	if dsn == "" {
		t.Skip("set SHEETSQL_DSN to run live tests")
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("sheets", dsn+sep+"readonly=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// A read-only connection keeps the whole single-table pushdown surface.
func TestLiveReadOnlySelect(t *testing.T) {
	db := readOnlyDB(t)
	rows, err := db.Query(`SELECT dept, count(*), avg(salary) FROM employees
		WHERE active = true GROUP BY dept ORDER BY count(*) DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var dept string
		var c int
		var avg float64
		if err := rows.Scan(&dept, &c, &avg); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("got %d groups, want 3", n)
	}
}

func TestLiveReadOnlyTypedScanAndPing(t *testing.T) {
	db := readOnlyDB(t)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	var name string
	var salary float64
	if err := db.QueryRow("SELECT name, salary FROM employees WHERE id = ?", 2).
		Scan(&name, &salary); err != nil {
		t.Fatal(err)
	}
	if name != "Grace Hopper" || salary != 172000 {
		t.Errorf("got %q %v", name, salary)
	}
}

func TestLiveReadOnlyRejectsWrites(t *testing.T) {
	db := readOnlyDB(t)
	for _, q := range []string{
		"INSERT INTO employees (id, name) VALUES (9999, 'nope')",
		"UPDATE employees SET salary = 1 WHERE id = 1",
		"DELETE FROM employees WHERE id = 9999",
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: expected a read-only error", q)
		} else if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: got %v, want a read-only error", q, err)
		}
	}
}

// Joins are evaluated by writing a formula, so a read-only connection must
// refuse them and say why.
func TestLiveReadOnlyRejectsFormulaQueries(t *testing.T) {
	db := readOnlyDB(t)
	for _, q := range []string{
		`SELECT e.name FROM employees e JOIN depts d ON e.dept = d.dept`,
		`SELECT dept FROM employees GROUP BY dept HAVING count(*) > 1`,
		`SELECT name FROM employees UNION ALL SELECT name FROM employees`,
		`SELECT CASE WHEN salary > 1 THEN 'a' ELSE 'b' END FROM employees`,
	} {
		_, err := db.Query(q)
		if err == nil {
			t.Errorf("%s: expected a read-only error", q)
			continue
		}
		if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: got %v, want an error mentioning read-only", q, err)
		}
	}
}
