package sheetsql_test

import (
	"fmt"
	"sync"
	"testing"
)

// TestLiveConcurrentFormulaQueries runs several join queries at once. Formula
// evaluation is stateful -- the formula must occupy a real cell while Google
// computes it -- so concurrent statements must not share one scratch cell.
func TestLiveConcurrentFormulaQueries(t *testing.T) {
	db := liveDB(t)
	db.SetMaxOpenConns(8)

	// Each department has a known headcount; a clobbered scratch cell shows up
	// as one query reading another's result.
	want := map[string]int{"eng": 4, "research": 3, "networking": 1}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for round := 0; round < 3; round++ {
		for dept, n := range want {
			wg.Add(1)
			go func(dept string, n int) {
				defer wg.Done()
				var got int
				err := db.QueryRow(`SELECT count(*) FROM employees e
					JOIN depts d ON e.dept = d.dept WHERE d.dept = ?`, dept).Scan(&got)
				if err != nil {
					errs <- fmt.Errorf("%s: %w", dept, err)
					return
				}
				if got != n {
					errs <- fmt.Errorf("%s: got %d, want %d", dept, got, n)
				}
			}(dept, n)
		}
	}
	wg.Wait()
	close(errs)

	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		t.Errorf("%d of 9 concurrent queries returned the wrong result:", len(failures))
		for _, e := range failures {
			t.Errorf("  %v", e)
		}
	}
}
