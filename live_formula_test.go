package sheetsql

import (
	"context"
	"database/sql/driver"
	"os"
	"testing"
)

func liveConn(t *testing.T) *conn {
	t.Helper()
	dsn := os.Getenv("SHEETSQL_DSN")
	if dsn == "" {
		t.Skip("set SHEETSQL_DSN to run live tests")
	}
	cn, err := Driver{}.OpenConnector(dsn)
	if err != nil {
		t.Fatal(err)
	}
	dc, err := cn.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.Close() })
	return dc.(*conn)
}

// TestLiveEvalFormula checks the scratch-pad round trip: write a formula, let
// Google evaluate it, read the spilled range back.
func TestLiveEvalFormula(t *testing.T) {
	c := liveConn(t)
	grid, err := c.evalFormula(context.Background(), `=QUERY(employees!A:F,"select C, count(A) group by C order by count(A) desc label count(A) ''",1)`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(grid) < 2 {
		t.Fatalf("expected a header plus rows, got %v", grid)
	}
	// employees has eng=4, research=3, networking=1
	if got := grid[1][0]; got != "eng" {
		t.Errorf("top dept = %v, want eng", got)
	}
	if got := grid[1][1]; got != float64(4) {
		t.Errorf("top count = %v (%T), want 4", got, got)
	}
}

// TestLiveFormulaJoin is the capability that gviz cannot express at all.
func TestLiveFormulaJoin(t *testing.T) {
	c := liveConn(t)
	grid, err := c.evalFormula(context.Background(), `=FILTER({employees!B2:B9,ARRAYFORMULA(VLOOKUP(employees!C2:C9,{"eng","Engineering";"research","Research";"networking","Networking"},2,FALSE))},employees!D2:D9>170000)`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(grid) == 0 {
		t.Fatal("join returned nothing")
	}
	for _, row := range grid {
		if len(row) != 2 {
			t.Fatalf("expected 2 columns, got %v", row)
		}
		if row[1] == "" || row[1] == nil {
			t.Errorf("unjoined row: %v", row)
		}
	}
	t.Logf("joined %d rows, first: %v", len(grid), grid[0])
}

func TestLiveFormulaError(t *testing.T) {
	c := liveConn(t)
	_, err := c.evalFormula(context.Background(), `=1/0`, 0)
	if err == nil {
		t.Fatal("expected a formula error")
	}
	var fe *formulaError
	if !asErr(err, &fe) {
		t.Fatalf("expected *formulaError, got %T: %v", err, err)
	}
	if fe.Code != "#DIV/0!" {
		t.Errorf("code = %q, want #DIV/0!", fe.Code)
	}
}

// TestLiveScratchCleared checks the pad does not retain a live formula, which
// would recalculate on every future edit to the spreadsheet.
func TestLiveScratchCleared(t *testing.T) {
	c := liveConn(t)
	ctx := context.Background()
	if _, err := c.evalFormula(ctx, `=QUERY(employees!A:F,"select B",1)`, 0); err != nil {
		t.Fatal(err)
	}
	svc, err := c.sheetsService(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vr, err := svc.Spreadsheets.Values.Get(c.cfg.SpreadsheetID, c.scratchSheet()).Do()
	if err != nil {
		t.Fatal(err)
	}
	if len(vr.Values) != 0 {
		t.Errorf("scratch sheet still holds %d row(s): %v", len(vr.Values), vr.Values)
	}
}

// TestLiveScalarPushdown confirms gviz evaluates scalar functions server-side.
func TestLiveScalarPushdown(t *testing.T) {
	c := liveConn(t)
	rows, err := c.QueryContext(context.Background(),
		"SELECT name FROM employees WHERE year(hired) > 2019 AND upper(dept) = 'RESEARCH'", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	dest := make([]driver.Value, 1)
	for rows.Next(dest) == nil {
		names = append(names, dest[0].(string))
	}
	// Both research hires are after 2019: Hamilton (2020) and Bartik (2022).
	if len(names) != 2 || names[0] != "Margaret Hamilton" || names[1] != "Jean Bartik" {
		t.Errorf("got %v, want [Margaret Hamilton Jean Bartik]", names)
	}
}

func asErr(err error, target **formulaError) bool {
	fe, ok := err.(*formulaError)
	if ok {
		*target = fe
	}
	return ok
}
