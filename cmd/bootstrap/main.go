// Command bootstrap populates an existing spreadsheet with test fixtures.
// The spreadsheet must already be shared with the service account as Editor;
// service accounts have no Drive storage quota and cannot create files.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// _rid is deliberately the last column: it keeps the identity column out of the
// way in the UI and exercises the driver's handling of non-contiguous columns.
var fixtures = map[string][][]any{
	"employees": {
		{"id", "name", "dept", "salary", "hired", "active", "_rid"},
		{1, "Ada Lovelace", "eng", 165000, "2019-03-01", true, 1},
		{2, "Grace Hopper", "eng", 172000, "2017-07-15", true, 2},
		{3, "Katherine Johnson", "research", 158000, "2018-01-09", true, 3},
		{4, "Alan Turing", "eng", 181000, "2016-06-23", false, 4},
		{5, "Margaret Hamilton", "research", 174000, "2020-11-02", true, 5},
		{6, "Barbara Liskov", "eng", 190000, "2015-02-14", true, 6},
		{7, "Radia Perlman", "networking", 168000, "2021-08-30", true, 7},
		{8, "Jean Bartik", "research", 142000, "2022-04-18", false, 8},
	},
	"depts": {
		{"dept", "label", "budget"},
		{"eng", "Engineering", 5000000},
		{"research", "Research", 3000000},
		{"networking", "Networking", 1500000},
		{"qa", "Quality", 800000},
		{"sre", "Reliability", 1200000},
		{"design", "Design", 900000},
	},
	"regions": {
		{"region", "continent", "tz"},
		{"emea", "Europe", "UTC"},
		{"amer", "Americas", "UTC-5"},
		{"apac", "Asia", "UTC+8"},
		{"latam", "Americas", "UTC-3"},
	},
	// targets joins to sales on two columns at once, exercising composite keys.
	"targets": {
		{"region", "quarter", "target"},
		{"emea", "Q1", 100000},
		{"emea", "Q2", 110000},
		{"amer", "Q1", 200000},
		{"amer", "Q2", 190000},
		{"apac", "Q1", 80000},
		{"apac", "Q2", 85000},
	},
	"sales": {
		{"region", "quarter", "amount"},
		{"emea", "Q1", 120500.5},
		{"emea", "Q2", 98000},
		{"amer", "Q1", 210300.25},
		{"amer", "Q2", 187900},
		{"apac", "Q1", 76400},
		{"apac", "Q2", 91250.75},
	},
}

var (
	depts   = []string{"eng", "research", "networking", "sre", "qa", "design"}
	regions = []string{"emea", "amer", "apac", "latam"}
)

// scaleRows builds a deterministic large fixture.
func scaleRows(n int) [][]any {
	rows := make([][]any, 0, n+1)
	rows = append(rows, []any{"id", "name", "dept", "region", "salary", "hired", "active", "_rid"})
	start := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		rows = append(rows, []any{
			i,
			fmt.Sprintf("Person %06d", i),
			depts[i%len(depts)],
			regions[i%len(regions)],
			50000 + (i*7919)%150000,
			start.AddDate(0, 0, (i*13)%5000).Format("2006-01-02"),
			i%3 != 0,
			i,
		})
	}
	return rows
}

// bigRows builds one side of a large three-way join. Keys are spread so that
// every row matches exactly one row in each of the other two tables.
func bigRows(kind string, n int) [][]any {
	rows := make([][]any, 0, n+1)
	switch kind {
	case "big_a":
		rows = append(rows, []any{"id", "b_key", "c_key", "amount"})
		for i := 1; i <= n; i++ {
			rows = append(rows, []any{i, (i*7919)%n + 1, (i*104729)%n + 1, 100 + (i*31)%9000})
		}
	default:
		rows = append(rows, []any{"id", "label", "grp"})
		for i := 1; i <= n; i++ {
			rows = append(rows, []any{i, fmt.Sprintf("%s-%06d", kind, i), i % 50})
		}
	}
	return rows
}

func main() {
	id := flag.String("id", os.Getenv("SHEETSQL_ID"), "spreadsheet id")
	scale := flag.Int("scale", 0, "also build a 'scale' tab with N rows")
	big := flag.Int("big", 0, "also build big_a/big_b/big_c with N rows each")
	flag.Parse()
	if *id == "" {
		log.Fatal("need -id <spreadsheetID>")
	}

	ctx := context.Background()
	svc, err := sheets.NewService(ctx,
		option.WithCredentialsFile(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")),
		option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		log.Fatalf("service: %v", err)
	}

	work := map[string][][]any{}
	for k, v := range fixtures {
		work[k] = v
	}
	if *scale > 0 {
		work["scale"] = scaleRows(*scale)
	}
	if *big > 0 {
		for _, k := range []string{"big_a", "big_b", "big_c"} {
			work[k] = bigRows(k, *big)
		}
	}

	ss, err := svc.Spreadsheets.Get(*id).Fields("sheets.properties(title,sheetId,gridProperties)").Do()
	if err != nil {
		log.Fatalf("open spreadsheet: %v", err)
	}
	existing := map[string]bool{}
	gid := map[string]int64{}
	rowCap := map[string]int64{}
	for _, s := range ss.Sheets {
		existing[s.Properties.Title] = true
		gid[s.Properties.Title] = s.Properties.SheetId
		if s.Properties.GridProperties != nil {
			rowCap[s.Properties.Title] = s.Properties.GridProperties.RowCount
		}
	}

	var reqs []*sheets.Request
	for name := range work {
		if !existing[name] {
			reqs = append(reqs, &sheets.Request{
				AddSheet: &sheets.AddSheetRequest{
					Properties: &sheets.SheetProperties{Title: name},
				},
			})
		}
	}
	if len(reqs) > 0 {
		if _, err := svc.Spreadsheets.BatchUpdate(*id,
			&sheets.BatchUpdateSpreadsheetRequest{Requests: reqs}).Do(); err != nil {
			log.Fatalf("add tabs: %v", err)
		}
		fmt.Printf("created %d tab(s)\n", len(reqs))
		ss, err = svc.Spreadsheets.Get(*id).Fields("sheets.properties(title,sheetId,gridProperties)").Do()
		if err != nil {
			log.Fatalf("re-read tabs: %v", err)
		}
		for _, s := range ss.Sheets {
			gid[s.Properties.Title] = s.Properties.SheetId
			if s.Properties.GridProperties != nil {
				rowCap[s.Properties.Title] = s.Properties.GridProperties.RowCount
			}
		}
	}

	// Size each grid to its data. A new tab defaults to 26 columns, so three
	// 100k-row tabs would occupy ~8M of the spreadsheet's 10M cell budget and
	// every read of them slows to tens of seconds.
	var grow []*sheets.Request
	for name, vals := range work {
		rows := int64(len(vals)) + 10
		cols := int64(len(vals[0]))
		grow = append(grow, &sheets.Request{
			UpdateSheetProperties: &sheets.UpdateSheetPropertiesRequest{
				Properties: &sheets.SheetProperties{
					SheetId:        gid[name],
					GridProperties: &sheets.GridProperties{RowCount: rows, ColumnCount: cols},
				},
				Fields: "gridProperties(rowCount,columnCount)",
			},
		})
		_ = rowCap
	}
	if len(grow) > 0 {
		if _, err := svc.Spreadsheets.BatchUpdate(*id,
			&sheets.BatchUpdateSpreadsheetRequest{Requests: grow}).Do(); err != nil {
			log.Fatalf("grow grid: %v", err)
		}
		fmt.Printf("grew %d grid(s)\n", len(grow))
	}

	for name, vals := range work {
		if _, err := svc.Spreadsheets.Values.Clear(*id, name,
			&sheets.ClearValuesRequest{}).Do(); err != nil {
			log.Fatalf("clear %s: %v", name, err)
		}
		// Large fixtures are written in chunks to keep each payload modest.
		const chunk = 10000
		for off := 0; off < len(vals); off += chunk {
			end := min(off+chunk, len(vals))
			rng := fmt.Sprintf("%s!A%d", name, off+1)
			if _, err := svc.Spreadsheets.Values.Update(*id, rng,
				&sheets.ValueRange{Values: vals[off:end]}).
				ValueInputOption("USER_ENTERED").Do(); err != nil {
				log.Fatalf("write %s rows %d-%d: %v", name, off, end, err)
			}
		}
		fmt.Printf("loaded %-10s %d rows\n", name, len(vals)-1)
	}
	fmt.Printf("\nhttps://docs.google.com/spreadsheets/d/%s/edit\n", *id)
}
