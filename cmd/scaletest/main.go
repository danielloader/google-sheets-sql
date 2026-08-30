// Command scaletest finds the size at which a spreadsheet stops behaving as a
// usable database. For each row count it builds three joinable tabs, runs a
// correctness-checked three-way join, records the timings and deletes the tabs.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	_ "github.com/danielloader/google-sheets-sql"
)

var id string

const threshold = 8000

func rowsA(n int) [][]any {
	out := make([][]any, 0, n+1)
	out = append(out, []any{"id", "b_key", "c_key", "amount"})
	for i := 1; i <= n; i++ {
		out = append(out, []any{i, (i*7919)%n + 1, (i*104729)%n + 1, 100 + (i*31)%9000})
	}
	return out
}

func rowsLookup(kind string, n int) [][]any {
	out := make([][]any, 0, n+1)
	out = append(out, []any{"id", "label"})
	for i := 1; i <= n; i++ {
		out = append(out, []any{i, fmt.Sprintf("%s-%06d", kind, i)})
	}
	return out
}

func expected(n int) int {
	c := 0
	for i := 1; i <= n; i++ {
		if 100+(i*31)%9000 > threshold {
			c++
		}
	}
	return c
}

func main() {
	flag.StringVar(&id, "id", os.Getenv("SHEETSQL_ID"), "spreadsheet id")
	sizes := flag.String("sizes", "1000,5000,10000,25000", "row counts to test")
	flag.Parse()

	ctx := context.Background()
	b, _ := os.ReadFile(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
	cfg, _ := google.JWTConfigFromJSON(b, "https://www.googleapis.com/auth/spreadsheets")
	hc := cfg.Client(ctx)
	hc.Timeout = 240 * time.Second
	svc, err := sheets.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		panic(err)
	}

	fmt.Printf("%8s %10s %10s %12s %12s %10s\n", "rows", "load", "1-table", "3-join", "rows-ok", "cells")
	fmt.Println(strings.Repeat("-", 70))

	for _, s := range strings.Split(*sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			continue
		}
		runSize(svc, n)
	}
}

func runSize(svc *sheets.Service, n int) {
	defer dropTabs(svc)

	data := map[string][][]any{
		"big_a": rowsA(n),
		"big_b": rowsLookup("b", n),
		"big_c": rowsLookup("c", n),
	}

	start := time.Now()
	if err := loadTabs(svc, data); err != nil {
		fmt.Printf("%8d  load failed: %v\n", n, err)
		return
	}
	load := time.Since(start)

	db, err := sql.Open("sheets", os.Getenv("SHEETSQL_DSN"))
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	defer db.Close()

	start = time.Now()
	var one int
	err = db.QueryRow("SELECT count(*) FROM big_a WHERE amount > ?", threshold).Scan(&one)
	single := time.Since(start)
	if err != nil {
		single = 0
		one = -1
	}

	start = time.Now()
	rows, err := db.Query(`SELECT count(*) FROM big_a a
		JOIN big_b b ON a.b_key = b.id
		JOIN big_c c ON a.c_key = c.id
		WHERE a.amount > ?`, threshold)
	join := time.Since(start)
	got := -1
	if err == nil {
		if rows.Next() {
			rows.Scan(&got)
		}
		rows.Close()
	} else {
		msg := err.Error()
		if len(msg) > 240 {
			msg = msg[:240]
		}
		fmt.Printf("   join error at n=%d: %s\n", n, msg)
	}

	want := expected(n)
	status := fmt.Sprintf("%d/%d", got, want)
	if got == want {
		status = "OK " + status
	} else {
		status = "BAD " + status
	}

	cells := cellCount(svc)
	fmt.Printf("%8d %10v %10v %12v %12s %10d\n", n,
		load.Round(time.Millisecond), single.Round(time.Millisecond),
		join.Round(time.Millisecond), status, cells)
	_ = one
}

func loadTabs(svc *sheets.Service, data map[string][][]any) error {
	ss, err := svc.Spreadsheets.Get(id).Fields("sheets.properties(title,sheetId)").Do()
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, sh := range ss.Sheets {
		have[sh.Properties.Title] = true
	}
	for name, vals := range data {
		if have[name] {
			continue
		}
		// Size the grid exactly: a default 26-column tab wastes millions of
		// cells and slows every request to the whole spreadsheet.
		_, err := svc.Spreadsheets.BatchUpdate(id, &sheets.BatchUpdateSpreadsheetRequest{
			Requests: []*sheets.Request{{AddSheet: &sheets.AddSheetRequest{
				Properties: &sheets.SheetProperties{
					Title: name,
					GridProperties: &sheets.GridProperties{
						RowCount:    int64(len(vals) + 1),
						ColumnCount: int64(len(vals[0])),
					},
				}}}},
		}).Do()
		if err != nil {
			return err
		}
	}
	for name, vals := range data {
		const chunk = 10000
		for off := 0; off < len(vals); off += chunk {
			end := off + chunk
			if end > len(vals) {
				end = len(vals)
			}
			if _, err := svc.Spreadsheets.Values.Update(id,
				fmt.Sprintf("%s!A%d", name, off+1),
				&sheets.ValueRange{Values: vals[off:end]}).
				ValueInputOption("RAW").Do(); err != nil {
				return err
			}
		}
	}
	return nil
}

func dropTabs(svc *sheets.Service) {
	ss, err := svc.Spreadsheets.Get(id).Fields("sheets.properties(title,sheetId)").Do()
	if err != nil {
		return
	}
	for _, sh := range ss.Sheets {
		if !strings.HasPrefix(sh.Properties.Title, "big_") {
			continue
		}
		svc.Spreadsheets.BatchUpdate(id, &sheets.BatchUpdateSpreadsheetRequest{
			Requests: []*sheets.Request{{DeleteSheet: &sheets.DeleteSheetRequest{
				SheetId: sh.Properties.SheetId}}},
		}).Do()
	}
}

func cellCount(svc *sheets.Service) int64 {
	ss, err := svc.Spreadsheets.Get(id).Fields("sheets.properties.gridProperties").Do()
	if err != nil {
		return -1
	}
	var total int64
	for _, sh := range ss.Sheets {
		if sh.Properties.GridProperties != nil {
			total += sh.Properties.GridProperties.RowCount * sh.Properties.GridProperties.ColumnCount
		}
	}
	return total
}
