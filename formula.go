package sheetsql

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/xwb1989/sqlparser"
)

// A joined query is compiled into a single spreadsheet formula of the shape
//
//	=LET(_s0, <base>, _l1, <lookup>, _s1, {_s0, VLOOKUP(...)}, ..., QUERY(_sN, "..."))
//
// and evaluated by Google. LET binds each intermediate array once, so a chain
// of joins does not duplicate its inputs. LET names must not look like A1
// references -- Sheets rejects "j1" as a name -- hence the underscore prefix.

type joinKind int

const (
	joinInner joinKind = iota
	joinLeft
)

type srcTable struct {
	alias  string
	sheet  string
	schema *schema
	offset int // 0-based column offset within the composed array
	width  int

	kind joinKind
	// A join may key on several columns. Positions are 1-based within the
	// composed array; rightKeys are the matching columns of this table.
	leftKeys  []int
	rightKeys []column
}

// keySep joins the parts of a composite key. U+241F is the symbol for "unit
// separator" and does not occur in spreadsheet data.
const keySep = "\u241F"

// composition is the column namespace of a compiled join.
type composition struct {
	tables []*srcTable
}

func (c *composition) find(alias string) *srcTable {
	for _, t := range c.tables {
		if strings.EqualFold(t.alias, alias) || strings.EqualFold(t.sheet, alias) {
			return t
		}
	}
	return nil
}

func (c *composition) column(qualifier, name string) (string, string, error) {
	if qualifier != "" {
		t := c.find(qualifier)
		if t == nil {
			return "", "", fmt.Errorf("sheetsql: unknown table %q in %s", qualifier, c.describe())
		}
		col, ok := t.schema.lookup(name)
		if !ok {
			return "", "", fmt.Errorf("sheetsql: no column %q in %q (have: %s)",
				name, t.alias, strings.Join(t.schema.names(), ", "))
		}
		return colRef(t.offset + colIndex(col.Letter)), col.Type, nil
	}

	var found *srcTable
	var col column
	for _, t := range c.tables {
		if cc, ok := t.schema.lookup(name); ok {
			if found != nil {
				return "", "", fmt.Errorf("sheetsql: column %q is ambiguous; qualify it with a table name", name)
			}
			found, col = t, cc
		}
	}
	if found == nil {
		return "", "", fmt.Errorf("sheetsql: no column %q in %s", name, c.describe())
	}
	return colRef(found.offset + colIndex(col.Letter)), col.Type, nil
}

// position returns the 1-based column position of a reference within the
// composed array, for INDEX().
func (c *composition) position(qualifier, name string) (int, error) {
	ident, _, err := c.column(qualifier, name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimPrefix(ident, "Col"))
	if err != nil {
		return 0, fmt.Errorf("sheetsql: bad column reference %q", ident)
	}
	return n, nil
}

func (c *composition) star(qualifier string) ([]projected, error) {
	var out []projected
	for _, t := range c.tables {
		if qualifier != "" && !strings.EqualFold(t.alias, qualifier) && !strings.EqualFold(t.sheet, qualifier) {
			continue
		}
		for _, col := range t.schema.Columns {
			out = append(out, projected{
				Ident: colRef(t.offset + colIndex(col.Letter)),
				Name:  col.Label,
				Type:  col.Type,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("sheetsql: unknown table %q", qualifier)
	}
	return out, nil
}

func (c *composition) first() string { return colRef(0) }

func (c *composition) describe() string {
	names := make([]string, len(c.tables))
	for i, t := range c.tables {
		names[i] = t.alias
	}
	return strings.Join(names, " + ")
}

// colRef renders a 0-based composed position as a QUERY column reference.
func colRef(i int) string { return "Col" + strconv.Itoa(i+1) }

// a1Range renders "'sheet'!A2:F", the data rows of a tab excluding its header.
func a1Range(sheet string, headerRows, fromCol, toCol int) string {
	return fmt.Sprintf("%s!%s%d:%s", normaliseSheet(sheet),
		colLetter(fromCol), headerRows+1, colLetter(toCol))
}

func a1Column(sheet string, headerRows, col int) string {
	return fmt.Sprintf("%s!%s%d:%s", normaliseSheet(sheet),
		colLetter(col), headerRows+1, colLetter(col))
}

func colLetter(i int) string {
	s := ""
	for i >= 0 {
		s = string(rune('A'+i%26)) + s
		i = i/26 - 1
	}
	return s
}

// flattenJoins walks a left-deep join tree into an ordered table list.
func flattenJoins(e sqlparser.TableExpr, out *[]sqlparser.TableExpr, conds *[]*sqlparser.JoinTableExpr) error {
	switch n := e.(type) {
	case *sqlparser.AliasedTableExpr:
		*out = append(*out, n)
		return nil
	case *sqlparser.JoinTableExpr:
		if err := flattenJoins(n.LeftExpr, out, conds); err != nil {
			return err
		}
		right, ok := n.RightExpr.(*sqlparser.AliasedTableExpr)
		if !ok {
			return unsupported("nested joins on the right-hand side")
		}
		*out = append(*out, right)
		*conds = append(*conds, n)
		return nil
	case *sqlparser.ParenTableExpr:
		if len(n.Exprs) != 1 {
			return unsupported("this parenthesised FROM clause")
		}
		return flattenJoins(n.Exprs[0], out, conds)
	}
	return unsupported("this FROM clause")
}

func tableNameOf(e sqlparser.TableExpr) (name, alias string, err error) {
	ate, ok := e.(*sqlparser.AliasedTableExpr)
	if !ok {
		return "", "", unsupported("this table expression")
	}
	tn, ok := ate.Expr.(sqlparser.TableName)
	if !ok {
		return "", "", unsupported("subqueries in FROM")
	}
	name = tn.Name.String()
	alias = ate.As.String()
	if alias == "" {
		alias = name
	}
	return name, alias, nil
}

// buildComposition resolves every table in the FROM clause and assigns each a
// column offset within the composed array.
func (c *conn) buildComposition(ctx context.Context, sel *sqlparser.Select) (*composition, []*sqlparser.JoinTableExpr, error) {
	var exprs []sqlparser.TableExpr
	var joins []*sqlparser.JoinTableExpr
	if len(sel.From) != 1 {
		return nil, nil, unsupported("comma-separated FROM lists; use explicit JOIN")
	}
	if err := flattenJoins(sel.From[0], &exprs, &joins); err != nil {
		return nil, nil, err
	}

	comp := &composition{}
	offset := 0
	for _, e := range exprs {
		name, alias, err := tableNameOf(e)
		if err != nil {
			return nil, nil, err
		}
		sheet, err := c.resolveSheet(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		s, err := c.schemas.get(ctx, c, sheet)
		if err != nil {
			return nil, nil, err
		}
		w := s.width()
		comp.tables = append(comp.tables, &srcTable{
			alias: alias, sheet: sheet, schema: s, offset: offset, width: w,
		})
		offset += w
	}
	return comp, joins, nil
}

// resolveJoinKeys interprets each ON clause as one or more equalities between
// columns of an already-composed table and columns of the table being joined.
// Several equalities become a composite key, matched as one concatenated value.
func (comp *composition) resolveJoinKeys(joins []*sqlparser.JoinTableExpr) error {
	for i, j := range joins {
		right := comp.tables[i+1]

		switch strings.ToLower(j.Join) {
		case "join", "inner join", "straight_join":
			right.kind = joinInner
		case "left join", "left outer join":
			right.kind = joinLeft
		default:
			return unsupported(strings.ToUpper(j.Join))
		}
		if len(j.Condition.Using) > 0 {
			return unsupported("USING; write an explicit ON clause")
		}
		if j.Condition.On == nil {
			return unsupported("a join without an ON clause")
		}
		var eqs []*sqlparser.ComparisonExpr
		if err := collectEqualities(j.Condition.On, &eqs); err != nil {
			return err
		}
		for _, cmp := range eqs {
			lc, ok1 := cmp.Left.(*sqlparser.ColName)
			rc, ok2 := cmp.Right.(*sqlparser.ColName)
			if !ok1 || !ok2 {
				return unsupported("join conditions that are not column = column")
			}

			// Whichever side names the table being joined is the lookup key.
			lTab, lCol, lerr := comp.locate(lc)
			rTab, rCol, rerr := comp.locate(rc)
			if lerr != nil {
				return lerr
			}
			if rerr != nil {
				return rerr
			}
			switch {
			case rTab == right && lTab != right:
				right.rightKeys = append(right.rightKeys, rCol)
				right.leftKeys = append(right.leftKeys, lTab.offset+colIndex(lCol.Letter)+1)
			case lTab == right && rTab != right:
				right.rightKeys = append(right.rightKeys, lCol)
				right.leftKeys = append(right.leftKeys, rTab.offset+colIndex(rCol.Letter)+1)
			default:
				return fmt.Errorf("sheetsql: the ON clause for %q must compare its columns to an earlier table", right.alias)
			}
		}
	}
	return nil
}

// collectEqualities flattens an ON clause of ANDed equalities.
func collectEqualities(e sqlparser.Expr, out *[]*sqlparser.ComparisonExpr) error {
	switch n := e.(type) {
	case *sqlparser.AndExpr:
		if err := collectEqualities(n.Left, out); err != nil {
			return err
		}
		return collectEqualities(n.Right, out)
	case *sqlparser.ParenExpr:
		return collectEqualities(n.Expr, out)
	case *sqlparser.ComparisonExpr:
		if n.Operator != "=" {
			return unsupported("join conditions other than equality")
		}
		*out = append(*out, n)
		return nil
	}
	return unsupported("this join condition")
}

func (comp *composition) locate(cn *sqlparser.ColName) (*srcTable, column, error) {
	q := cn.Qualifier.Name.String()
	if q != "" {
		t := comp.find(q)
		if t == nil {
			return nil, column{}, fmt.Errorf("sheetsql: unknown table %q", q)
		}
		col, ok := t.schema.lookup(cn.Name.String())
		if !ok {
			return nil, column{}, fmt.Errorf("sheetsql: no column %q in %q", cn.Name.String(), t.alias)
		}
		return t, col, nil
	}
	var found *srcTable
	var col column
	for _, t := range comp.tables {
		if cc, ok := t.schema.lookup(cn.Name.String()); ok {
			if found != nil {
				return nil, column{}, fmt.Errorf("sheetsql: column %q is ambiguous", cn.Name.String())
			}
			found, col = t, cc
		}
	}
	if found == nil {
		return nil, column{}, fmt.Errorf("sheetsql: no column %q", cn.Name.String())
	}
	return found, col, nil
}

// baseSource renders the left-most table, trimming trailing blank rows so that
// aggregates do not pick up an empty group.
func (t *srcTable) baseSource() string {
	full := a1Range(t.sheet, 1, 0, t.width-1)
	guard := a1Column(t.sheet, 1, colIndex(t.schema.Columns[0].Letter))
	return fmt.Sprintf("FILTER(%s,LEN(%s)>0)", full, guard)
}

// lookupSource renders {key, all-columns} so VLOOKUP can search on the join key
// and return any column of the table.
func (t *srcTable) lookupSource() string {
	return fmt.Sprintf("{%s,%s}", t.keyColumn(), a1Range(t.sheet, 1, 0, t.width-1))
}

// keyColumn is the lookup table's key: a single column, or the parts of a
// composite key concatenated so VLOOKUP can match on one value.
func (t *srcTable) keyColumn() string {
	if len(t.rightKeys) == 1 {
		return a1Column(t.sheet, 1, colIndex(t.rightKeys[0].Letter))
	}
	parts := make([]string, 0, len(t.rightKeys))
	for _, k := range t.rightKeys {
		parts = append(parts, a1Column(t.sheet, 1, colIndex(k.Letter)))
	}
	return "ARRAYFORMULA(" + strings.Join(parts, `&"`+keySep+`"&`) + ")"
}

// leftKeyExpr is the probe side of the join, taken from the composed array.
func (t *srcTable) leftKeyExpr(src string) string {
	if len(t.leftKeys) == 1 {
		return fmt.Sprintf("INDEX(%s,0,%d)", src, t.leftKeys[0])
	}
	parts := make([]string, 0, len(t.leftKeys))
	for _, p := range t.leftKeys {
		parts = append(parts, fmt.Sprintf("INDEX(%s,0,%d)", src, p))
	}
	return "ARRAYFORMULA(" + strings.Join(parts, `&"`+keySep+`"&`) + ")"
}

// vlookupIndices selects every column of the joined table; index 1 is the key
// copy prepended by lookupSource.
func (t *srcTable) vlookupIndices() string {
	idx := make([]string, t.width)
	for i := range idx {
		idx[i] = strconv.Itoa(i + 2)
	}
	return "{" + strings.Join(idx, ",") + "}"
}

// compileFormula turns a parsed SELECT into one spreadsheet formula.
// compileFormula assembles a complete formula for one SELECT.
func compileFormula(comp *composition, sel *sqlparser.Select, args []driver.NamedValue) (string, []resultCol, bool, error) {
	binds, body, cols, bare, err := compileSelectParts(comp, sel, args, "_")
	if err != nil {
		return "", nil, false, err
	}
	return assembleLet(binds, body), cols, bare, nil
}

// assembleLet wraps bindings and a result expression in the LET that Google
// evaluates. The values API omits blank cells, so a row that is entirely NULL
// -- an unmatched LEFT JOIN, say -- would come back indistinguishable from no
// row at all. Substituting a sentinel keeps such rows addressable.
func assembleLet(binds []string, body string) string {
	binds = append(binds, "_r,"+body)
	tail := fmt.Sprintf("ARRAYFORMULA(IF(_r=\"\",%s,_r))", quoteFormulaString(formulaNull))
	return "=LET(" + strings.Join(binds, ",") + "," + tail + ")"
}

// compileSelectParts renders one SELECT into LET bindings plus the expression
// that produces its rows. Bindings are prefixed so several selects can share
// one LET scope, as UNION requires.
func compileSelectParts(comp *composition, sel *sqlparser.Select, args []driver.NamedValue, p string) ([]string, string, []resultCol, bool, error) {
	tr := &translator{src: comp, args: args, multi: true}

	var binds []string
	prev := p + "s0"
	binds = append(binds, prev+","+comp.tables[0].baseSource())

	for i, t := range comp.tables[1:] {
		lk := fmt.Sprintf("%sl%d", p, i+1)
		cur := fmt.Sprintf("%ss%d", p, i+1)
		binds = append(binds, lk+","+t.lookupSource())
		binds = append(binds, fmt.Sprintf("%s,{%s,ARRAYFORMULA(IFNA(VLOOKUP(%s,%s,%s,FALSE),\"\"))}",
			cur, prev, t.leftKeyExpr(prev), lk, t.vlookupIndices()))
		prev = cur
	}

	// An inner join drops rows with no match; VLOOKUP alone cannot, so the
	// unmatched rows are filtered out afterwards by testing the key.
	var guards []string
	for _, t := range comp.tables[1:] {
		if t.kind == joinInner {
			guards = append(guards, fmt.Sprintf("ISNUMBER(MATCH(%s,%s,0))",
				t.leftKeyExpr(prev), t.keyColumn()))
		}
	}
	if len(guards) > 0 {
		binds = append(binds, p+"f,FILTER("+prev+","+strings.Join(guards, ",")+")")
		prev = p + "f"
	}

	// Anything QUERY cannot evaluate -- CASE, or a function outside the small
	// gviz set -- is computed first as extra columns of the source array.
	extra, pre, err := precomputeColumns(comp, sel, prev, args)
	if err != nil {
		return nil, "", nil, false, err
	}
	if len(extra) > 0 {
		exprs := make([]string, 0, len(extra)+1)
		exprs = append(exprs, prev)
		for i, x := range extra {
			name := fmt.Sprintf("%sx%d", p, i+1)
			binds = append(binds, name+",ARRAYFORMULA("+x+")")
			exprs = append(exprs, name)
		}
		binds = append(binds, p+"e,{"+strings.Join(exprs, ",")+"}")
		prev = p + "e"
	}
	tr.precomputed = pre

	q, cols, bare, err := tr.compileQuery(sel, prev)
	if err != nil {
		return nil, "", nil, false, err
	}
	return binds, q, cols, bare, nil
}

// compileQuery renders the QUERY() call(s) over the composed array. A HAVING
// clause becomes a second QUERY wrapping the first, because the Visualization
// language has no HAVING of its own.
func (t *translator) compileQuery(sel *sqlparser.Select, src string) (string, []resultCol, bool, error) {
	inner, err := t.translateSelect(sel)
	if err != nil {
		return "", nil, false, err
	}

	if sel.Having == nil {
		return fmt.Sprintf("QUERY(%s,%s,0)", src, quoteFormulaString(inner.TQ+labelClause(inner.Idents))),
			inner.Cols, inner.BareAggregate, nil
	}

	// Inside the wrapper, each projected expression is addressable only by its
	// position in the inner result.
	remap := map[string]string{}
	for i, id := range inner.Idents {
		remap[id] = colRef(i)
	}
	outer := &translator{src: t.src, args: t.args, multi: true, remap: remap}
	cond, err := outer.condition(sel.Having.Expr)
	if err != nil {
		return "", nil, false, err
	}

	idents := make([]string, len(inner.Idents))
	for i := range inner.Idents {
		idents[i] = colRef(i)
	}
	q := "select " + strings.Join(idents, ", ") + " where " + cond
	tail, err := outer.tailClauses(sel, true)
	if err != nil {
		return "", nil, false, err
	}
	q += tail
	q += labelClause(idents)

	innerQ := fmt.Sprintf("QUERY(%s,%s,0)", src, quoteFormulaString(inner.TQ+labelClause(inner.Idents)))
	// A HAVING clause filters groups, so the result is legitimately empty when
	// no group qualifies; there is no single row to synthesise.
	return fmt.Sprintf("QUERY(%s,%s,0)", innerQ, quoteFormulaString(q)), inner.Cols, false, nil
}

// labelClause blanks every generated header. QUERY emits a header row for
// aggregate columns even when told the source has none, which would otherwise
// arrive as a spurious first row of data.
func labelClause(idents []string) string {
	seen := map[string]bool{}
	var parts []string
	for _, id := range idents {
		if seen[id] {
			continue
		}
		seen[id] = true
		parts = append(parts, id+" ''")
	}
	if len(parts) == 0 {
		return ""
	}
	return " label " + strings.Join(parts, ", ")
}

// quoteFormulaString renders a Go string as a formula string literal.
func quoteFormulaString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// formulaNull marks a cell the compiled formula produced as blank. U+2400 is
// the Unicode symbol for NUL and does not occur in spreadsheet data.
const formulaNull = "\u2400"

// gridRows adapts the rectangle a formula produced into driver.Rows.
type gridRows struct {
	cols  []resultCol
	grid  [][]any
	pos   int
	names []string
}

func newGridRows(grid [][]any, cols []resultCol, bareAggregate bool) *gridRows {
	// SQL requires one row from an aggregate with no GROUP BY even when
	// nothing matched; the spreadsheet returns an empty result instead.
	if bareAggregate && len(grid) == 0 {
		row := make([]any, len(cols))
		for i, c := range cols {
			if c.Agg == "count" {
				row[i] = float64(0)
			}
		}
		grid = [][]any{row}
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
		if names[i] == "" {
			names[i] = colRef(i)
		}
	}
	return &gridRows{cols: cols, grid: grid, names: names}
}

func (r *gridRows) Columns() []string { return r.names }
func (r *gridRows) Close() error      { r.grid = nil; return nil }

func (r *gridRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.grid) {
		return io.EOF
	}
	row := r.grid[r.pos]
	r.pos++
	for i := range dest {
		if i >= len(row) {
			dest[i] = nil
			continue
		}
		v, err := convertGridCell(row[i], r.cols[i].Type)
		if err != nil {
			return fmt.Errorf("sheetsql: column %q: %w", r.names[i], err)
		}
		dest[i] = v
	}
	return nil
}

// convertGridCell maps a raw cell from the values API onto a driver value.
// Unmatched outer-join columns arrive as empty strings, which become NULL.
func convertGridCell(v any, typ string) (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok && (s == "" || s == formulaNull) {
		return nil, nil
	}
	if typ == "" {
		// A computed column has no declared type; take it from the value.
		switch x := v.(type) {
		case bool:
			return x, nil
		case float64:
			if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
				return int64(x), nil
			}
			return x, nil
		case string:
			return x, nil
		}
		return fmt.Sprint(v), nil
	}
	switch typ {
	case "number":
		f, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("expected a number, got %T (%v)", v, v)
		}
		if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return int64(f), nil
		}
		return f, nil
	case "boolean":
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			return strconv.ParseBool(x)
		}
		return nil, fmt.Errorf("expected a boolean, got %T", v)
	case "date", "datetime":
		if f, ok := toFloat(v); ok {
			return serialToTime(f), nil
		}
		return fmt.Sprint(v), nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return fmt.Sprint(v), nil
}

// precomputeColumns finds every expression the query language cannot evaluate,
// renders it as an array formula, and reports where each will sit in the
// extended array.
func precomputeColumns(comp *composition, sel *sqlparser.Select, src string, args []driver.NamedValue) ([]string, map[string]precomputedCol, error) {
	width := 0
	for _, t := range comp.tables {
		width += t.width
	}

	r := &sheetRenderer{comp: comp, src: src, args: args}
	pre := map[string]precomputedCol{}
	var out []string

	var walkErr error
	visit := func(e sqlparser.Expr) {
		if walkErr != nil || !isSheetOnly(e) {
			return
		}
		key := sqlparser.String(e)
		if _, seen := pre[key]; seen {
			return
		}
		rendered, err := r.render(e)
		if err != nil {
			walkErr = err
			return
		}
		pre[key] = precomputedCol{Ident: colRef(width + len(out)), Type: sheetExprType(e)}
		out = append(out, rendered)
	}

	forEachExpr(sel, visit)
	if walkErr != nil {
		return nil, nil, walkErr
	}
	return out, pre, nil
}

type precomputedCol struct {
	Ident string
	Type  string
}

// isSheetOnly reports whether an expression must be computed by the formula
// engine because the query language has no equivalent.
func isSheetOnly(e sqlparser.Expr) bool {
	switch n := e.(type) {
	case *sqlparser.CaseExpr, *sqlparser.SubstrExpr:
		return true
	case *sqlparser.FuncExpr:
		name := strings.ToLower(n.Name.String())
		if _, ok := gvizAggregates[name]; ok {
			return false
		}
		if _, ok := gvizScalars[name]; ok {
			return false
		}
		return true
	}
	return false
}

// sheetExprType guesses the type of a computed column so values convert back
// sensibly. CASE may yield either, so it is left unknown.
func sheetExprType(e sqlparser.Expr) string {
	if _, ok := e.(*sqlparser.SubstrExpr); ok {
		return "string"
	}
	if fe, ok := e.(*sqlparser.FuncExpr); ok {
		switch strings.ToLower(fe.Name.String()) {
		case "abs", "sqrt", "power", "pow", "floor", "ceil", "ceiling", "round",
			"mod", "exp", "ln", "log10", "sign", "length", "char_length",
			"character_length", "year", "month", "day", "dayofmonth", "hour",
			"minute", "second":
			return "number"
		case "concat", "upper", "ucase", "lower", "lcase", "trim", "ltrim",
			"rtrim", "substr", "substring", "mid", "left", "right", "replace":
			return "string"
		}
	}
	return ""
}

// forEachExpr visits the expressions of a SELECT that may need precomputing.
func forEachExpr(sel *sqlparser.Select, fn func(sqlparser.Expr)) {
	walk := func(e sqlparser.Expr) {
		if e == nil {
			return
		}
		sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
			if ex, ok := node.(sqlparser.Expr); ok {
				fn(ex)
			}
			return true, nil
		}, e)
	}
	for _, se := range sel.SelectExprs {
		if ae, ok := se.(*sqlparser.AliasedExpr); ok {
			walk(ae.Expr)
		}
	}
	if sel.Where != nil {
		walk(sel.Where.Expr)
	}
	for _, g := range sel.GroupBy {
		walk(g)
	}
	if sel.Having != nil {
		walk(sel.Having.Expr)
	}
	for _, o := range sel.OrderBy {
		walk(o.Expr)
	}
}

// needsFormula reports whether a statement exceeds what one gviz query can do.
func needsFormula(sel *sqlparser.Select) bool {
	if sel.Having != nil {
		return true
	}
	if len(sel.From) != 1 {
		return true
	}
	if _, isJoin := sel.From[0].(*sqlparser.JoinTableExpr); isJoin {
		return true
	}
	found := false
	forEachExpr(sel, func(e sqlparser.Expr) {
		if isSheetOnly(e) {
			found = true
		}
	})
	return found
}

// queryFormula compiles and runs a statement through the formula engine.
func (c *conn) queryFormula(ctx context.Context, sel *sqlparser.Select, args []driver.NamedValue) (driver.Rows, error) {
	comp, joins, err := c.buildComposition(ctx, sel)
	if err != nil {
		return nil, err
	}
	if err := comp.resolveJoinKeys(joins); err != nil {
		return nil, err
	}
	formula, cols, bare, err := compileFormula(comp, sel, args)
	if err != nil {
		return nil, err
	}
	grid, err := c.evalFormula(ctx, formula, len(cols))
	if err != nil {
		return nil, err
	}
	return newGridRows(grid, cols, bare), nil
}

func (r *gridRows) ColumnTypeDatabaseTypeName(i int) string {
	if r.cols[i].Type == "" {
		return "TEXT"
	}
	return strings.ToUpper(r.cols[i].Type)
}

func (r *gridRows) ColumnTypeScanType(i int) reflect.Type {
	switch r.cols[i].Type {
	case "number":
		return reflect.TypeOf(float64(0))
	case "boolean":
		return reflect.TypeOf(false)
	case "date", "datetime":
		return reflect.TypeOf(time.Time{})
	default:
		return reflect.TypeOf("")
	}
}

func (r *gridRows) ColumnTypeNullable(i int) (nullable, ok bool) { return true, true }
