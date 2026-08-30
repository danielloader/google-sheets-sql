package sheetsql

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/api/sheets/v4"
)

// DefaultScratchSheet holds formulas while Google evaluates them.
const DefaultScratchSheet = "_sheetsql_scratch"

// formulaError is a spreadsheet error value returned in place of a result.
type formulaError struct {
	Code    string
	Formula string
}

func (e *formulaError) Error() string {
	return fmt.Sprintf("sheetsql: spreadsheet returned %s evaluating the compiled formula: %s", e.Code, e.Formula)
}

// spreadsheetErrors are the values Sheets writes into a cell when a formula
// cannot be evaluated. They arrive as ordinary strings.
var spreadsheetErrors = []string{
	"#ERROR!", "#VALUE!", "#REF!", "#N/A", "#NAME?", "#DIV/0!", "#NUM!", "#NULL!",
}

func asFormulaError(v any, formula string) error {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	// QUERY and FILTER both yield #N/A when nothing matches, which is an empty
	// result rather than a failure.
	if strings.HasPrefix(s, "#N/A") {
		return nil
	}
	for _, code := range spreadsheetErrors {
		if s == code || strings.HasPrefix(s, code) {
			return &formulaError{Code: code, Formula: formula}
		}
	}
	return nil
}

// scratchPad serialises use of the scratch cell. Formula evaluation is
// inherently stateful: the formula must occupy a real cell while Google
// computes it, so two statements cannot share one pad concurrently.
//
// Pads are shared per (spreadsheet, scratch sheet) rather than per connection.
// database/sql opens a connection per concurrent query, so a per-connection
// lock would let two joins write the same cell and each read the other's
// result -- silently, with no error. Formula evaluation is therefore
// single-writer within a process, as SQLite is for its database file.
type scratchPad struct {
	mu      sync.Mutex
	ensured bool
}

var (
	padMu  sync.Mutex
	padReg = map[string]*scratchPad{}
)

func sharedPad(spreadsheetID, sheet string) *scratchPad {
	key := spreadsheetID + "\x00" + sheet
	padMu.Lock()
	defer padMu.Unlock()
	if p, ok := padReg[key]; ok {
		return p
	}
	p := &scratchPad{}
	padReg[key] = p
	return p
}

func (c *conn) scratchSheet() string {
	if c.cfg.Scratch != "" {
		return c.cfg.Scratch
	}
	return DefaultScratchSheet
}

func (c *conn) ensureScratch(ctx context.Context) error {
	c.pad.mu.Lock()
	ensured := c.pad.ensured
	c.pad.mu.Unlock()
	if ensured {
		return nil
	}

	svc, err := c.sheetsService(ctx)
	if err != nil {
		return err
	}
	name := c.scratchSheet()
	if err := c.loadTabs(ctx); err != nil {
		return err
	}
	c.tabs.mu.Lock()
	_, exists := c.tabs.byName[strings.ToLower(name)]
	c.tabs.mu.Unlock()

	if !exists {
		if err := c.limiter.wait(ctx); err != nil {
			return err
		}
		_, err := svc.Spreadsheets.BatchUpdate(c.cfg.SpreadsheetID,
			&sheets.BatchUpdateSpreadsheetRequest{Requests: []*sheets.Request{{
				AddSheet: &sheets.AddSheetRequest{Properties: &sheets.SheetProperties{
					Title:          name,
					Hidden:         true,
					GridProperties: &sheets.GridProperties{RowCount: 1000, ColumnCount: 26},
				}},
			}}}).Context(ctx).Do()
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("sheetsql: create scratch sheet: %w", err)
		}
		if err := c.loadTabs(ctx); err != nil {
			return err
		}
	}

	c.pad.mu.Lock()
	c.pad.ensured = true
	c.pad.mu.Unlock()
	return nil
}

// evalFormula writes a formula into the scratch sheet, reads back the range it
// spilled into, and clears it. The returned grid is the formula's result.
func (c *conn) evalFormula(ctx context.Context, formula string, ncols int) ([][]any, error) {
	if c.cfg.ReadOnly {
		return nil, fmt.Errorf("sheetsql: joins, HAVING, UNION and CASE are evaluated by writing a formula to the %q sheet, which a read-only connection cannot do; single-table queries still work",
			c.scratchSheet())
	}
	if err := c.ensureScratch(ctx); err != nil {
		return nil, err
	}
	svc, err := c.sheetsService(ctx)
	if err != nil {
		return nil, err
	}
	name := normaliseSheet(c.scratchSheet())

	c.pad.mu.Lock()
	defer c.pad.mu.Unlock()

	if debug {
		fmt.Fprintf(stderr, "[sheetsql] formula: %s\n", formula)
	}

	if err := c.limiter.wait(ctx); err != nil {
		return nil, err
	}
	if _, err := svc.Spreadsheets.Values.Update(c.cfg.SpreadsheetID, name+"!A1",
		&sheets.ValueRange{Values: [][]any{{formula}}}).
		ValueInputOption("USER_ENTERED").Context(ctx).Do(); err != nil {
		return nil, fmt.Errorf("sheetsql: write formula: %w", err)
	}

	// Always clear, even if the read fails: a stale formula left in the pad
	// would recalculate on every future edit to the spreadsheet.
	defer func() {
		if err := c.limiter.wait(context.WithoutCancel(ctx)); err == nil {
			svc.Spreadsheets.Values.Clear(c.cfg.SpreadsheetID, name,
				&sheets.ClearValuesRequest{}).Context(context.WithoutCancel(ctx)).Do()
		}
	}()

	if err := c.limiter.wait(ctx); err != nil {
		return nil, err
	}
	// values.get trims to the populated rectangle, so asking for a large
	// window costs nothing when the result is small.
	// Bound the read to the columns the query actually projects; a wide open
	// window makes the API materialise far more of the grid than is needed.
	last := colLetter(max(0, ncols-1))
	if ncols <= 0 {
		last = "ZZ"
	}
	vr, err := svc.Spreadsheets.Values.Get(c.cfg.SpreadsheetID,
		fmt.Sprintf("%s!A1:%s%d", name, last, c.cfg.MaxRows)).
		ValueRenderOption("UNFORMATTED_VALUE").
		DateTimeRenderOption("SERIAL_NUMBER").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sheetsql: read formula result: %w", err)
	}
	if len(vr.Values) == 0 {
		return nil, nil
	}
	if len(vr.Values[0]) > 0 {
		if ferr := asFormulaError(vr.Values[0][0], formula); ferr != nil {
			return nil, ferr
		}
		if s, ok := vr.Values[0][0].(string); ok && strings.HasPrefix(s, "#N/A") {
			return nil, nil
		}
	}
	return vr.Values, nil
}
