package sheetsql

import (
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/xwb1989/sqlparser"
)

func testSchema() *schema {
	cols := []column{
		{"A", "id", "number"},
		{"B", "name", "string"},
		{"C", "dept", "string"},
		{"D", "salary", "number"},
		{"E", "hired", "date"},
		{"F", "active", "boolean"},
	}
	s := &schema{Sheet: "employees", Columns: cols,
		byName: map[string]column{}, byLetter: map[string]column{}}
	for _, c := range cols {
		s.byName[c.Label] = c
		s.byLetter[c.Letter] = c
	}
	return s
}

func tq(t *testing.T, sql string, args ...any) (string, error) {
	t.Helper()
	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		return "", err
	}
	nv := make([]driver.NamedValue, len(args))
	for i, a := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	tr := &translator{src: testSchema(), args: nv}
	out, err := tr.translateSelect(stmt.(*sqlparser.Select))
	if err != nil {
		return "", err
	}
	return out.TQ, nil
}

func TestTranslate(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"SELECT * FROM employees",
			"select A, B, C, D, E, F"},
		{"SELECT name, salary FROM employees WHERE dept = 'eng'",
			"select B, D where C = 'eng'"},
		{"SELECT name FROM employees WHERE salary > 150000 AND active = true",
			"select B where (D > 150000 and F = true)"},
		{"SELECT name FROM employees WHERE dept = 'eng' OR dept = 'research'",
			"select B where (C = 'eng' or C = 'research')"},
		{"SELECT name FROM employees WHERE dept IN ('eng','research')",
			"select B where (C = 'eng' or C = 'research')"},
		{"SELECT name FROM employees WHERE dept NOT IN ('eng','research')",
			"select B where (C != 'eng' and C != 'research')"},
		{"SELECT name FROM employees WHERE salary BETWEEN 100 AND 200",
			"select B where (D >= 100 and D <= 200)"},
		{"SELECT name FROM employees WHERE name LIKE 'A%'",
			"select B where B like 'A%'"},
		{"SELECT name FROM employees WHERE name IS NULL",
			"select B where B is null"},
		{"SELECT name FROM employees WHERE name IS NOT NULL",
			"select B where B is not null"},
		{"SELECT dept, count(*) FROM employees GROUP BY dept",
			"select C, count(A) group by C"},
		{"SELECT dept, avg(salary) FROM employees GROUP BY dept ORDER BY avg(salary) DESC",
			"select C, avg(D) group by C order by avg(D) desc"},
		{"SELECT DISTINCT dept FROM employees",
			"select C group by C"},
		{"SELECT name FROM employees ORDER BY salary DESC LIMIT 5",
			"select B order by D desc limit 5"},
		{"SELECT name FROM employees LIMIT 10 OFFSET 20",
			"select B limit 10 offset 20"},
		{"SELECT name FROM employees WHERE hired > '2020-01-01'",
			"select B where E > date '2020-01-01'"},
		{"SELECT name FROM employees WHERE NOT (dept = 'eng')",
			"select B where not (C = 'eng')"},
		{"SELECT salary / 12 FROM employees",
			"select (D / 12)"},
		{"SELECT name FROM employees WHERE name = 'O''Brien'",
			`select B where B = "O'Brien"`},
	}
	for _, c := range cases {
		got, err := tq(t, c.sql)
		if err != nil {
			t.Errorf("%s\n  unexpected error: %v", c.sql, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s\n  got:  %s\n  want: %s", c.sql, got, c.want)
		}
	}
}

// gviz evaluates a handful of scalar functions server-side; pushing them down
// keeps the filtering on Google's side instead of fetching rows to test locally.
func TestTranslateScalarFunctions(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"SELECT upper(name) FROM employees", "select upper(B)"},
		{"SELECT lower(dept) FROM employees", "select lower(C)"},
		{"SELECT name FROM employees WHERE year(hired) > 2018",
			"select B where year(E) > 2018"},
		{"SELECT name FROM employees WHERE quarter(hired) = 1",
			"select B where quarter(E) = 1"},
		{"SELECT name FROM employees WHERE dayofweek(hired) = 2",
			"select B where dayOfWeek(E) = 2"},
		// gviz numbers months from zero; SQL numbers them from one.
		{"SELECT name FROM employees WHERE month(hired) = 3",
			"select B where (month(E) + 1) = 3"},
		{"SELECT year(hired), count(*) FROM employees GROUP BY year(hired)",
			"select year(E), count(A) group by year(E)"},
	}
	for _, c := range cases {
		got, err := tq(t, c.sql)
		if err != nil {
			t.Errorf("%s\n  unexpected error: %v", c.sql, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s\n  got:  %s\n  want: %s", c.sql, got, c.want)
		}
	}
}

func TestTranslateScalarArity(t *testing.T) {
	if _, err := tq(t, "SELECT year(hired, dept) FROM employees"); err == nil {
		t.Error("expected an arity error for year() with two arguments")
	}
}

func TestTranslatePlaceholders(t *testing.T) {
	got, err := tq(t, "SELECT name FROM employees WHERE dept = ? AND salary > ?", "eng", int64(150000))
	if err != nil {
		t.Fatal(err)
	}
	if want := "select B where (C = 'eng' and D > 150000)"; got != want {
		t.Errorf("got %s want %s", got, want)
	}

	got, err = tq(t, "SELECT name FROM employees WHERE hired >= ?", time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if want := "select B where E >= date '2020-01-02'"; got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestTranslateRejects(t *testing.T) {
	cases := []struct{ sql, contains string }{
		{"SELECT a.x FROM employees a JOIN other b ON a.x = b.y", "joins"},
		{"SELECT dept FROM employees GROUP BY dept HAVING count(id) > 1", "HAVING"},
		{"SELECT nope FROM employees", `no column "nope"`},
		{"SELECT concat(name, dept) FROM employees", "concat()"},
		{"SELECT name FROM employees WHERE id IN (SELECT id FROM other)", "IN with a non-literal list"},
	}
	for _, c := range cases {
		_, err := tq(t, c.sql)
		if err == nil {
			t.Errorf("%s: expected error", c.sql)
			continue
		}
		if !strings.Contains(err.Error(), c.contains) {
			t.Errorf("%s\n  error %q does not mention %q", c.sql, err, c.contains)
		}
	}
}

func TestUnknownColumnListsAvailable(t *testing.T) {
	_, err := tq(t, "SELECT nope FROM employees")
	if err == nil || !strings.Contains(err.Error(), "id, name, dept, salary, hired, active") {
		t.Fatalf("expected available columns in error, got %v", err)
	}
}
