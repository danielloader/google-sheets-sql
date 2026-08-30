package sheetsql

import (
	"database/sql/driver"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type rows struct {
	cols  []string
	types []string
	tbl   *gvizTable
	pos   int
}

func newRows(tbl *gvizTable, cols []resultCol, bareAggregate bool) *rows {
	// SQL guarantees one row for an aggregate with no GROUP BY, even when the
	// WHERE clause excludes everything; gviz returns an empty result instead.
	if bareAggregate && len(tbl.Rows) == 0 && len(tbl.Cols) > 0 {
		row := gvizRow{C: make([]*gvizCell, len(tbl.Cols))}
		for i := range row.C {
			var v any
			if i < len(cols) && cols[i].Agg == "count" {
				v = float64(0)
			}
			row.C[i] = &gvizCell{V: v}
		}
		tbl.Rows = []gvizRow{row}
	}
	r := &rows{tbl: tbl}
	for i, gc := range tbl.Cols {
		name := gc.Label
		if i < len(cols) && cols[i].Name != "" {
			name = cols[i].Name
		}
		if name == "" {
			name = gc.ID
		}
		r.cols = append(r.cols, name)
		r.types = append(r.types, gc.Type)
	}
	return r
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error      { r.tbl = nil; return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.pos >= len(r.tbl.Rows) {
		return io.EOF
	}
	row := r.tbl.Rows[r.pos]
	r.pos++
	for i := range dest {
		if i >= len(row.C) || row.C[i] == nil {
			dest[i] = nil
			continue
		}
		v, err := convertCell(row.C[i].V, r.types[i])
		if err != nil {
			return fmt.Errorf("sheetsql: column %q: %w", r.cols[i], err)
		}
		dest[i] = v
	}
	return nil
}

// dateCtor matches the "Date(2019,2,1)" / "Date(2019,2,1,10,30,0)" form gviz
// uses for date and datetime cells. The month field is 0-based, as in JS.
var dateCtor = regexp.MustCompile(`^Date\((\d+),(\d+),(\d+)(?:,(\d+),(\d+),(\d+))?(?:,(\d+))?\)$`)

func convertCell(v any, typ string) (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	switch typ {
	case "number":
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("expected number, got %T", v)
		}
		// Sheets has one numeric type; surface whole numbers as int64 so they
		// scan into integer destinations without a float round-trip.
		if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return int64(f), nil
		}
		return f, nil
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("expected boolean, got %T", v)
		}
		return b, nil
	case "date", "datetime":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected date string, got %T", v)
		}
		return parseGvizDate(s)
	case "timeofday":
		parts, ok := v.([]any)
		if !ok {
			return fmt.Sprint(v), nil
		}
		n := make([]int, 4)
		for i := 0; i < len(parts) && i < 4; i++ {
			if f, ok := parts[i].(float64); ok {
				n[i] = int(f)
			}
		}
		return fmt.Sprintf("%02d:%02d:%02d", n[0], n[1], n[2]), nil
	default:
		if s, ok := v.(string); ok {
			return s, nil
		}
		return fmt.Sprint(v), nil
	}
}

func parseGvizDate(s string) (time.Time, error) {
	m := dateCtor.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("unrecognised date value %q", s)
	}
	at := func(i int) int {
		if m[i] == "" {
			return 0
		}
		n, _ := strconv.Atoi(m[i])
		return n
	}
	return time.Date(at(1), time.Month(at(2)+1), at(3), at(4), at(5), at(6), at(7)*int(time.Millisecond), time.UTC), nil
}

func (r *rows) ColumnTypeDatabaseTypeName(i int) string {
	return strings.ToUpper(r.types[i])
}

func (r *rows) ColumnTypeScanType(i int) reflect.Type {
	switch r.types[i] {
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

// ColumnTypeNullable reports true for every column: a spreadsheet cell can
// always be blank, whatever the inferred type of its neighbours.
func (r *rows) ColumnTypeNullable(i int) (nullable, ok bool) { return true, true }
