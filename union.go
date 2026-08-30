package sheetsql

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"

	"github.com/xwb1989/sqlparser"
)

// UNION has no equivalent in the query language. Stacking two result arrays
// with the {a; b} literal does have one, so each branch is compiled separately
// into the same LET scope and the results are concatenated.

// projectionSource addresses an already-computed result by column position,
// which is how a UNION's ORDER BY has to reach its operands.
type projectionSource struct {
	cols []resultCol
}

func (p *projectionSource) column(qualifier, name string) (string, string, error) {
	for i, c := range p.cols {
		if strings.EqualFold(c.Name, name) {
			return colRef(i), c.Type, nil
		}
	}
	// Postgres and MySQL both allow ORDER BY <ordinal> after a UNION.
	if n, err := strconv.Atoi(name); err == nil && n >= 1 && n <= len(p.cols) {
		return colRef(n - 1), p.cols[n-1].Type, nil
	}
	names := make([]string, len(p.cols))
	for i, c := range p.cols {
		names[i] = c.Name
	}
	return "", "", fmt.Errorf("sheetsql: no column %q in the union result (have: %s)",
		name, strings.Join(names, ", "))
}

func (p *projectionSource) star(qualifier string) ([]projected, error) {
	out := make([]projected, len(p.cols))
	for i, c := range p.cols {
		out[i] = projected{Ident: colRef(i), Name: c.Name, Type: c.Type}
	}
	return out, nil
}

func (p *projectionSource) first() string    { return colRef(0) }
func (p *projectionSource) describe() string { return "union" }

// flattenUnion collects the branches of a chain of UNIONs, rejecting a mix of
// ALL and DISTINCT because they would need different combining strategies.
func flattenUnion(stmt sqlparser.SelectStatement, branches *[]*sqlparser.Select, distinct *bool, seen *bool) error {
	switch n := stmt.(type) {
	case *sqlparser.Select:
		*branches = append(*branches, n)
		return nil
	case *sqlparser.ParenSelect:
		return flattenUnion(n.Select, branches, distinct, seen)
	case *sqlparser.Union:
		d := !strings.EqualFold(n.Type, sqlparser.UnionAllStr)
		if *seen && d != *distinct {
			return unsupported("mixing UNION and UNION ALL in one statement")
		}
		*distinct, *seen = d, true
		if err := flattenUnion(n.Left, branches, distinct, seen); err != nil {
			return err
		}
		return flattenUnion(n.Right, branches, distinct, seen)
	}
	return unsupported("this UNION operand")
}

func (c *conn) queryUnion(ctx context.Context, u *sqlparser.Union, args []driver.NamedValue) (driver.Rows, error) {
	var branches []*sqlparser.Select
	var distinct, seen bool
	if err := flattenUnion(u, &branches, &distinct, &seen); err != nil {
		return nil, err
	}
	if len(branches) < 2 {
		return nil, unsupported("this UNION")
	}

	var binds []string
	var bodies []string
	var cols []resultCol

	for i, sel := range branches {
		comp, joins, err := c.buildComposition(ctx, sel)
		if err != nil {
			return nil, err
		}
		if err := comp.resolveJoinKeys(joins); err != nil {
			return nil, err
		}
		prefix := fmt.Sprintf("_u%d", i)
		b, body, bcols, _, err := compileSelectParts(comp, sel, args, prefix)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			cols = bcols
		} else if len(bcols) != len(cols) {
			return nil, fmt.Errorf("sheetsql: UNION branches project %d and %d columns; they must match",
				len(cols), len(bcols))
		}
		binds = append(binds, b...)
		bodies = append(bodies, body)
	}

	// ";" stacks arrays vertically.
	combined := "{" + strings.Join(bodies, ";") + "}"
	if distinct {
		combined = "UNIQUE(" + combined + ")"
	}

	if len(u.OrderBy) > 0 || u.Limit != nil {
		tr := &translator{src: &projectionSource{cols: cols}, args: args, multi: true}
		fake := &sqlparser.Select{OrderBy: u.OrderBy, Limit: u.Limit}
		tail, err := tr.tailClauses(fake, false)
		if err != nil {
			return nil, err
		}
		idents := make([]string, len(cols))
		for i := range cols {
			idents[i] = colRef(i)
		}
		q := "select " + strings.Join(idents, ", ") + tail + labelClause(idents)
		combined = fmt.Sprintf("QUERY(%s,%s,0)", combined, quoteFormulaString(q))
	}

	grid, err := c.evalFormula(ctx, assembleLet(binds, combined), len(cols))
	if err != nil {
		return nil, err
	}
	return newGridRows(grid, cols, false), nil
}
