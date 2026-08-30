package sheetsql_test

import (
	"database/sql"
	"testing"
)

// TestLiveJoin checks a two-table join end to end. depts holds one row per
// department, so every employee matches exactly one.
func TestLiveJoin(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query(`SELECT e.name, d.label, d.budget
		FROM employees e JOIN depts d ON e.dept = d.dept
		WHERE e.salary > 170000 ORDER BY e.salary DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type rec struct {
		name, label string
		budget      int64
	}
	var got []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.name, &r.label, &r.budget); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := []rec{
		{"Barbara Liskov", "Engineering", 5000000},
		{"Alan Turing", "Engineering", 5000000},
		{"Margaret Hamilton", "Research", 3000000},
		{"Grace Hopper", "Engineering", 5000000},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestLiveThreeTableJoin joins a fact table to two dimensions.
func TestLiveThreeTableJoin(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query(`SELECT s.name, d.label, r.continent
		FROM scale s
		JOIN depts d ON s.dept = d.dept
		JOIN regions r ON s.region = r.region
		WHERE s.salary > 199950 ORDER BY s.salary DESC LIMIT 5`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var name, label, continent string
		if err := rows.Scan(&name, &label, &continent); err != nil {
			t.Fatal(err)
		}
		if name == "" || label == "" || continent == "" {
			t.Errorf("unjoined row: %q %q %q", name, label, continent)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("three-table join returned nothing")
	}
}

// TestLiveJoinAggregateHaving exercises the wrapped-QUERY path.
func TestLiveJoinAggregateHaving(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query(`SELECT d.label, count(*) AS n, avg(e.salary) AS pay
		FROM employees e JOIN depts d ON e.dept = d.dept
		GROUP BY d.label HAVING count(*) > 1 ORDER BY avg(e.salary) DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type rec struct {
		label string
		n     int
		pay   float64
	}
	var got []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.label, &r.n, &r.pay); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []rec{{"Engineering", 4, 177000}, {"Research", 3, 158000}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestLiveLeftJoinKeepsUnmatched checks that a row with no match survives a
// LEFT JOIN and reports NULL for the right-hand columns.
func TestLiveLeftJoinKeepsUnmatched(t *testing.T) {
	db := liveDB(t)
	const probe = "Zz Orphan"
	db.Exec("DELETE FROM employees WHERE name = ?", probe)
	if _, err := db.Exec(
		"INSERT INTO employees (id, name, dept, salary) VALUES (950, ?, 'nosuchdept', 1)", probe); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM employees WHERE name = ?", probe) })

	var label sql.NullString
	err := db.QueryRow(`SELECT d.label FROM employees e
		LEFT JOIN depts d ON e.dept = d.dept WHERE e.name = ?`, probe).Scan(&label)
	if err != nil {
		t.Fatalf("left join lost the unmatched row: %v", err)
	}
	if label.Valid {
		t.Errorf("expected NULL label, got %q", label.String)
	}

	// The same query as an INNER JOIN must drop it.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM employees e
		JOIN depts d ON e.dept = d.dept WHERE e.name = ?`, probe).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("inner join kept an unmatched row (count=%d)", n)
	}
}

// TestLiveJoinSelectStar checks that "*" spans both tables.
func TestLiveJoinSelectStar(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query(`SELECT * FROM employees e JOIN depts d ON e.dept = d.dept LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	// employees has 6 visible columns (_rid is hidden), depts has 3.
	if len(cols) != 9 {
		t.Errorf("got %d columns %v, want 9", len(cols), cols)
	}
	for _, c := range cols {
		if c == "_rid" {
			t.Errorf("_rid leaked into a join projection: %v", cols)
		}
	}
}
