package sheetsql

import (
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xwb1989/sqlparser"
)

type resultCol struct {
	Name string
	Type string // gviz type, "" when computed
	Agg  string // aggregate function producing this column, if any
}

type translation struct {
	Sheet  string
	TQ     string
	Cols   []resultCol
	Idents []string // rendered projection expressions, in order

	// BareAggregate marks a query whose projection is entirely aggregates with
	// no GROUP BY. SQL requires exactly one result row for such a query; gviz
	// returns none when nothing matches the WHERE clause.
	BareAggregate bool
}

// colSource resolves SQL column references to identifiers the Google
// Visualization query language understands. A single-sheet query resolves them
// to spreadsheet column letters; a compiled join resolves them to the ColN
// positions of the composed array.
type colSource interface {
	// column resolves a possibly table-qualified reference.
	column(qualifier, name string) (ident, typ string, err error)
	// star expands "*" or "t.*" in a select list.
	star(qualifier string) ([]projected, error)
	// first is the identifier used for count(*), which gviz cannot express.
	first() string
	describe() string
}

type projected struct {
	Ident string
	Name  string
	Type  string
}

type translator struct {
	src  colSource
	args []driver.NamedValue
	// multi allows a join; the single-sheet path must reject one.
	multi bool
	// remap rewrites rendered identifiers, used when a HAVING clause has to
	// address the columns of an inner query by position.
	remap map[string]string
}

func (s *schema) column(qualifier, name string) (string, string, error) {
	c, ok := s.lookup(name)
	if !ok {
		return "", "", fmt.Errorf("sheetsql: no column %q in sheet %q (have: %s)",
			name, s.Sheet, strings.Join(s.names(), ", "))
	}
	return c.Letter, c.Type, nil
}

func (s *schema) star(qualifier string) ([]projected, error) {
	out := make([]projected, 0, len(s.Columns))
	for _, c := range s.Columns {
		out = append(out, projected{Ident: c.Letter, Name: c.Label, Type: c.Type})
	}
	return out, nil
}

func (s *schema) first() string    { return s.Columns[0].Letter }
func (s *schema) describe() string { return s.Sheet }

func unsupported(what string) error {
	return fmt.Errorf("sheetsql: %s is not supported by the Google Visualization query language", what)
}

// sheetFromSelect extracts the single table name; gviz addresses one tab per query.
func sheetFromSelect(sel *sqlparser.Select) (string, error) {
	if len(sel.From) != 1 {
		return "", unsupported("joins / multiple tables")
	}
	if _, isJoin := sel.From[0].(*sqlparser.JoinTableExpr); isJoin {
		return "", unsupported("joins")
	}
	ate, ok := sel.From[0].(*sqlparser.AliasedTableExpr)
	if !ok {
		return "", unsupported("this FROM clause")
	}
	tn, ok := ate.Expr.(sqlparser.TableName)
	if !ok {
		return "", unsupported("subqueries in FROM")
	}
	return tn.Name.String(), nil
}

func (t *translator) translateSelect(sel *sqlparser.Select) (*translation, error) {
	// Re-checked here so the guarantee does not depend on the caller having
	// resolved the sheet first.
	if !t.multi {
		if _, err := sheetFromSelect(sel); err != nil {
			return nil, err
		}
		if sel.Having != nil {
			return nil, unsupported("HAVING")
		}
	}
	if len(sel.SelectExprs) == 0 {
		return nil, fmt.Errorf("sheetsql: empty select list")
	}

	var sb strings.Builder
	var cols []resultCol
	var projected []string

	for _, se := range sel.SelectExprs {
		switch e := se.(type) {
		case *sqlparser.StarExpr:
			stars, err := t.src.star(e.TableName.Name.String())
			if err != nil {
				return nil, err
			}
			for _, p := range stars {
				projected = append(projected, p.Ident)
				cols = append(cols, resultCol{Name: p.Name, Type: p.Type})
			}
		case *sqlparser.AliasedExpr:
			expr, typ, err := t.expr(e.Expr)
			if err != nil {
				return nil, err
			}
			name := e.As.String()
			if name == "" {
				name = defaultColName(e.Expr, t.src)
			}
			projected = append(projected, expr)
			cols = append(cols, resultCol{Name: name, Type: typ, Agg: aggregateOf(e.Expr)})
		default:
			return nil, unsupported("this select expression")
		}
	}

	sb.WriteString("select ")
	sb.WriteString(strings.Join(projected, ", "))

	if sel.Where != nil {
		w, err := t.condition(sel.Where.Expr)
		if err != nil {
			return nil, err
		}
		sb.WriteString(" where ")
		sb.WriteString(w)
	}

	groupBy := make([]string, 0, len(sel.GroupBy))
	for _, g := range sel.GroupBy {
		s, _, err := t.expr(g)
		if err != nil {
			return nil, err
		}
		groupBy = append(groupBy, s)
	}
	// SELECT DISTINCT has no gviz equivalent, but grouping by every projected
	// column is exactly equivalent when the projection has no aggregates.
	if sel.Distinct != "" {
		if len(groupBy) > 0 {
			return nil, unsupported("DISTINCT combined with GROUP BY")
		}
		if hasAggregate(sel.SelectExprs) {
			return nil, unsupported("DISTINCT over aggregates")
		}
		groupBy = append(groupBy, projected...)
	}
	if len(groupBy) > 0 {
		sb.WriteString(" group by ")
		sb.WriteString(strings.Join(groupBy, ", "))
	}

	// With a HAVING clause the ordering and limit belong to the wrapper query,
	// which is applied after grouping.
	if sel.Having == nil || !t.multi {
		tail, err := t.tailClauses(sel, false)
		if err != nil {
			return nil, err
		}
		sb.WriteString(tail)
	}

	return &translation{
		Sheet:         t.src.describe(),
		TQ:            sb.String(),
		Cols:          cols,
		Idents:        projected,
		BareAggregate: sel.Distinct == "" && len(sel.GroupBy) == 0 && hasAggregate(sel.SelectExprs),
	}, nil
}

// scalar renders a scalar function call that gviz evaluates server-side.
func (t *translator) scalar(name string, sf scalarFn, n *sqlparser.FuncExpr) (string, string, error) {
	if len(n.Exprs) != sf.nargs {
		return "", "", fmt.Errorf("sheetsql: %s() takes %d argument(s), got %d",
			name, sf.nargs, len(n.Exprs))
	}
	args := make([]string, 0, len(n.Exprs))
	for _, se := range n.Exprs {
		ae, ok := se.(*sqlparser.AliasedExpr)
		if !ok {
			return "", "", unsupported("this argument to " + name + "()")
		}
		a, _, err := t.expr(ae.Expr)
		if err != nil {
			return "", "", err
		}
		args = append(args, a)
	}
	call := sf.gviz + "(" + strings.Join(args, ", ") + ")"
	if sf.oneBased {
		return "(" + call + " + 1)", sf.typ, nil
	}
	return call, sf.typ, nil
}

// tailClauses renders ORDER BY, LIMIT and OFFSET.
func (t *translator) tailClauses(sel *sqlparser.Select, _ bool) (string, error) {
	var sb strings.Builder
	if len(sel.OrderBy) > 0 {
		parts := make([]string, 0, len(sel.OrderBy))
		for _, o := range sel.OrderBy {
			s, _, err := t.expr(o.Expr)
			if err != nil {
				return "", err
			}
			dir := "asc"
			if strings.EqualFold(o.Direction, "desc") {
				dir = "desc"
			}
			parts = append(parts, s+" "+dir)
		}
		sb.WriteString(" order by ")
		sb.WriteString(strings.Join(parts, ", "))
	}
	if sel.Limit != nil {
		if sel.Limit.Rowcount != nil {
			n, err := t.intLiteral(sel.Limit.Rowcount)
			if err != nil {
				return "", err
			}
			sb.WriteString(" limit " + strconv.FormatInt(n, 10))
		}
		if sel.Limit.Offset != nil {
			n, err := t.intLiteral(sel.Limit.Offset)
			if err != nil {
				return "", err
			}
			sb.WriteString(" offset " + strconv.FormatInt(n, 10))
		}
	}
	return sb.String(), nil
}

// aggregateOf reports the aggregate function an expression applies, if any.
func aggregateOf(e sqlparser.Expr) string {
	fe, ok := e.(*sqlparser.FuncExpr)
	if !ok {
		return ""
	}
	return gvizAggregates[strings.ToLower(fe.Name.String())]
}

func hasAggregate(exprs sqlparser.SelectExprs) bool {
	found := false
	for _, se := range exprs {
		ae, ok := se.(*sqlparser.AliasedExpr)
		if !ok {
			continue
		}
		if fe, ok := ae.Expr.(*sqlparser.FuncExpr); ok {
			if _, agg := gvizAggregates[strings.ToLower(fe.Name.String())]; agg {
				found = true
			}
		}
	}
	return found
}

var gvizAggregates = map[string]string{
	"sum": "sum", "avg": "avg", "count": "count", "max": "max", "min": "min",
}

// scalarFn is a SQL scalar function that gviz can evaluate server-side.
type scalarFn struct {
	gviz  string
	typ   string
	nargs int
	// oneBased corrects gviz's zero-based month() against SQL's MONTH(),
	// which numbers January as 1.
	oneBased bool
}

var gvizScalars = map[string]scalarFn{
	"year":       {gviz: "year", typ: "number", nargs: 1},
	"month":      {gviz: "month", typ: "number", nargs: 1, oneBased: true},
	"day":        {gviz: "day", typ: "number", nargs: 1},
	"dayofmonth": {gviz: "day", typ: "number", nargs: 1},
	"hour":       {gviz: "hour", typ: "number", nargs: 1},
	"minute":     {gviz: "minute", typ: "number", nargs: 1},
	"second":     {gviz: "second", typ: "number", nargs: 1},
	"quarter":    {gviz: "quarter", typ: "number", nargs: 1},
	"dayofweek":  {gviz: "dayOfWeek", typ: "number", nargs: 1},
	"upper":      {gviz: "upper", typ: "string", nargs: 1},
	"ucase":      {gviz: "upper", typ: "string", nargs: 1},
	"lower":      {gviz: "lower", typ: "string", nargs: 1},
	"lcase":      {gviz: "lower", typ: "string", nargs: 1},
	"date":       {gviz: "toDate", typ: "date", nargs: 1},
	"now":        {gviz: "now", typ: "datetime", nargs: 0},
	"datediff":   {gviz: "dateDiff", typ: "number", nargs: 2},
}

func defaultColName(e sqlparser.Expr, src colSource) string {
	if cn, ok := e.(*sqlparser.ColName); ok {
		return cn.Name.String()
	}
	return strings.TrimSpace(sqlparser.String(e))
}

// condition renders a boolean expression into a gviz where clause.
func (t *translator) condition(e sqlparser.Expr) (string, error) {
	switch n := e.(type) {
	case *sqlparser.AndExpr:
		l, err := t.condition(n.Left)
		if err != nil {
			return "", err
		}
		r, err := t.condition(n.Right)
		if err != nil {
			return "", err
		}
		return "(" + l + " and " + r + ")", nil
	case *sqlparser.OrExpr:
		l, err := t.condition(n.Left)
		if err != nil {
			return "", err
		}
		r, err := t.condition(n.Right)
		if err != nil {
			return "", err
		}
		return "(" + l + " or " + r + ")", nil
	case *sqlparser.NotExpr:
		s, err := t.condition(n.Expr)
		if err != nil {
			return "", err
		}
		return "not " + s, nil
	case *sqlparser.ParenExpr:
		s, err := t.condition(n.Expr)
		if err != nil {
			return "", err
		}
		return "(" + s + ")", nil
	case *sqlparser.IsExpr:
		s, _, err := t.expr(n.Expr)
		if err != nil {
			return "", err
		}
		switch strings.ToLower(n.Operator) {
		case "is null":
			return s + " is null", nil
		case "is not null":
			return s + " is not null", nil
		case "is true":
			return s + " = true", nil
		case "is false":
			return s + " = false", nil
		}
		return "", unsupported("IS " + n.Operator)
	case *sqlparser.RangeCond:
		return t.between(n)
	case *sqlparser.ComparisonExpr:
		return t.comparison(n)
	case *sqlparser.BoolVal:
		if bool(*n) {
			return "true", nil
		}
		return "false", nil
	}
	return "", unsupported(fmt.Sprintf("condition %s", sqlparser.String(e)))
}

func (t *translator) between(n *sqlparser.RangeCond) (string, error) {
	col, typ, err := t.expr(n.Left)
	if err != nil {
		return "", err
	}
	lo, err := t.value(n.From, typ)
	if err != nil {
		return "", err
	}
	hi, err := t.value(n.To, typ)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(n.Operator, "not between") {
		return fmt.Sprintf("(%s < %s or %s > %s)", col, lo, col, hi), nil
	}
	return fmt.Sprintf("(%s >= %s and %s <= %s)", col, lo, col, hi), nil
}

func (t *translator) comparison(n *sqlparser.ComparisonExpr) (string, error) {
	left, typ, err := t.expr(n.Left)
	if err != nil {
		return "", err
	}
	op := strings.ToLower(n.Operator)

	// gviz has no IN; expand into an OR / AND chain over the tuple.
	if op == "in" || op == "not in" {
		tuple, ok := n.Right.(sqlparser.ValTuple)
		if !ok {
			return "", unsupported("IN with a non-literal list")
		}
		if len(tuple) == 0 {
			return "", fmt.Errorf("sheetsql: empty IN list")
		}
		cmp, join := "=", " or "
		if op == "not in" {
			cmp, join = "!=", " and "
		}
		parts := make([]string, 0, len(tuple))
		for _, v := range tuple {
			lit, err := t.value(v, typ)
			if err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf("%s %s %s", left, cmp, lit))
		}
		return "(" + strings.Join(parts, join) + ")", nil
	}

	right, err := t.value(n.Right, typ)
	if err != nil {
		return "", err
	}
	switch op {
	case "=", "<", ">", "<=", ">=":
		return fmt.Sprintf("%s %s %s", left, op, right), nil
	case "!=", "<>":
		return fmt.Sprintf("%s != %s", left, right), nil
	case "like":
		return fmt.Sprintf("%s like %s", left, right), nil
	case "not like":
		return fmt.Sprintf("not %s like %s", left, right), nil
	case "regexp":
		return fmt.Sprintf("%s matches %s", left, right), nil
	case "not regexp":
		return fmt.Sprintf("not %s matches %s", left, right), nil
	}
	return "", unsupported("operator " + n.Operator)
}

// expr renders a scalar expression and reports the gviz type it yields,
// applying any identifier remapping the caller installed.
func (t *translator) expr(e sqlparser.Expr) (string, string, error) {
	s, ty, err := t.exprRaw(e)
	if err != nil {
		return "", "", err
	}
	if t.remap != nil {
		if m, ok := t.remap[s]; ok {
			return m, ty, nil
		}
	}
	return s, ty, nil
}

func (t *translator) exprRaw(e sqlparser.Expr) (string, string, error) {
	switch n := e.(type) {
	case *sqlparser.ColName:
		return t.src.column(n.Qualifier.Name.String(), n.Name.String())

	case *sqlparser.FuncExpr:
		fn := strings.ToLower(n.Name.String())
		if sf, ok := gvizScalars[fn]; ok {
			return t.scalar(fn, sf, n)
		}
		gfn, ok := gvizAggregates[fn]
		if !ok {
			return "", "", unsupported("function " + fn + "()")
		}
		if len(n.Exprs) != 1 {
			return "", "", fmt.Errorf("sheetsql: %s() takes one argument", fn)
		}
		// gviz has no count(*): count any real column. It counts non-null
		// cells, so a fully blank row is not counted.
		if _, star := n.Exprs[0].(*sqlparser.StarExpr); star {
			if gfn != "count" {
				return "", "", fmt.Errorf("sheetsql: %s(*) is not meaningful", fn)
			}
			return "count(" + t.src.first() + ")", "number", nil
		}
		ae, ok := n.Exprs[0].(*sqlparser.AliasedExpr)
		if !ok {
			return "", "", unsupported("this aggregate argument")
		}
		inner, _, err := t.expr(ae.Expr)
		if err != nil {
			return "", "", err
		}
		return gfn + "(" + inner + ")", "number", nil

	case *sqlparser.BinaryExpr:
		switch n.Operator {
		case "+", "-", "*", "/":
			l, _, err := t.expr(n.Left)
			if err != nil {
				return "", "", err
			}
			r, _, err := t.expr(n.Right)
			if err != nil {
				return "", "", err
			}
			return "(" + l + " " + n.Operator + " " + r + ")", "number", nil
		}
		return "", "", unsupported("operator " + n.Operator)

	case *sqlparser.ParenExpr:
		s, ty, err := t.expr(n.Expr)
		if err != nil {
			return "", "", err
		}
		return "(" + s + ")", ty, nil

	case *sqlparser.SQLVal, *sqlparser.NullVal, sqlparser.BoolVal:
		s, err := t.value(e, "")
		return s, "", err
	}
	return "", "", unsupported(fmt.Sprintf("expression %s", sqlparser.String(e)))
}

func (t *translator) intLiteral(e sqlparser.Expr) (int64, error) {
	s, err := t.value(e, "number")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sheetsql: LIMIT/OFFSET must be an integer, got %s", s)
	}
	return n, nil
}

// value renders a literal, using typeHint to decide whether a string needs a
// gviz date/datetime/timeofday constructor.
func (t *translator) value(e sqlparser.Expr, typeHint string) (string, error) {
	switch n := e.(type) {
	case *sqlparser.NullVal:
		return "null", nil
	case sqlparser.BoolVal:
		if bool(n) {
			return "true", nil
		}
		return "false", nil
	case *sqlparser.SQLVal:
		switch n.Type {
		case sqlparser.IntVal, sqlparser.FloatVal:
			return string(n.Val), nil
		case sqlparser.StrVal:
			return typedLiteral(string(n.Val), typeHint)
		case sqlparser.ValArg:
			v, err := t.arg(n)
			if err != nil {
				return "", err
			}
			return goValueLiteral(v, typeHint)
		}
		return "", unsupported("this literal type")
	case *sqlparser.UnaryExpr:
		if n.Operator == "-" {
			s, err := t.value(n.Expr, typeHint)
			if err != nil {
				return "", err
			}
			return "-" + s, nil
		}
	case *sqlparser.ColName:
		s, _, err := t.expr(n)
		return s, err
	}
	return "", unsupported(fmt.Sprintf("value %s", sqlparser.String(e)))
}

// arg resolves a ":vN" placeholder to the supplied argument.
func (t *translator) arg(v *sqlparser.SQLVal) (any, error) {
	name := strings.TrimPrefix(string(v.Val), ":")
	idx, err := strconv.Atoi(strings.TrimPrefix(name, "v"))
	if err != nil {
		return nil, fmt.Errorf("sheetsql: unsupported placeholder %q", v.Val)
	}
	if idx < 1 || idx > len(t.args) {
		return nil, fmt.Errorf("sheetsql: missing argument %d (got %d)", idx, len(t.args))
	}
	return t.args[idx-1].Value, nil
}

func goValueLiteral(v any, typeHint string) (string, error) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case []byte:
		return typedLiteral(string(x), typeHint)
	case string:
		return typedLiteral(x, typeHint)
	case time.Time:
		switch typeHint {
		case "date":
			return "date '" + x.Format("2006-01-02") + "'", nil
		case "timeofday":
			return "timeofday '" + x.Format("15:04:05") + "'", nil
		default:
			return "datetime '" + x.Format("2006-01-02 15:04:05") + "'", nil
		}
	}
	return "", fmt.Errorf("sheetsql: cannot bind %T", v)
}

func typedLiteral(s, typeHint string) (string, error) {
	switch typeHint {
	case "date", "datetime", "timeofday":
		q, err := quote(s)
		if err != nil {
			return "", err
		}
		return typeHint + " " + q, nil
	case "number":
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			return s, nil
		}
	case "boolean":
		if b, err := strconv.ParseBool(s); err == nil {
			return strconv.FormatBool(b), nil
		}
	}
	return quote(s)
}

// quote renders a string literal. gviz accepts both quote styles but defines no
// escape sequence, so a value containing both cannot be expressed.
func quote(s string) (string, error) {
	hasSingle := strings.Contains(s, "'")
	hasDouble := strings.Contains(s, `"`)
	switch {
	case hasSingle && hasDouble:
		return "", fmt.Errorf("sheetsql: string literal contains both quote characters, which the gviz query language cannot escape: %q", s)
	case hasSingle:
		return `"` + s + `"`, nil
	default:
		return "'" + s + "'", nil
	}
}

func parseInt(s string) (any, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("sheetsql: bad integer %q", s)
	}
	return n, nil
}

func parseFloat(s string) (any, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("sheetsql: bad number %q", s)
	}
	return f, nil
}
