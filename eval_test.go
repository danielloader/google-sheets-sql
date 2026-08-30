package sheetsql

import (
	"database/sql/driver"
	"testing"
	"time"

	"github.com/xwb1989/sqlparser"
)

// row mirrors testSchema: id, name, dept, salary, hired(serial), active
func evalRow(t *testing.T, sql string, row []any, args ...any) bool {
	t.Helper()
	stmt, err := sqlparser.Parse("SELECT * FROM employees WHERE " + sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	nv := make([]driver.NamedValue, len(args))
	for i, a := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	ev := &evaluator{s: testSchema(), args: nv, row: row}
	ok, err := ev.match(stmt.(*sqlparser.Select).Where.Expr)
	if err != nil {
		t.Fatalf("eval %q: %v", sql, err)
	}
	return ok
}

func TestEvaluator(t *testing.T) {
	// hired 2019-03-01 as a Sheets serial number.
	hired := 43525.0
	row := []any{2.0, "Grace Hopper", "eng", 172000.0, hired, true}

	cases := []struct {
		expr string
		want bool
	}{
		{"dept = 'eng'", true},
		{"dept = 'research'", false},
		{"salary > 150000", true},
		{"salary >= 172000", true},
		{"salary < 172000", false},
		{"active = true", true},
		{"active = false", false},
		{"name LIKE 'Grace%'", true},
		{"name LIKE '%Hopper'", true},
		{"name LIKE 'H%'", false},
		{"name NOT LIKE 'H%'", true},
		{"dept IN ('eng','research')", true},
		{"dept NOT IN ('eng','research')", false},
		{"salary BETWEEN 100000 AND 200000", true},
		{"salary BETWEEN 1 AND 2", false},
		{"dept = 'eng' AND salary > 150000", true},
		{"dept = 'ops' OR salary > 150000", true},
		{"NOT (dept = 'eng')", false},
		{"hired > '2019-01-01'", true},
		{"hired < '2019-01-01'", false},
		{"hired BETWEEN '2019-01-01' AND '2019-12-31'", true},
		{"id IS NOT NULL", true},
	}
	for _, c := range cases {
		if got := evalRow(t, c.expr, row); got != c.want {
			t.Errorf("WHERE %s = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestEvaluatorNulls(t *testing.T) {
	row := []any{1.0, "", "eng", nil, nil, nil}
	if !evalRow(t, "name IS NULL", row) {
		t.Error("empty string cell should read as NULL")
	}
	if evalRow(t, "salary > 0", row) {
		t.Error("NULL must not satisfy a comparison")
	}
	if evalRow(t, "salary = null", row) {
		t.Error("NULL = NULL must be false in a WHERE clause")
	}
	if !evalRow(t, "salary IS NULL", row) {
		t.Error("IS NULL should match a missing cell")
	}
}

func TestEvaluatorPlaceholders(t *testing.T) {
	row := []any{2.0, "Grace Hopper", "eng", 172000.0, 43525.0, true}
	if !evalRow(t, "dept = ?", row, "eng") {
		t.Error("string placeholder failed")
	}
	if !evalRow(t, "salary > ?", row, int64(1000)) {
		t.Error("int placeholder failed")
	}
	if !evalRow(t, "hired < ?", row, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("time placeholder failed")
	}
}

func TestSerialToTime(t *testing.T) {
	got := serialToTime(43525).Format("2006-01-02")
	if got != "2019-03-01" {
		t.Errorf("serial 43525 = %s, want 2019-03-01", got)
	}
	if got := serialToTime(25569).Format("2006-01-02"); got != "1970-01-01" {
		t.Errorf("serial 25569 = %s, want 1970-01-01", got)
	}
}

func TestColIndex(t *testing.T) {
	for _, c := range []struct {
		letter string
		want   int
	}{{"A", 0}, {"B", 1}, {"Z", 25}, {"AA", 26}, {"AB", 27}} {
		if got := colIndex(c.letter); got != c.want {
			t.Errorf("colIndex(%q) = %d, want %d", c.letter, got, c.want)
		}
	}
}
