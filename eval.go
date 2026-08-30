package sheetsql

import (
	"database/sql/driver"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xwb1989/sqlparser"
)

// sheetEpoch is the origin Google Sheets uses for date serial numbers.
var sheetEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

func serialToTime(f float64) time.Time {
	days, frac := math.Modf(f)
	return sheetEpoch.AddDate(0, 0, int(days)).
		Add(time.Duration(math.Round(frac*24*60*60*1000)) * time.Millisecond)
}

// evaluator applies a WHERE clause locally. UPDATE and DELETE need the physical
// row numbers of the matches, which the Visualization endpoint never reports,
// so those statements cannot push their predicate server-side.
type evaluator struct {
	s    *schema
	args []driver.NamedValue
	row  []any // raw cell values for the current row, in sheet column order
}

func (e *evaluator) cell(c column) any {
	idx := colIndex(c.Letter)
	if idx < 0 || idx >= len(e.row) {
		return nil
	}
	v := e.row[idx]
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	switch c.Type {
	case "date", "datetime":
		if f, ok := toFloat(v); ok {
			return serialToTime(f)
		}
	case "number":
		if f, ok := toFloat(v); ok {
			return f
		}
	case "boolean":
		switch x := v.(type) {
		case bool:
			return x
		case string:
			if b, err := strconv.ParseBool(x); err == nil {
				return b
			}
		}
	}
	return v
}

func colIndex(letter string) int {
	n := 0
	for _, r := range letter {
		if r < 'A' || r > 'Z' {
			return -1
		}
		n = n*26 + int(r-'A') + 1
	}
	return n - 1
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func (e *evaluator) match(expr sqlparser.Expr) (bool, error) {
	if expr == nil {
		return true, nil
	}
	switch n := expr.(type) {
	case *sqlparser.AndExpr:
		l, err := e.match(n.Left)
		if err != nil || !l {
			return false, err
		}
		return e.match(n.Right)
	case *sqlparser.OrExpr:
		l, err := e.match(n.Left)
		if err != nil || l {
			return l, err
		}
		return e.match(n.Right)
	case *sqlparser.NotExpr:
		b, err := e.match(n.Expr)
		return !b, err
	case *sqlparser.ParenExpr:
		return e.match(n.Expr)
	case sqlparser.BoolVal:
		return bool(n), nil
	case *sqlparser.IsExpr:
		v, _, err := e.value(n.Expr)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(n.Operator) {
		case "is null":
			return v == nil, nil
		case "is not null":
			return v != nil, nil
		case "is true":
			return v == true, nil
		case "is false":
			return v == false, nil
		}
		return false, unsupported("IS " + n.Operator)
	case *sqlparser.RangeCond:
		lv, typ, err := e.value(n.Left)
		if err != nil {
			return false, err
		}
		lo, err := e.typed(n.From, typ)
		if err != nil {
			return false, err
		}
		hi, err := e.typed(n.To, typ)
		if err != nil {
			return false, err
		}
		ge, err1 := compare(lv, lo)
		le, err2 := compare(lv, hi)
		if err1 != nil || err2 != nil {
			return false, nil
		}
		in := ge >= 0 && le <= 0
		if strings.EqualFold(n.Operator, "not between") {
			return !in, nil
		}
		return in, nil
	case *sqlparser.ComparisonExpr:
		return e.compareExpr(n)
	}
	return false, unsupported(fmt.Sprintf("condition %s", sqlparser.String(expr)))
}

func (e *evaluator) compareExpr(n *sqlparser.ComparisonExpr) (bool, error) {
	lv, typ, err := e.value(n.Left)
	if err != nil {
		return false, err
	}
	op := strings.ToLower(n.Operator)

	if op == "in" || op == "not in" {
		tuple, ok := n.Right.(sqlparser.ValTuple)
		if !ok {
			return false, unsupported("IN with a non-literal list")
		}
		found := false
		for _, item := range tuple {
			rv, err := e.typed(item, typ)
			if err != nil {
				return false, err
			}
			if c, err := compare(lv, rv); err == nil && c == 0 {
				found = true
				break
			}
		}
		return found == (op == "in"), nil
	}

	rv, err := e.typed(n.Right, typ)
	if err != nil {
		return false, err
	}

	switch op {
	case "like", "not like":
		s, ok := lv.(string)
		p, ok2 := rv.(string)
		if !ok || !ok2 {
			return false, nil
		}
		re, err := likeRegexp(p)
		if err != nil {
			return false, err
		}
		return re.MatchString(s) == (op == "like"), nil
	}

	// NULL compares equal to nothing, as in SQL.
	if lv == nil || rv == nil {
		return false, nil
	}
	c, err := compare(lv, rv)
	if err != nil {
		return false, nil
	}
	switch op {
	case "=":
		return c == 0, nil
	case "!=", "<>":
		return c != 0, nil
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	case ">=":
		return c >= 0, nil
	}
	return false, unsupported("operator " + n.Operator)
}

func (e *evaluator) value(expr sqlparser.Expr) (any, string, error) {
	switch n := expr.(type) {
	case *sqlparser.ColName:
		c, ok := e.s.lookup(n.Name.String())
		if !ok {
			return nil, "", fmt.Errorf("sheetsql: no column %q in sheet %q (have: %s)",
				n.Name.String(), e.s.Sheet, strings.Join(e.s.names(), ", "))
		}
		return e.cell(c), c.Type, nil
	case *sqlparser.ParenExpr:
		return e.value(n.Expr)
	}
	v, err := e.typed(expr, "")
	return v, "", err
}

// typed converts a literal to the Go type used for comparison against a column
// of the given gviz type.
func (e *evaluator) typed(expr sqlparser.Expr, typ string) (any, error) {
	switch n := expr.(type) {
	case *sqlparser.NullVal:
		return nil, nil
	case sqlparser.BoolVal:
		return bool(n), nil
	case *sqlparser.ColName:
		v, _, err := e.value(n)
		return v, err
	case *sqlparser.UnaryExpr:
		if n.Operator == "-" {
			v, err := e.typed(n.Expr, typ)
			if err != nil {
				return nil, err
			}
			if f, ok := toFloat(v); ok {
				return -f, nil
			}
		}
	case *sqlparser.SQLVal:
		switch n.Type {
		case sqlparser.IntVal, sqlparser.FloatVal:
			f, _ := strconv.ParseFloat(string(n.Val), 64)
			return f, nil
		case sqlparser.StrVal:
			return coerceLiteral(string(n.Val), typ)
		case sqlparser.ValArg:
			raw, err := e.argValue(n)
			if err != nil {
				return nil, err
			}
			return normaliseArg(raw, typ)
		}
	}
	return nil, unsupported(fmt.Sprintf("value %s", sqlparser.String(expr)))
}

func (e *evaluator) argValue(v *sqlparser.SQLVal) (any, error) {
	t := &translator{src: e.s, args: e.args}
	return t.arg(v)
}

func normaliseArg(raw any, typ string) (any, error) {
	switch x := raw.(type) {
	case nil:
		return nil, nil
	case bool:
		return x, nil
	case int64:
		return float64(x), nil
	case float64:
		return x, nil
	case time.Time:
		return x, nil
	case []byte:
		return coerceLiteral(string(x), typ)
	case string:
		return coerceLiteral(x, typ)
	}
	return nil, fmt.Errorf("sheetsql: cannot bind %T", raw)
}

func coerceLiteral(s, typ string) (any, error) {
	switch typ {
	case "number":
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, nil
		}
	case "boolean":
		if b, err := strconv.ParseBool(s); err == nil {
			return b, nil
		}
	case "date":
		if t, err := time.Parse("2006-01-02", s); err == nil {
			return t, nil
		}
	case "datetime":
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00", "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, nil
			}
		}
	}
	return s, nil
}

func compare(a, b any) (int, error) {
	if a == nil || b == nil {
		if a == nil && b == nil {
			return 0, nil
		}
		return 0, fmt.Errorf("null")
	}
	switch x := a.(type) {
	case float64:
		y, ok := toFloat(b)
		if !ok {
			return 0, fmt.Errorf("type mismatch")
		}
		switch {
		case x < y:
			return -1, nil
		case x > y:
			return 1, nil
		}
		return 0, nil
	case string:
		y, ok := b.(string)
		if !ok {
			return 0, fmt.Errorf("type mismatch")
		}
		return strings.Compare(x, y), nil
	case bool:
		y, ok := b.(bool)
		if !ok {
			return 0, fmt.Errorf("type mismatch")
		}
		if x == y {
			return 0, nil
		}
		if !x {
			return -1, nil
		}
		return 1, nil
	case time.Time:
		var y time.Time
		switch t := b.(type) {
		case time.Time:
			y = t
		case float64:
			y = serialToTime(t)
		default:
			return 0, fmt.Errorf("type mismatch")
		}
		switch {
		case x.Before(y):
			return -1, nil
		case x.After(y):
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("uncomparable %T", a)
}

func likeRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("(?s)^")
	for _, r := range pattern {
		switch r {
		case '%':
			sb.WriteString(".*")
		case '_':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}
