package sheetsql_test

import (
	"database/sql"
	"testing"
)

// TestLiveCase checks CASE, which is computed by the formula engine and then
// projected by the query.
func TestLiveCase(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query(`SELECT name,
		CASE WHEN salary > 175000 THEN 'senior'
		     WHEN salary > 160000 THEN 'mid'
		     ELSE 'junior' END AS band
		FROM employees ORDER BY salary DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var name, band string
		if err := rows.Scan(&name, &band); err != nil {
			t.Fatal(err)
		}
		got[name] = band
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Barbara Liskov":    "senior", // 190000
		"Alan Turing":       "senior", // 181000
		"Margaret Hamilton": "mid",    // 174000
		"Grace Hopper":      "mid",    // 172000
		"Radia Perlman":     "mid",    // 168000
		"Ada Lovelace":      "mid",    // 165000
		"Katherine Johnson": "junior", // 158000
		"Jean Bartik":       "junior", // 142000
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: band = %q, want %q", name, got[name], w)
		}
	}
}

func TestLiveSimpleCase(t *testing.T) {
	db := liveDB(t)
	var n int
	err := db.QueryRow(`SELECT sum(CASE dept WHEN 'eng' THEN 1 ELSE 0 END) FROM employees`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("eng count via CASE = %d, want 4", n)
	}
}

// TestLiveScalarFunctions covers functions the query language lacks, which are
// precomputed as extra columns.
func TestLiveScalarFunctions(t *testing.T) {
	db := liveDB(t)
	var upper, concat string
	var length int
	var rounded float64
	err := db.QueryRow(`SELECT upper(name), length(name), round(salary/1000), concat(dept, '!')
		FROM employees WHERE name = ?`, "Grace Hopper").
		Scan(&upper, &length, &rounded, &concat)
	if err != nil {
		t.Fatal(err)
	}
	if upper != "GRACE HOPPER" {
		t.Errorf("upper = %q", upper)
	}
	if length != len("Grace Hopper") {
		t.Errorf("length = %d, want %d", length, len("Grace Hopper"))
	}
	if rounded != 172 {
		t.Errorf("round(salary/1000) = %v, want 172", rounded)
	}
	if concat != "eng!" {
		t.Errorf("concat = %q, want %q", concat, "eng!")
	}
}

func TestLiveCoalesce(t *testing.T) {
	db := liveDB(t)
	const probe = "Zz Coalesce"
	db.Exec("DELETE FROM employees WHERE name = ?", probe)
	if _, err := db.Exec("INSERT INTO employees (id, name, dept) VALUES (960, ?, 'qa')", probe); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM employees WHERE name = ?", probe) })

	// The default must share the column's type: QUERY coerces a mixed-type
	// column to whichever type dominates and nulls the rest.
	var v float64
	if err := db.QueryRow(`SELECT coalesce(salary, 0) FROM employees WHERE name = ?`, probe).
		Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Errorf("coalesce over a blank cell = %v, want 0", v)
	}
}

// TestLiveUnionAll keeps duplicates and concatenates branches.
func TestLiveUnionAll(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query(`SELECT name FROM employees WHERE dept = 'eng'
		UNION ALL SELECT name FROM employees WHERE dept = 'research'
		ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(names) != 7 { // 4 eng + 3 research
		t.Fatalf("got %d rows %v, want 7", len(names), names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("not ordered: %v", names)
			break
		}
	}
}

// TestLiveUnionDistinct removes duplicates across overlapping branches.
func TestLiveUnionDistinct(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query(`SELECT dept FROM employees WHERE salary > 150000
		UNION SELECT dept FROM employees WHERE salary > 100000`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		if seen[d] {
			t.Errorf("UNION returned duplicate %q", d)
		}
		seen[d] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Errorf("got %d distinct depts %v, want 3", len(seen), seen)
	}
}

func TestLiveUnionColumnCountMismatch(t *testing.T) {
	db := liveDB(t)
	_, err := db.Query(`SELECT name FROM employees UNION ALL SELECT name, dept FROM employees`)
	if err == nil {
		t.Fatal("expected an error for mismatched column counts")
	}
}

// TestLiveCompositeJoinKey joins on two columns at once.
func TestLiveCompositeJoinKey(t *testing.T) {
	db := liveDB(t)
	rows, err := db.Query(`SELECT s.region, s.quarter, s.amount, t.target
		FROM sales s JOIN targets t
		  ON s.region = t.region AND s.quarter = t.quarter
		ORDER BY s.region, s.quarter`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type rec struct {
		region, quarter string
		amount, target  float64
	}
	var got []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.region, &r.quarter, &r.amount, &r.target); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d rows, want 6: %+v", len(got), got)
	}
	// Each (region, quarter) must pick up its own target, not another quarter's.
	want := map[string]float64{
		"amer Q1": 200000, "amer Q2": 190000,
		"apac Q1": 80000, "apac Q2": 85000,
		"emea Q1": 100000, "emea Q2": 110000,
	}
	for _, r := range got {
		if w := want[r.region+" "+r.quarter]; r.target != w {
			t.Errorf("%s %s: target = %v, want %v", r.region, r.quarter, r.target, w)
		}
	}
}

func TestLiveCaseInWhere(t *testing.T) {
	db := liveDB(t)
	var n sql.NullInt64
	err := db.QueryRow(`SELECT count(*) FROM employees
		WHERE length(name) > 12`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	// Katherine Johnson (17), Margaret Hamilton (17), Barbara Liskov (14),
	// Radia Perlman (13), Ada Lovelace (12 -> excluded).
	if !n.Valid || n.Int64 != 4 {
		t.Errorf("count = %v, want 4", n)
	}
}
