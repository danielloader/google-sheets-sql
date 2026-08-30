package sheetsql

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sort"
	"strings"

	"github.com/xwb1989/sqlparser"
	"google.golang.org/api/sheets/v4"
)

// afterScan is a test seam: it runs between the scan that selects rows and the
// verification that re-locates them, which is exactly the window a concurrent
// edit has to race in.
var afterScan func()

// matchedRow is a row that satisfied a WHERE clause, with its position in the
// sheet and, when the sheet has one, its stable identity.
type matchedRow struct {
	sheetRow int
	rid      string
	values   []any
}

// readRows returns every data row of a tab with its 1-based sheet position.
func (c *conn) readRows(ctx context.Context, sheet string) ([]matchedRow, error) {
	svc, err := c.sheetsService(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.limiter.wait(ctx); err != nil {
		return nil, err
	}
	vr, err := svc.Spreadsheets.Values.Get(c.cfg.SpreadsheetID, normaliseSheet(sheet)).
		ValueRenderOption("UNFORMATTED_VALUE").
		DateTimeRenderOption("SERIAL_NUMBER").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sheetsql: read sheet: %w", err)
	}
	out := make([]matchedRow, 0, len(vr.Values))
	for i, row := range vr.Values {
		if i < c.cfg.HeaderRow {
			continue
		}
		out = append(out, matchedRow{sheetRow: i + 1, values: row})
	}
	return out, nil
}

func ridOf(row []any, s *schema) string {
	if s.RID == nil {
		return ""
	}
	i := colIndex(s.RID.Letter)
	if i < 0 || i >= len(row) {
		return ""
	}
	return ridKey(row[i])
}

// scanFor returns the rows matching where. This is a full read: the predicate
// cannot be pushed to gviz because gviz does not report row numbers, and
// UPDATE/DELETE need them to address cells.
func (c *conn) scanFor(ctx context.Context, sheet string, s *schema, where *sqlparser.Where, args []driver.NamedValue) ([]matchedRow, error) {
	all, err := c.readRows(ctx, sheet)
	if err != nil {
		return nil, err
	}
	var expr sqlparser.Expr
	if where != nil {
		expr = where.Expr
	}
	ev := &evaluator{s: s, args: args}
	var out []matchedRow
	for _, r := range all {
		ev.row = r.values
		ok, err := ev.match(expr)
		if err != nil {
			return nil, err
		}
		if ok {
			r.rid = ridOf(r.values, s)
			out = append(out, r)
		}
	}
	return out, nil
}

// verify re-checks, immediately before writing, that the rows selected by the
// scan are still the rows that will be written. It returns them with their
// current positions, or a ConflictError if any moved out from under the
// statement.
//
// The re-read is unconditional. A Drive revision pre-check would be cheaper,
// but Drive reports a spreadsheet's version and modifiedTime lazily: both were
// observed unchanged for more than 11 seconds after a committed Sheets write,
// so "revision unchanged" does not mean "no rows moved". Trusting it would
// reintroduce exactly the wrong-row write this function exists to prevent.
//
// Without a _rid column there is nothing to re-locate by, so the matches are
// returned unchanged and the write remains position-based.
func (c *conn) verify(ctx context.Context, sheet string, s *schema, matches []matchedRow) ([]matchedRow, error) {
	if s.RID == nil || len(matches) == 0 {
		return matches, nil
	}
	fresh, err := c.readRows(ctx, sheet)
	if err != nil {
		return nil, err
	}

	index := make(map[string]matchedRow, len(fresh))
	dupes := map[string]bool{}
	for _, r := range fresh {
		rid := ridOf(r.values, s)
		if rid == "" {
			continue
		}
		if _, seen := index[rid]; seen {
			dupes[rid] = true
		}
		index[rid] = r
	}

	out := make([]matchedRow, 0, len(matches))
	for _, m := range matches {
		if m.rid == "" {
			return nil, &ConflictError{Sheet: sheet, RID: "(blank)",
				Reason: "matched row has no " + RIDColumn + " value"}
		}
		if dupes[m.rid] {
			return nil, &ConflictError{Sheet: sheet, RID: m.rid,
				Reason: "duplicate " + RIDColumn + " values in the sheet"}
		}
		cur, ok := index[m.rid]
		if !ok {
			return nil, &ConflictError{Sheet: sheet, RID: m.rid,
				Reason: "row was deleted concurrently"}
		}
		if !rowsEqual(m.values, cur.values) {
			return nil, &ConflictError{Sheet: sheet, RID: m.rid,
				Reason: "row was modified concurrently"}
		}
		m.sheetRow = cur.sheetRow // may have shifted since the scan
		out = append(out, m)
	}
	return out, nil
}

func targetTable(exprs sqlparser.TableExprs, what string) (string, error) {
	if len(exprs) != 1 {
		return "", unsupported("multi-table " + what)
	}
	ate, ok := exprs[0].(*sqlparser.AliasedTableExpr)
	if !ok {
		return "", unsupported("this " + what + " target")
	}
	tn, ok := ate.Expr.(sqlparser.TableName)
	if !ok {
		return "", unsupported("this " + what + " target")
	}
	return tn.Name.String(), nil
}

func (c *conn) execUpdate(ctx context.Context, up *sqlparser.Update, args []driver.NamedValue) (driver.Result, error) {
	name, err := targetTable(up.TableExprs, "UPDATE")
	if err != nil {
		return nil, err
	}
	sheet, err := c.resolveSheet(ctx, name)
	if err != nil {
		return nil, err
	}
	s, err := c.schemas.get(ctx, c, sheet)
	if err != nil {
		return nil, err
	}

	type assignment struct {
		col column
		val any
	}
	tr := &translator{src: s, args: args}
	sets := make([]assignment, 0, len(up.Exprs))
	for _, e := range up.Exprs {
		colName := e.Name.Name.String()
		if strings.EqualFold(colName, RIDColumn) {
			return nil, fmt.Errorf("sheetsql: %s is managed by the driver and cannot be assigned", RIDColumn)
		}
		col, ok := s.lookup(colName)
		if !ok {
			return nil, fmt.Errorf("sheetsql: no column %q in sheet %q (have: %s)",
				colName, sheet, strings.Join(s.names(), ", "))
		}
		v, err := tr.literalGoValue(e.Expr)
		if err != nil {
			return nil, err
		}
		sets = append(sets, assignment{col: col, val: v})
	}

	matches, err := c.scanFor(ctx, sheet, s, up.Where, args)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return result{}, nil
	}
	if afterScan != nil {
		afterScan()
	}
	matches, err = c.verify(ctx, sheet, s, matches)
	if err != nil {
		return nil, err
	}

	data := make([]*sheets.ValueRange, 0, len(matches)*len(sets))
	for _, m := range matches {
		for _, a := range sets {
			data = append(data, &sheets.ValueRange{
				Range:  fmt.Sprintf("%s!%s%d", normaliseSheet(sheet), a.col.Letter, m.sheetRow),
				Values: [][]any{{a.val}},
			})
		}
	}

	svc, err := c.sheetsService(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.limiter.wait(ctx); err != nil {
		return nil, err
	}
	_, err = svc.Spreadsheets.Values.BatchUpdate(c.cfg.SpreadsheetID,
		&sheets.BatchUpdateValuesRequest{
			ValueInputOption: "USER_ENTERED",
			Data:             data,
		}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sheetsql: update: %w", err)
	}
	return result{affected: int64(len(matches))}, nil
}

func (c *conn) execDelete(ctx context.Context, del *sqlparser.Delete, args []driver.NamedValue) (driver.Result, error) {
	name, err := targetTable(del.TableExprs, "DELETE")
	if err != nil {
		return nil, err
	}
	sheet, err := c.resolveSheet(ctx, name)
	if err != nil {
		return nil, err
	}
	s, err := c.schemas.get(ctx, c, sheet)
	if err != nil {
		return nil, err
	}
	gid, err := c.sheetID(ctx, sheet)
	if err != nil {
		return nil, err
	}

	matches, err := c.scanFor(ctx, sheet, s, del.Where, args)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return result{}, nil
	}
	if afterScan != nil {
		afterScan()
	}
	matches, err = c.verify(ctx, sheet, s, matches)
	if err != nil {
		return nil, err
	}

	// Delete bottom-up: each DeleteDimension shifts every later row up, which
	// would invalidate the remaining indices if applied in ascending order.
	sort.Slice(matches, func(i, j int) bool { return matches[i].sheetRow > matches[j].sheetRow })

	reqs := make([]*sheets.Request, 0, len(matches))
	for _, m := range matches {
		reqs = append(reqs, &sheets.Request{
			DeleteDimension: &sheets.DeleteDimensionRequest{
				Range: &sheets.DimensionRange{
					SheetId:    gid,
					Dimension:  "ROWS",
					StartIndex: int64(m.sheetRow - 1),
					EndIndex:   int64(m.sheetRow),
				},
			},
		})
	}

	svc, err := c.sheetsService(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.limiter.wait(ctx); err != nil {
		return nil, err
	}
	// spreadsheets.batchUpdate applies every request atomically.
	_, err = svc.Spreadsheets.BatchUpdate(c.cfg.SpreadsheetID,
		&sheets.BatchUpdateSpreadsheetRequest{Requests: reqs}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sheetsql: delete: %w", err)
	}
	return result{affected: int64(len(matches))}, nil
}
