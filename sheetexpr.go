package sheetsql

import (
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xwb1989/sqlparser"
)

// The Visualization query language understands only a fixed handful of
// functions and has no CASE expression. The spreadsheet formula language has
// neither limit, so anything QUERY cannot express is computed first as an extra
// column of the source array and then referenced by position.
//
// sheetRenderer emits that array formula: column references become
// INDEX(<array>,0,n), which yields the whole column.

type sheetRenderer struct {
	comp *composition
	src  string // LET variable holding the array being extended
	args []driver.NamedValue
}

// sheetFuncs maps SQL scalar functions onto spreadsheet equivalents that take
// their arguments in the same order.
var sheetFuncs = map[string]string{
	"abs": "ABS", "sqrt": "SQRT", "power": "POWER", "pow": "POWER",
	"floor": "FLOOR", "ceil": "CEILING", "ceiling": "CEILING",
	"exp": "EXP", "ln": "LN", "log10": "LOG10", "sign": "SIGN",
	"upper": "UPPER", "ucase": "UPPER", "lower": "LOWER", "lcase": "LOWER",
	"trim": "TRIM", "ltrim": "TRIM", "rtrim": "TRIM",
	"length": "LEN", "char_length": "LEN", "character_length": "LEN",
	"year": "YEAR", "month": "MONTH", "day": "DAY", "dayofmonth": "DAY",
	"hour": "HOUR", "minute": "MINUTE", "second": "SECOND",
	"now": "NOW", "curdate": "TODAY",
}

// sheetFuncsVariadic take a different shape and are rendered individually.
func (r *sheetRenderer) function(name string, args []string) (string, bool) {
	switch name {
	case "concat":
		// CONCATENATE collapses an array to one value; "&" concatenates
		// elementwise, which is what a computed column needs.
		return "(" + strings.Join(args, "&") + ")", true
	case "round":
		if len(args) == 1 {
			return "ROUND(" + args[0] + ")", true
		}
		return "ROUND(" + strings.Join(args, ",") + ")", true
	case "mod":
		if len(args) == 2 {
			return "MOD(" + args[0] + "," + args[1] + ")", true
		}
	case "substr", "substring", "mid":
		if len(args) == 3 {
			return "MID(" + strings.Join(args, ",") + ")", true
		}
		if len(args) == 2 {
			return fmt.Sprintf("MID(%s,%s,LEN(%s))", args[0], args[1], args[0]), true
		}
	case "left":
		if len(args) == 2 {
			return "LEFT(" + args[0] + "," + args[1] + ")", true
		}
	case "right":
		if len(args) == 2 {
			return "RIGHT(" + args[0] + "," + args[1] + ")", true
		}
	case "replace":
		if len(args) == 3 {
			return "SUBSTITUTE(" + strings.Join(args, ",") + ")", true
		}
	case "coalesce", "ifnull":
		if len(args) >= 2 {
			// A blank cell is this driver's NULL, so fall through on "".
			out := args[len(args)-1]
			for i := len(args) - 2; i >= 0; i-- {
				out = fmt.Sprintf("IF(%s=\"\",%s,%s)", args[i], out, args[i])
			}
			return out, true
		}
	case "nullif":
		if len(args) == 2 {
			return fmt.Sprintf("IF(%s=%s,\"\",%s)", args[0], args[1], args[0]), true
		}
	case "if":
		if len(args) == 3 {
			return "IF(" + strings.Join(args, ",") + ")", true
		}
	}
	if fn, ok := sheetFuncs[name]; ok {
		return fn + "(" + strings.Join(args, ",") + ")", true
	}
	return "", false
}

// render turns a SQL expression into a spreadsheet array expression.
func (r *sheetRenderer) render(e sqlparser.Expr) (string, error) {
	switch n := e.(type) {
	case *sqlparser.ColName:
		pos, err := r.comp.position(n.Qualifier.Name.String(), n.Name.String())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("INDEX(%s,0,%d)", r.src, pos), nil

	case *sqlparser.ParenExpr:
		s, err := r.render(n.Expr)
		if err != nil {
			return "", err
		}
		return "(" + s + ")", nil

	case *sqlparser.BinaryExpr:
		l, err := r.render(n.Left)
		if err != nil {
			return "", err
		}
		rr, err := r.render(n.Right)
		if err != nil {
			return "", err
		}
		switch n.Operator {
		case "+", "-", "*", "/":
			return "(" + l + n.Operator + rr + ")", nil
		case "%":
			return "MOD(" + l + "," + rr + ")", nil
		}
		return "", unsupported("operator " + n.Operator)

	case *sqlparser.UnaryExpr:
		if n.Operator == "-" {
			s, err := r.render(n.Expr)
			if err != nil {
				return "", err
			}
			return "(-" + s + ")", nil
		}
		return "", unsupported("unary " + n.Operator)

	case *sqlparser.CaseExpr:
		return r.renderCase(n)

	case *sqlparser.SubstrExpr:
		// SUBSTR parses to its own node rather than a generic call.
		col, err := r.render(n.Name)
		if err != nil {
			return "", err
		}
		from, err := r.render(n.From)
		if err != nil {
			return "", err
		}
		if n.To == nil {
			return fmt.Sprintf("MID(%s,%s,LEN(%s))", col, from, col), nil
		}
		length, err := r.render(n.To)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("MID(%s,%s,%s)", col, from, length), nil

	case *sqlparser.FuncExpr:
		name := strings.ToLower(n.Name.String())
		args := make([]string, 0, len(n.Exprs))
		for _, se := range n.Exprs {
			ae, ok := se.(*sqlparser.AliasedExpr)
			if !ok {
				return "", unsupported("this argument to " + name + "()")
			}
			a, err := r.render(ae.Expr)
			if err != nil {
				return "", err
			}
			args = append(args, a)
		}
		if out, ok := r.function(name, args); ok {
			return out, nil
		}
		return "", unsupported("function " + name + "()")

	case *sqlparser.SQLVal, *sqlparser.NullVal, sqlparser.BoolVal:
		return r.literal(e)

	case *sqlparser.AndExpr:
		l, err := r.condition(n.Left)
		if err != nil {
			return "", err
		}
		rr, err := r.condition(n.Right)
		if err != nil {
			return "", err
		}
		// Multiplication ands elementwise; AND() would collapse the array.
		return "((" + l + ")*(" + rr + "))", nil

	case *sqlparser.OrExpr:
		l, err := r.condition(n.Left)
		if err != nil {
			return "", err
		}
		rr, err := r.condition(n.Right)
		if err != nil {
			return "", err
		}
		return "(SIGN((" + l + ")+(" + rr + ")))", nil

	case *sqlparser.NotExpr:
		s, err := r.condition(n.Expr)
		if err != nil {
			return "", err
		}
		return "(1-(" + s + "))", nil

	case *sqlparser.ComparisonExpr:
		return r.comparison(n)

	case *sqlparser.IsExpr:
		s, err := r.render(n.Expr)
		if err != nil {
			return "", err
		}
		switch strings.ToLower(n.Operator) {
		case "is null":
			return "(" + s + "=\"\")", nil
		case "is not null":
			return "(" + s + "<>\"\")", nil
		}
		return "", unsupported("IS " + n.Operator)
	}
	return "", unsupported(fmt.Sprintf("expression %s", sqlparser.String(e)))
}

// condition renders a boolean expression; results are 1/0 so they compose
// arithmetically over arrays.
func (r *sheetRenderer) condition(e sqlparser.Expr) (string, error) {
	s, err := r.render(e)
	if err != nil {
		return "", err
	}
	return s, nil
}

func (r *sheetRenderer) comparison(n *sqlparser.ComparisonExpr) (string, error) {
	l, err := r.render(n.Left)
	if err != nil {
		return "", err
	}
	op := strings.ToLower(n.Operator)

	if op == "in" || op == "not in" {
		tuple, ok := n.Right.(sqlparser.ValTuple)
		if !ok {
			return "", unsupported("IN with a non-literal list")
		}
		items := make([]string, 0, len(tuple))
		for _, v := range tuple {
			s, err := r.render(v)
			if err != nil {
				return "", err
			}
			items = append(items, s)
		}
		m := fmt.Sprintf("ISNUMBER(MATCH(%s,{%s},0))", l, strings.Join(items, ","))
		if op == "not in" {
			return "(1-N(" + m + "))", nil
		}
		return "N(" + m + ")", nil
	}

	rr, err := r.render(n.Right)
	if err != nil {
		return "", err
	}
	switch op {
	case "=":
		return "N(" + l + "=" + rr + ")", nil
	case "!=", "<>":
		return "N(" + l + "<>" + rr + ")", nil
	case "<", "<=", ">", ">=":
		return "N(" + l + op + rr + ")", nil
	case "like":
		// A LIKE pattern maps onto the wildcard syntax COUNTIF understands.
		return fmt.Sprintf("N(COUNTIF(%s,%s)>0)", l, likeToWildcard(rr)), nil
	}
	return "", unsupported("operator " + n.Operator)
}

// likeToWildcard converts % and _ to the * and ? COUNTIF expects.
func likeToWildcard(rendered string) string {
	return fmt.Sprintf("SUBSTITUTE(SUBSTITUTE(%s,\"%%\",\"*\"),\"_\",\"?\")", rendered)
}

func (r *sheetRenderer) renderCase(n *sqlparser.CaseExpr) (string, error) {
	var subject string
	if n.Expr != nil {
		s, err := r.render(n.Expr)
		if err != nil {
			return "", err
		}
		subject = s
	}

	parts := make([]string, 0, len(n.Whens)*2+2)
	for _, w := range n.Whens {
		var cond string
		if subject != "" {
			v, err := r.render(w.Cond)
			if err != nil {
				return "", err
			}
			cond = "(" + subject + "=" + v + ")"
		} else {
			c, err := r.condition(w.Cond)
			if err != nil {
				return "", err
			}
			cond = "(" + c + ")=1"
		}
		val, err := r.render(w.Val)
		if err != nil {
			return "", err
		}
		parts = append(parts, cond, val)
	}

	els := `""`
	if n.Else != nil {
		v, err := r.render(n.Else)
		if err != nil {
			return "", err
		}
		els = v
	}
	// A trailing TRUE branch stands in for ELSE and keeps IFS from erroring
	// when no condition matches.
	parts = append(parts, "TRUE", els)
	return "IFS(" + strings.Join(parts, ",") + ")", nil
}

func (r *sheetRenderer) literal(e sqlparser.Expr) (string, error) {
	switch n := e.(type) {
	case *sqlparser.NullVal:
		return `""`, nil
	case sqlparser.BoolVal:
		if bool(n) {
			return "TRUE", nil
		}
		return "FALSE", nil
	case *sqlparser.SQLVal:
		switch n.Type {
		case sqlparser.IntVal, sqlparser.FloatVal:
			return string(n.Val), nil
		case sqlparser.StrVal:
			return sheetString(string(n.Val)), nil
		case sqlparser.ValArg:
			t := &translator{src: r.comp, args: r.args}
			v, err := t.arg(n)
			if err != nil {
				return "", err
			}
			return sheetGoValue(v)
		}
	}
	return "", unsupported(fmt.Sprintf("literal %s", sqlparser.String(e)))
}

func sheetGoValue(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return `""`, nil
	case bool:
		return strings.ToUpper(strconv.FormatBool(x)), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case []byte:
		return sheetString(string(x)), nil
	case string:
		return sheetString(x), nil
	case time.Time:
		return sheetString(x.Format("2006-01-02 15:04:05")), nil
	}
	return "", fmt.Errorf("sheetsql: cannot bind %T", v)
}

// sheetString renders a formula string literal, doubling embedded quotes.
func sheetString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
