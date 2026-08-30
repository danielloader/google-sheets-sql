// Command dropsheets removes tabs by name and reports the spreadsheet's cell budget.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

func main() {
	id := flag.String("id", os.Getenv("SHEETSQL_ID"), "spreadsheet id")
	drop := flag.String("drop", "", "comma-separated tab names to delete")
	flag.Parse()

	ctx := context.Background()
	b, _ := os.ReadFile(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
	cfg, _ := google.JWTConfigFromJSON(b, "https://www.googleapis.com/auth/spreadsheets")
	hc := cfg.Client(ctx)
	hc.Timeout = 240 * time.Second
	svc, _ := sheets.NewService(ctx, option.WithHTTPClient(hc))

	report := func() {
		ss, err := svc.Spreadsheets.Get(*id).Fields("sheets.properties(title,sheetId,gridProperties)").Do()
		if err != nil {
			fmt.Println("get:", err)
			return
		}
		var total int64
		for _, sh := range ss.Sheets {
			p := sh.Properties
			if p.GridProperties == nil {
				continue
			}
			total += p.GridProperties.RowCount * p.GridProperties.ColumnCount
		}
		fmt.Printf("spreadsheet now holds %d cells across %d tabs\n", total, len(ss.Sheets))
	}

	if *drop == "" {
		report()
		return
	}

	ss, err := svc.Spreadsheets.Get(*id).Fields("sheets.properties(title,sheetId)").Do()
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	targets := map[string]bool{}
	for _, n := range strings.Split(*drop, ",") {
		targets[strings.TrimSpace(n)] = true
	}
	var reqs []*sheets.Request
	for _, sh := range ss.Sheets {
		if targets[sh.Properties.Title] {
			reqs = append(reqs, &sheets.Request{
				DeleteSheet: &sheets.DeleteSheetRequest{SheetId: sh.Properties.SheetId},
			})
			fmt.Println("deleting", sh.Properties.Title)
		}
	}
	if len(reqs) == 0 {
		fmt.Println("nothing to delete")
		report()
		return
	}
	// One request per call: a batch covering several large tabs exceeds the
	// time the backend will spend on a single mutation.
	for _, r := range reqs {
		for attempt := 1; attempt <= 3; attempt++ {
			start := time.Now()
			_, err := svc.Spreadsheets.BatchUpdate(*id,
				&sheets.BatchUpdateSpreadsheetRequest{Requests: []*sheets.Request{r}}).Do()
			if err == nil {
				fmt.Printf("  deleted sheetId %d in %v\n", r.DeleteSheet.SheetId,
					time.Since(start).Round(time.Millisecond))
				break
			}
			fmt.Printf("  sheetId %d attempt %d failed after %v\n", r.DeleteSheet.SheetId,
				attempt, time.Since(start).Round(time.Millisecond))
			time.Sleep(3 * time.Second)
		}
	}
	report()
}
