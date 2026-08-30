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
		{`SELECT e.name FROM employees e JOIN depts d ON e.salary > d.budget`, "other than equality"},
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

// singleComposition builds a one-table namespace for expression tests.
func singleComposition() *composition {
	emp := mkSchema("employees",
		column{"A", "id", "number"}, column{"B", "name", "string"},
		column{"C", "dept", "string"}, column{"D", "salary", "number"})
	return &composition{tables: []*srcTable{
		{alias: "employees", sheet: "employees", schema: emp, offset: 0, width: emp.width()},
	}}
}

func compileSingle(t *testing.T, sql string, args ...any) string {
	t.Helper()
	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	nv := make([]driver.NamedValue, len(args))
	for i, a := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	f, _, _, err := compileFormula(singleComposition(), stmt.(*sqlparser.Select), nv)
	if err != nil {
		t.Fatalf("compile %q: %v", sql, err)
	}
	return f
}

// CASE has no equivalent in the query language, so it is computed as an extra
// source column and then referenced by position.
func TestCompileCaseBecomesPrecomputedColumn(t *testing.T) {
	f := compileSingle(t, `SELECT name, CASE WHEN salary > 100 THEN 'high' ELSE 'low' END AS band FROM employees`)
	if !strings.Contains(f, "IFS(") {
		t.Errorf("CASE should compile to IFS:\n%s", f)
	}
	if !strings.Contains(f, `_x1,ARRAYFORMULA(IFS(`) {
		t.Errorf("CASE should be bound as a computed column:\n%s", f)
	}
	// employees is 4 wide, so the computed column lands at Col5.
	if !strings.Contains(f, "select Col2, Col5") {
		t.Errorf("CASE column should be projected by position:\n%s", f)
	}
}

func TestCompileSimpleCase(t *testing.T) {
	f := compileSingle(t, `SELECT CASE dept WHEN 'eng' THEN 1 ELSE 0 END AS x FROM employees`)
	if !strings.Contains(f, `(INDEX(_s0,0,3)="eng")`) {
		t.Errorf("simple CASE should compare the subject:\n%s", f)
	}
}

// A function gviz knows stays inside QUERY; one it does not is precomputed.
func TestCompileFunctionSplit(t *testing.T) {
	f := compileSingle(t, `SELECT upper(name), length(name) FROM employees`)
	if !strings.Contains(f, "upper(Col2)") {
		t.Errorf("upper() should stay in the query:\n%s", f)
	}
	if !strings.Contains(f, "ARRAYFORMULA(LEN(INDEX(_s0,0,2)))") {
		t.Errorf("length() should be precomputed as LEN:\n%s", f)
	}
}

func TestCompileScalarFunctions(t *testing.T) {
	cases := []struct{ sql, want string }{
		{`SELECT abs(salary) FROM employees`, "ABS(INDEX(_s0,0,4))"},
		{`SELECT round(salary, 2) FROM employees`, "ROUND(INDEX(_s0,0,4),2)"},
		{`SELECT concat(name, dept) FROM employees`, "(INDEX(_s0,0,2)&INDEX(_s0,0,3))"},
		{`SELECT coalesce(name, 'none') FROM employees`, `IF(INDEX(_s0,0,2)="","none",INDEX(_s0,0,2))`},
		{`SELECT mod(salary, 7) FROM employees`, "MOD(INDEX(_s0,0,4),7)"},
		{`SELECT substr(name, 1, 3) FROM employees`, "MID(INDEX(_s0,0,2),1,3)"},
	}
	for _, c := range cases {
		f := compileSingle(t, c.sql)
		if !strings.Contains(f, c.want) {
			t.Errorf("%s\n  missing %q in:\n  %s", c.sql, c.want, f)
		}
	}
}

func TestCompileUnknownFunctionRejected(t *testing.T) {
	stmt, _ := sqlparser.Parse(`SELECT nosuchfn(name) FROM employees`)
	_, _, _, err := compileFormula(singleComposition(), stmt.(*sqlparser.Select), nil)
	if err == nil || !strings.Contains(err.Error(), "nosuchfn()") {
		t.Errorf("expected an unsupported-function error, got %v", err)
	}
}

// A multi-column ON clause is matched as one concatenated key.
func TestCompileCompositeJoinKey(t *testing.T) {
	f, _ := compile(t, `SELECT e.name FROM employees e
		JOIN depts d ON e.dept = d.dept AND e.name = d.label`)
	if !strings.Contains(f, `INDEX(_s0,0,3)&"`+keySep+`"&INDEX(_s0,0,2)`) {
		t.Errorf("probe side should concatenate both keys:\n%s", f)
	}
	if !strings.Contains(f, `ARRAYFORMULA(depts!A2:A&"`+keySep+`"&depts!B2:B)`) {
		t.Errorf("lookup side should concatenate both keys:\n%s", f)
	}
}

func TestFlattenUnion(t *testing.T) {
	stmt, err := sqlparser.Parse(`SELECT a FROM t UNION ALL SELECT b FROM u UNION ALL SELECT c FROM v`)
	if err != nil {
		t.Fatal(err)
	}
	var branches []*sqlparser.Select
	var distinct, seen bool
	if err := flattenUnion(stmt.(*sqlparser.Union), &branches, &distinct, &seen); err != nil {
		t.Fatal(err)
	}
	if len(branches) != 3 {
		t.Errorf("got %d branches, want 3", len(branches))
	}
	if distinct {
		t.Error("UNION ALL must not be treated as distinct")
	}
}

func TestFlattenUnionDistinct(t *testing.T) {
	stmt, _ := sqlparser.Parse(`SELECT a FROM t UNION SELECT b FROM u`)
	var branches []*sqlparser.Select
	var distinct, seen bool
	if err := flattenUnion(stmt.(*sqlparser.Union), &branches, &distinct, &seen); err != nil {
		t.Fatal(err)
	}
	if !distinct {
		t.Error("bare UNION should deduplicate")
	}
}

// Mixing ALL and DISTINCT would need two combining strategies at once.
func TestFlattenUnionRejectsMixed(t *testing.T) {
	stmt, _ := sqlparser.Parse(`SELECT a FROM t UNION ALL SELECT b FROM u UNION SELECT c FROM v`)
	var branches []*sqlparser.Select
	var distinct, seen bool
	err := flattenUnion(stmt.(*sqlparser.Union), &branches, &distinct, &seen)
	if err == nil || !strings.Contains(err.Error(), "mixing UNION and UNION ALL") {
		t.Errorf("expected a mixed-union error, got %v", err)
	}
}

func TestProjectionSource(t *testing.T) {
	p := &projectionSource{cols: []resultCol{{Name: "k", Type: "string"}, {Name: "n", Type: "number"}}}
	if id, _, err := p.column("", "n"); err != nil || id != "Col2" {
		t.Errorf("by name: %q %v", id, err)
	}
	// ORDER BY <ordinal> is legal after a UNION.
	if id, _, err := p.column("", "2"); err != nil || id != "Col2" {
		t.Errorf("by ordinal: %q %v", id, err)
	}
	if _, _, err := p.column("", "zz"); err == nil {
		t.Error("expected an error for an unknown column")
	}
}

func TestNeedsFormulaForSheetOnlyExpressions(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"SELECT upper(a) FROM t", false},
		{"SELECT year(a) FROM t", false},
		{"SELECT length(a) FROM t", true},
		{"SELECT CASE WHEN a > 1 THEN 'x' END FROM t", true},
		{"SELECT a FROM t WHERE length(a) > 2", true},
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
