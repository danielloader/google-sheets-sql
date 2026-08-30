// Command sheetsql runs a SQL query against a Google Spreadsheet.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	_ "github.com/danielloader/google-sheets-sql"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("SHEETSQL_DSN"), "sheets DSN")
	flag.Parse()
	if *dsn == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: sheetsql -dsn <dsn> '<SQL>' [args...]")
		os.Exit(2)
	}

	db, err := sql.Open("sheets", *dsn)
	check(err)
	defer db.Close()

	args := make([]any, 0, flag.NArg()-1)
	for _, a := range flag.Args()[1:] {
		args = append(args, a)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	query := flag.Arg(0)
	start := time.Now()
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(query)), "select") {
		res, err := db.ExecContext(ctx, query, args...)
		check(err)
		n, _ := res.RowsAffected()
		fmt.Printf("%d row(s) affected in %s\n", n, time.Since(start).Round(time.Millisecond))
		return
	}

	rows, err := db.QueryContext(ctx, query, args...)
	check(err)
	defer rows.Close()

	cols, err := rows.Columns()
	check(err)
	types, _ := rows.ColumnTypes()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	typeRow := make([]string, len(cols))
	for i := range cols {
		if i < len(types) {
			typeRow[i] = strings.ToLower(types[i].DatabaseTypeName())
		}
	}
	fmt.Fprintln(w, strings.Join(cols, "\t"))
	fmt.Fprintln(w, strings.Join(typeRow, "\t"))
	rule := make([]string, len(cols))
	for i, c := range cols {
		n := len(c)
		if len(typeRow[i]) > n {
			n = len(typeRow[i])
		}
		rule[i] = strings.Repeat("-", n)
	}
	fmt.Fprintln(w, strings.Join(rule, "\t"))

	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		check(rows.Scan(ptrs...))
		cells := make([]string, len(cols))
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				cells[i] = "NULL"
			case []byte:
				cells[i] = string(x)
			case time.Time:
				cells[i] = x.Format("2006-01-02")
			default:
				cells[i] = fmt.Sprint(x)
			}
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
		n++
	}
	check(rows.Err())
	w.Flush()
	fmt.Printf("\n%d row(s) in %s\n", n, time.Since(start).Round(time.Millisecond))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
