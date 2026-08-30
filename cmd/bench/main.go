// Command bench measures how the driver's cost scales with sheet size.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"time"

	_ "github.com/danielloader/google-sheets-sql"
)

const iters = 3

func main() {
	db, err := sql.Open("sheets", os.Getenv("SHEETSQL_DSN"))
	if err != nil {
		panic(err)
	}
	defer db.Close()

	fmt.Printf("%-52s %10s %10s %8s\n", "operation", "median", "min", "rows")
	fmt.Println(string(make([]byte, 84)))

	q := func(label, query string, args ...any) {
		var times []time.Duration
		n := 0
		for i := 0; i < iters; i++ {
			start := time.Now()
			rows, err := db.Query(query, args...)
			if err != nil {
				fmt.Printf("%-52s ERROR %v\n", label, err)
				return
			}
			n = 0
			for rows.Next() {
				n++
			}
			rows.Close()
			times = append(times, time.Since(start))
		}
		report(label, times, n)
	}

	fmt.Println("-- reads: 8-row tab --")
	q("point SELECT ... WHERE name = ?  (8 rows)",
		"SELECT id, name, dept FROM employees WHERE name = ?", "Grace Hopper")
	q("count(*)                          (8 rows)",
		"SELECT count(*) FROM employees")

	fmt.Println("\n-- reads: 10,000-row tab, all pushed to Google --")
	q("point SELECT ... WHERE name = ?  (10k rows)",
		"SELECT id, name, dept FROM scale WHERE name = ?", "Person 007500")
	q("count(*)                          (10k rows)",
		"SELECT count(*) FROM scale")
	q("count(*) WHERE dept = ?           (10k rows)",
		"SELECT count(*) FROM scale WHERE dept = ?", "eng")
	q("GROUP BY dept + avg(salary)       (10k rows)",
		"SELECT dept, count(*), avg(salary) FROM scale GROUP BY dept")
	q("ORDER BY salary DESC LIMIT 10     (10k rows)",
		"SELECT name, salary FROM scale ORDER BY salary DESC LIMIT 10")
	q("WHERE salary > ? AND active = true (10k rows)",
		"SELECT count(*) FROM scale WHERE salary > ? AND active = true", 150000)
	q("SELECT * no filter                (10k rows)",
		"SELECT id, name, dept FROM scale")

	fmt.Println("\n-- writes: 10,000-row tab, one row affected each --")
	db.Exec("DELETE FROM scale WHERE name = ?", "Zz Bench")

	var ins, upd, del []time.Duration
	for i := 0; i < iters; i++ {
		start := time.Now()
		if _, err := db.Exec("INSERT INTO scale (id, name, dept, region, salary, active)"+
			" VALUES (99999, ?, 'qa', 'emea', 1, true)", "Zz Bench"); err != nil {
			panic(err)
		}
		ins = append(ins, time.Since(start))

		start = time.Now()
		r, err := db.Exec("UPDATE scale SET salary = ? WHERE name = ?", 2+i, "Zz Bench")
		if err != nil {
			panic(err)
		}
		upd = append(upd, time.Since(start))
		if n, _ := r.RowsAffected(); n != 1 {
			fmt.Printf("  (warning: UPDATE affected %d rows)\n", n)
		}

		start = time.Now()
		r, err = db.Exec("DELETE FROM scale WHERE name = ?", "Zz Bench")
		if err != nil {
			panic(err)
		}
		del = append(del, time.Since(start))
		if n, _ := r.RowsAffected(); n != 1 {
			fmt.Printf("  (warning: DELETE affected %d rows)\n", n)
		}
	}
	report("INSERT one row  (append + max(_rid) lookup)", ins, 1)
	report("UPDATE one row  (scan + verify re-read + write)", upd, 1)
	report("DELETE one row  (scan + verify re-read + batchUpdate)", del, 1)

	fmt.Println("\n-- cost breakdown --")
	var raw []time.Duration
	for i := 0; i < iters; i++ {
		start := time.Now()
		rows, err := db.Query("SELECT id FROM scale")
		if err != nil {
			panic(err)
		}
		for rows.Next() {
		}
		rows.Close()
		raw = append(raw, time.Since(start))
	}
	report("one full read of the tab (what verify costs)", raw, 10000)
}

func report(label string, times []time.Duration, rows int) {
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	fmt.Printf("%-52s %10s %10s %8d\n", label,
		times[len(times)/2].Round(time.Millisecond),
		times[0].Round(time.Millisecond), rows)
}
