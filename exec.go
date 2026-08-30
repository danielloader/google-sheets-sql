package sheetsql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"github.com/xwb1989/sqlparser"
	"google.golang.org/api/sheets/v4"
)

type result struct {
	affected int64
}

func (r result) LastInsertId() (int64, error) {
	return 0, errors.New("sheetsql: LastInsertId is not meaningful for a spreadsheet (no primary key)")
}
func (r result) RowsAffected() (int64, error) { return r.affected, nil }

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.cfg.ReadOnly {
		return nil, errReadOnly
	}
	stmt, err := sqlparser.Parse(query)
	if err != nil {
		return nil, fmt.Errorf("sheetsql: parse: %w", err)
	}
	switch s := stmt.(type) {
	case *sqlparser.Insert:
		return c.execInsert(ctx, s, args)
	case *sqlparser.Update:
		return c.execUpdate(ctx, s, args)
	case *sqlparser.Delete:
		return c.execDelete(ctx, s, args)
	case *sqlparser.Select:
		return nil, fmt.Errorf("sheetsql: use Query for SELECT")
	}
	return nil, fmt.Errorf("sheetsql: unsupported statement %T", stmt)
}

func (c *conn) execInsert(ctx context.Context, ins *sqlparser.Insert, args []driver.NamedValue) (driver.Result, error) {
	sheet, err := c.resolveSheet(ctx, ins.Table.Name.String())
	if err != nil {
		return nil, err
	}
	s, err := c.schemas.get(ctx, c, sheet)
	if err != nil {
		return nil, err
	}

	// Map the statement's column list onto the sheet's physical columns.
	targets := make([]column, 0, len(ins.Columns))
	if len(ins.Columns) == 0 {
		targets = append(targets, s.Columns...)
	} else {
		for _, ci := range ins.Columns {
			name := ci.String()
			if strings.EqualFold(name, RIDColumn) {
				return nil, fmt.Errorf("sheetsql: %s is managed by the driver and cannot be inserted", RIDColumn)
			}
			col, ok := s.lookup(name)
			if !ok {
				return nil, fmt.Errorf("sheetsql: no column %q in sheet %q (have: %s)",
					name, sheet, strings.Join(s.names(), ", "))
			}
			targets = append(targets, col)
		}
	}

	vals, ok := ins.Rows.(sqlparser.Values)
	if !ok {
		return nil, unsupported("INSERT ... SELECT")
	}

	nextRID := int64(0)
	if s.RID != nil {
		nextRID, err = c.maxRID(ctx, sheet, s)
		if err != nil {
			return nil, err
		}
	}

	tr := &translator{src: s, args: args}
	width := s.width()
	out := make([][]any, 0, len(vals))
	for _, tuple := range vals {
		if len(tuple) != len(targets) {
			return nil, fmt.Errorf("sheetsql: %d values for %d columns", len(tuple), len(targets))
		}
		// Indexed by physical position: columns need not be contiguous, and a
		// _rid or unlabelled column may sit between two named ones.
		row := make([]any, width)
		for i, e := range tuple {
			v, err := tr.literalGoValue(e)
			if err != nil {
				return nil, err
			}
			idx := colIndex(targets[i].Letter)
			if idx < 0 || idx >= width {
				return nil, fmt.Errorf("sheetsql: column %q is out of range", targets[i].Label)
			}
			row[idx] = v
		}
		if s.RID != nil {
			nextRID++
			row[colIndex(s.RID.Letter)] = nextRID
		}
		out = append(out, row)
	}

	svc, err := c.sheetsService(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.limiter.wait(ctx); err != nil {
		return nil, err
	}
	_, err = svc.Spreadsheets.Values.
		Append(c.cfg.SpreadsheetID, normaliseSheet(sheet), &sheets.ValueRange{Values: out}).
		ValueInputOption("USER_ENTERED").
		InsertDataOption("INSERT_ROWS").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sheetsql: append: %w", err)
	}
	c.schemas.invalidate(sheet)
	return result{affected: int64(len(out))}, nil
}

// maxRID asks Google for the largest existing row id. Computing it server-side
// keeps INSERT to one small query rather than a full read of the tab.
func (c *conn) maxRID(ctx context.Context, sheet string, s *schema) (int64, error) {
	tbl, err := c.gviz(ctx, sheet, "select max("+s.RID.Letter+")")
	if err != nil {
		return 0, err
	}
	if len(tbl.Rows) == 0 || len(tbl.Rows[0].C) == 0 || tbl.Rows[0].C[0] == nil {
		return 0, nil
	}
	f, ok := tbl.Rows[0].C[0].V.(float64)
	if !ok {
		return 0, nil
	}
	return int64(f), nil
}

// literalGoValue resolves an INSERT value expression to a plain Go value for
// the values API, which does its own type coercion via USER_ENTERED.
func (t *translator) literalGoValue(e sqlparser.Expr) (any, error) {
	switch n := e.(type) {
	case *sqlparser.NullVal:
		return nil, nil
	case sqlparser.BoolVal:
		return bool(n), nil
	case *sqlparser.UnaryExpr:
		if n.Operator == "-" {
			v, err := t.literalGoValue(n.Expr)
			if err != nil {
				return nil, err
			}
			switch x := v.(type) {
			case int64:
				return -x, nil
			case float64:
				return -x, nil
			}
			return nil, fmt.Errorf("sheetsql: cannot negate %T", v)
		}
	case *sqlparser.SQLVal:
		switch n.Type {
		case sqlparser.IntVal:
			return parseInt(string(n.Val))
		case sqlparser.FloatVal:
			return parseFloat(string(n.Val))
		case sqlparser.StrVal:
			return string(n.Val), nil
		case sqlparser.ValArg:
			return t.arg(n)
		}
	}
	return nil, unsupported(fmt.Sprintf("INSERT value %s", sqlparser.String(e)))
}

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx is rejected rather than faked: a spreadsheet offers no isolation, and
// silently ignoring a transaction would be worse than refusing one.
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return nil, errors.New("sheetsql: transactions are not supported")
}

type sheetTx struct{}
