package sheetsql

import (
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/xwb1989/sqlparser"
)

func mkSchema(sheet string, cols ...column) *schema {
	s := &schema{Sheet: sheet, Columns: cols,
		byName: map[string]column{}, byLetter: map[string]column{}}
	for _, c := range cols {
		s.byName[strings.ToLower(c.Label)] = c
		s.byLetter[c.Letter] = c
	}
	return s
}

// employees(A id, B name, C dept, D salary) joined to depts(A dept, B label).
func testComposition(t *testing.T, sql string) (*composition, *sqlparser.Select) {
	t.Helper()
	emp := mkSchema("employees",
		column{"A", "id", "number"}, column{"B", "name", "string"},
		column{"C", "dept", "string"}, column{"D", "salary", "number"})
	dep := mkSchema("depts",
		column{"A", "dept", "string"}, column{"B", "label", "string"},
		column{"C", "budget", "number"})

	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := stmt.(*sqlparser.Select)

	var exprs []sqlparser.TableExpr
	var joins []*sqlparser.JoinTableExpr
	if err := flattenJoins(sel.From[0], &exprs, &joins); err != nil {
		t.Fatalf("flatten: %v", err)
	}
	comp := &composition{}
	offset := 0
	for _, e := range exprs {
		name, alias, err := tableNameOf(e)
		if err != nil {
			t.Fatal(err)
		}
		s := emp
		if name == "depts" {
			s = dep
		}
		comp.tables = append(comp.tables, &srcTable{
			alias: alias, sheet: name, schema: s, offset: offset, width: s.width(),
		})
		offset += s.width()
	}
	if err := comp.resolveJoinKeys(joins); err != nil {
		t.Fatalf("resolve join keys: %v", err)
	}
	return comp, sel
}

func compile(t *testing.T, sql string, args ...any) (string, []resultCol) {
	t.Helper()
	comp, sel := testComposition(t, sql)
	nv := make([]driver.NamedValue, len(args))
	for i, a := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	f, cols, _, err := compileFormula(comp, sel, nv)
	if err != nil {
		t.Fatalf("compile %q: %v", sql, err)
	}
	return f, cols
}

func TestCompileInnerJoin(t *testing.T) {
	f, cols := compile(t, `SELECT e.name, d.label FROM employees e JOIN depts d ON e.dept = d.dept`)

	// employees occupies Col1..Col4, depts Col5..Col7.
	for _, want := range []string{
		"=LET(",
		`_s0,FILTER(employees!A2:D,LEN(employees!A2:A)>0)`,
		`_l1,{depts!A2:A,depts!A2:C}`,
		`VLOOKUP(INDEX(_s0,0,3),_l1,{2,3,4},FALSE)`,
		`_f,FILTER(_s1,ISNUMBER(MATCH(INDEX(_s1,0,3),depts!A2:A,0)))`,
		`select Col2, Col6`,
	} {
		if !strings.Contains(f, want) {
			t.Errorf("formula missing %q\ngot: %s", want, f)
		}
	}
	if len(cols) != 2 || cols[0].Name != "name" || cols[1].Name != "label" {
		t.Errorf("columns = %+v", cols)
	}
}

// A LEFT JOIN must not filter unmatched rows out.
func TestCompileLeftJoinKeepsUnmatched(t *testing.T) {
	f, _ := compile(t, `SELECT e.name, d.label FROM employees e LEFT JOIN depts d ON e.dept = d.dept`)
	if strings.Contains(f, "ISNUMBER(MATCH(") {
		t.Errorf("LEFT JOIN must not drop unmatched rows:\n%s", f)
	}
	if !strings.Contains(f, "IFNA(") {
		t.Errorf("LEFT JOIN should blank unmatched lookups:\n%s", f)
	}
}

func TestCompileJoinWhereOrderLimit(t *testing.T) {
	f, _ := compile(t, `SELECT e.name FROM employees e JOIN depts d ON e.dept = d.dept
		WHERE e.salary > 100 AND d.label = 'Eng' ORDER BY e.salary DESC LIMIT 5`)
	if !strings.Contains(f, "where (Col4 > 100 and Col6 = 'Eng')") {
		t.Errorf("where clause not compiled:\n%s", f)
	}
	if !strings.Contains(f, "order by Col4 desc limit 5") {
		t.Errorf("order/limit not compiled:\n%s", f)
	}
}

// HAVING has no equivalent in the query language, so it becomes a second QUERY
// wrapping the grouped result and addressing it by position.
func TestCompileHavingWrapsQuery(t *testing.T) {
	f, _ := compile(t, `SELECT d.label, count(*) FROM employees e JOIN depts d ON e.dept = d.dept
		GROUP BY d.label HAVING count(*) > 1 ORDER BY count(*) DESC`)
	if strings.Count(f, "QUERY(") != 2 {
		t.Errorf("expected a wrapped QUERY, got %d:\n%s", strings.Count(f, "QUERY("), f)
	}
	if !strings.Contains(f, "where Col2 > 1") {
		t.Errorf("HAVING should address the inner aggregate by position:\n%s", f)
	}
	if !strings.Contains(f, "order by Col2 desc") {
		t.Errorf("ORDER BY on an aggregate belongs to the wrapper:\n%s", f)
	}
}

func TestCompilePlaceholders(t *testing.T) {
	f, _ := compile(t, `SELECT e.name FROM employees e JOIN depts d ON e.dept = d.dept WHERE d.label = ?`, "Engineering")
	if !strings.Contains(f, "where Col6 = 'Engineering'") {
		t.Errorf("placeholder not bound:\n%s", f)
	}
}

func TestCompileStarExpandsBothTables(t *testing.T) {
	_, cols := compile(t, `SELECT * FROM employees e JOIN depts d ON e.dept = d.dept`)
	if len(cols) != 7 {
		t.Fatalf("expected 4+3 columns, got %d: %+v", len(cols), cols)
	}
	if cols[0].Name != "id" || cols[4].Name != "dept" || cols[5].Name != "label" {
		t.Errorf("unexpected column order: %+v", cols)
	}
}

func TestCompileQualifiedStar(t *testing.T) {
	_, cols := compile(t, `SELECT d.* FROM employees e JOIN depts d ON e.dept = d.dept`)
	if len(cols) != 3 || cols[0].Name != "dept" {
		t.Errorf("d.* should expand to depts only, got %+v", cols)
	}
}

func TestCompileAmbiguousColumn(t *testing.T) {
	comp, sel := testComposition(t, `SELECT dept FROM employees e JOIN depts d ON e.dept = d.dept`)
	_, _, _, err := compileFormula(comp, sel, nil)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected an ambiguity error, got %v", err)
	}
}

func TestCompileRejects(t *testing.T) {
	cases := []struct{ sql, contains string }{
		{`SELECT e.name FROM employees e RIGHT JOIN depts d ON e.dept = d.dept`, "RIGHT JOIN"},
		{`SELECT e.name FROM employees e JOIN depts d ON e.salary > d.budget`, "single equality"},
		{`SELECT e.name FROM employees e JOIN depts d USING (dept)`, "USING"},
	}
	for _, c := range cases {
		stmt, err := sqlparser.Parse(c.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", c.sql, err)
		}
		sel := stmt.(*sqlparser.Select)
		var exprs []sqlparser.TableExpr
		var joins []*sqlparser.JoinTableExpr
		if err := flattenJoins(sel.From[0], &exprs, &joins); err != nil {
			if !strings.Contains(err.Error(), c.contains) {
				t.Errorf("%s: got %v, want mention of %q", c.sql, err, c.contains)
			}
			continue
		}
		comp := &composition{}
		emp := mkSchema("employees", column{"A", "id", "number"}, column{"C", "dept", "string"}, column{"D", "salary", "number"})
		dep := mkSchema("depts", column{"A", "dept", "string"}, column{"C", "budget", "number"})
		off := 0
		for _, e := range exprs {
			name, alias, _ := tableNameOf(e)
			sc := emp
			if name == "depts" {
				sc = dep
			}
			comp.tables = append(comp.tables, &srcTable{alias: alias, sheet: name, schema: sc, offset: off, width: sc.width()})
			off += sc.width()
		}
		err = comp.resolveJoinKeys(joins)
		if err == nil || !strings.Contains(err.Error(), c.contains) {
			t.Errorf("%s: got %v, want mention of %q", c.sql, err, c.contains)
		}
	}
}

func TestNeedsFormula(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"SELECT a FROM t", false},
		{"SELECT a FROM t WHERE b = 1 GROUP BY a ORDER BY a LIMIT 5", false},
		{"SELECT a FROM t GROUP BY a HAVING count(*) > 1", true},
		{"SELECT a FROM t JOIN u ON t.x = u.x", true},
	}
	for _, c := range cases {
		stmt, err := sqlparser.Parse(c.sql)
		if err != nil {
			t.Fatal(err)
		}
		if got := needsFormula(stmt.(*sqlparser.Select)); got != c.want {
			t.Errorf("%s: needsFormula = %v, want %v", c.sql, got, c.want)
		}
	}
}

func TestColLetter(t *testing.T) {
	for _, c := range []struct {
		i    int
		want string
	}{{0, "A"}, {1, "B"}, {25, "Z"}, {26, "AA"}, {27, "AB"}, {51, "AZ"}, {52, "BA"}} {
		if got := colLetter(c.i); got != c.want {
			t.Errorf("colLetter(%d) = %q, want %q", c.i, got, c.want)
		}
	}
}

func TestQuoteFormulaString(t *testing.T) {
	if got := quoteFormulaString(`select A where B = "x"`); got != `"select A where B = ""x"""` {
		t.Errorf("got %s", got)
	}
}
