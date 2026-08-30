package sheetsql

import (
	"fmt"
	"strconv"
)

// ConflictError reports that a row changed between the read that selected it
// and the write that would have modified it.
//
// Sheets offers no compare-and-set primitive, so UPDATE and DELETE re-read
// their target rows immediately before writing and compare them against the
// values they matched on. The race window is narrowed, not closed: a write that
// lands inside it is still possible.
type ConflictError struct {
	Sheet  string
	RID    string
	Reason string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("sheetsql: conflict on %s row %s=%s: %s; the statement was not applied",
		e.Sheet, RIDColumn, e.RID, e.Reason)
}

// ridKey renders a cell value as a stable identity string. Numbers arrive from
// the values API as float64, so they are formatted without exponent to keep the
// key identical across reads.
func ridKey(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

func cellKey(v any) string {
	if v == nil {
		return ""
	}
	return ridKey(v)
}

// rowsEqual compares two snapshots of the same row, ignoring trailing blanks:
// the values API omits empty cells at the end of a row, and how many it omits
// depends on what else is in the sheet.
func rowsEqual(a, b []any) bool {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var x, y any
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if cellKey(x) != cellKey(y) {
			return false
		}
	}
	return true
}
