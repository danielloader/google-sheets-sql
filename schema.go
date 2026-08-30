package sheetsql

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/api/sheets/v4"
)

// RIDColumn is the header of the optional row-identity column. When a sheet has
// one, UPDATE and DELETE address rows by identity instead of by position, so a
// concurrent edit that shifts rows cannot cause a write to land on the wrong
// one. It is hidden from "SELECT *" and from INSERT's default column list.
const RIDColumn = "_rid"

// column is one resolved spreadsheet column.
type column struct {
	Letter string // gviz column id: A, B, C...
	Label  string // header text
	Type   string // gviz type: string|number|boolean|date|datetime|timeofday
}

// schema maps header names to the column letters gviz actually addresses.
// The Visualization query language has no notion of column names, so every
// identifier in a user's SQL must be rewritten to a letter before it is sent.
type schema struct {
	Sheet   string
	Columns []column // visible columns, in sheet order, excluding _rid
	RID     *column  // row-identity column, if the sheet has one

	byName   map[string]column
	byLetter map[string]column
}

func (s *schema) lookup(name string) (column, bool) {
	if c, ok := s.byName[strings.ToLower(name)]; ok {
		return c, true
	}
	// Allow addressing by raw column letter for unlabelled columns.
	if c, ok := s.byLetter[strings.ToUpper(name)]; ok {
		return c, true
	}
	return column{}, false
}

func (s *schema) names() []string {
	out := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		out[i] = c.Label
	}
	return out
}

// width is the number of physical cells a full row occupies.
func (s *schema) width() int {
	w := 0
	for _, c := range s.Columns {
		if i := colIndex(c.Letter); i+1 > w {
			w = i + 1
		}
	}
	if s.RID != nil {
		if i := colIndex(s.RID.Letter); i+1 > w {
			w = i + 1
		}
	}
	return w
}

type schemaCache struct {
	mu sync.Mutex
	m  map[string]*schema
}

func newSchemaCache() *schemaCache { return &schemaCache{m: map[string]*schema{}} }

func (sc *schemaCache) get(ctx context.Context, c *conn, sheet string) (*schema, error) {
	sc.mu.Lock()
	if s, ok := sc.m[sheet]; ok {
		sc.mu.Unlock()
		return s, nil
	}
	sc.mu.Unlock()

	// Schema comes from the Sheets API, not gviz. The Visualization endpoint
	// parses the whole spreadsheet on every request, so once any tab is large
	// it takes tens of seconds to answer even about a tiny one.
	s, err := c.inferSchema(ctx, sheet)
	if err != nil {
		return nil, err
	}

	sc.mu.Lock()
	sc.m[sheet] = s
	sc.mu.Unlock()
	return s, nil
}

// inferSchema reads the header row and a sample of data rows, taking column
// types from each cell's number format where one is set.
func (c *conn) inferSchema(ctx context.Context, sheet string) (*schema, error) {
	svc, err := c.sheetsService(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.limiter.wait(ctx); err != nil {
		return nil, err
	}
	rng := fmt.Sprintf("%s!A%d:ZZ%d", normaliseSheet(sheet), c.cfg.HeaderRow, c.cfg.HeaderRow+sampleRows)
	ss, err := svc.Spreadsheets.Get(c.cfg.SpreadsheetID).
		Ranges(rng).IncludeGridData(true).
		Fields("sheets.data.rowData.values(effectiveValue,effectiveFormat.numberFormat.type)").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sheetsql: read schema for %q: %w", sheet, err)
	}
	if len(ss.Sheets) == 0 || len(ss.Sheets[0].Data) == 0 {
		return nil, fmt.Errorf("sheetsql: sheet %q returned no data", sheet)
	}
	rowData := ss.Sheets[0].Data[0].RowData
	if len(rowData) == 0 {
		return nil, fmt.Errorf("sheetsql: sheet %q is empty", sheet)
	}

	s := &schema{
		Sheet:    sheet,
		byName:   map[string]column{},
		byLetter: map[string]column{},
	}
	header := rowData[0].Values
	for i, hc := range header {
		label := ""
		if hc != nil && hc.EffectiveValue != nil {
			label = cellString(hc.EffectiveValue)
		}
		col := column{Letter: colLetter(i), Label: label, Type: inferColType(rowData[1:], i)}
		s.byLetter[col.Letter] = col
		if label == "" {
			continue
		}
		s.byName[strings.ToLower(label)] = col
		if strings.EqualFold(label, RIDColumn) {
			rid := col
			s.RID = &rid
			continue
		}
		s.Columns = append(s.Columns, col)
	}
	if len(s.Columns) == 0 {
		return nil, fmt.Errorf("sheetsql: sheet %q has no header row", sheet)
	}
	return s, nil
}

const sampleRows = 20

func cellString(v *sheets.ExtendedValue) string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.NumberValue != nil:
		return strconv.FormatFloat(*v.NumberValue, 'f', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	case v.FormulaValue != nil:
		return *v.FormulaValue
	}
	return ""
}

// inferColType picks the type of column i from the sampled rows. A number
// format wins over the raw value, because Sheets stores dates as numbers.
func inferColType(rows []*sheets.RowData, i int) string {
	typ := ""
	for _, r := range rows {
		if r == nil || i >= len(r.Values) || r.Values[i] == nil {
			continue
		}
		cell := r.Values[i]
		if cell.EffectiveFormat != nil && cell.EffectiveFormat.NumberFormat != nil {
			switch cell.EffectiveFormat.NumberFormat.Type {
			case "DATE":
				return "date"
			case "DATE_TIME":
				return "datetime"
			case "TIME":
				return "timeofday"
			}
		}
		v := cell.EffectiveValue
		if v == nil {
			continue
		}
		switch {
		case v.BoolValue != nil:
			if typ == "" {
				typ = "boolean"
			}
		case v.NumberValue != nil:
			if typ == "" {
				typ = "number"
			}
		case v.StringValue != nil:
			return "string"
		}
	}
	if typ == "" {
		return "string"
	}
	return typ
}

func (sc *schemaCache) invalidate(sheet string) {
	sc.mu.Lock()
	delete(sc.m, sheet)
	sc.mu.Unlock()
}
