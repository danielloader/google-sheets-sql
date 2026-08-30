package sheetsql

import (
	"database/sql/driver"
	"io"
	"strings"
	"testing"
	"time"
)

func TestConvertCell(t *testing.T) {
	cases := []struct {
		v    any
		typ  string
		want any
	}{
		{nil, "string", nil},
		{"hi", "string", "hi"},
		{float64(42), "number", int64(42)},
		{float64(42.5), "number", 42.5},
		{true, "boolean", true},
	}
	for _, c := range cases {
		got, err := convertCell(c.v, c.typ)
		if err != nil {
			t.Errorf("convertCell(%v,%s): %v", c.v, c.typ, err)
			continue
		}
		if got != c.want {
			t.Errorf("convertCell(%v,%s) = %#v, want %#v", c.v, c.typ, got, c.want)
		}
	}
}

func TestConvertDateCell(t *testing.T) {
	got, err := convertCell("Date(2019,2,1)", "date")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)
	if !got.(time.Time).Equal(want) {
		t.Errorf("got %v want %v (gviz months are 0-based)", got, want)
	}

	got, err = convertCell("Date(2019,2,1,10,30,15)", "datetime")
	if err != nil {
		t.Fatal(err)
	}
	want = time.Date(2019, 3, 1, 10, 30, 15, 0, time.UTC)
	if !got.(time.Time).Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestUnwrapJSONP(t *testing.T) {
	in := []byte("/*O_o*/\ngoogle.visualization.Query.setResponse({\"status\":\"ok\"});")
	out, err := unwrapJSONP(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"status":"ok"}` {
		t.Errorf("got %s", out)
	}
	if _, err := unwrapJSONP([]byte("<html>nope</html>")); err == nil {
		t.Error("expected error for HTML response")
	}
}

func TestParseDSN(t *testing.T) {
	cfg, err := ParseDSN("sheets://abc123?credentials=/k.json&sheet=employees&header=2&rate=30")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpreadsheetID != "abc123" || cfg.Sheet != "employees" ||
		cfg.HeaderRow != 2 || cfg.RatePerMin != 30 || cfg.CredentialsFile != "/k.json" {
		t.Errorf("bad parse: %+v", cfg)
	}

	cfg, err = ParseDSN("https://docs.google.com/spreadsheets/d/XYZ/edit")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpreadsheetID != "XYZ" {
		t.Errorf("browser URL: got %q", cfg.SpreadsheetID)
	}
	if _, err := ParseDSN("sheets://abc?bogus=1"); err == nil {
		t.Error("expected error for unknown parameter")
	}
}

// gviz returns no rows for a bare aggregate that matches nothing; SQL requires
// one row, with 0 for count and NULL for the others.
func TestBareAggregateSynthesisesRow(t *testing.T) {
	tbl := &gvizTable{Cols: []gvizCol{
		{ID: "count-A", Label: "count id", Type: "number"},
		{ID: "sum-D", Label: "sum salary", Type: "number"},
	}}
	cols := []resultCol{{Name: "n", Agg: "count"}, {Name: "total", Agg: "sum"}}

	r := newRows(tbl, cols, true)
	dest := make([]driver.Value, 2)
	if err := r.Next(dest); err != nil {
		t.Fatalf("expected one synthesised row, got %v", err)
	}
	if dest[0] != int64(0) {
		t.Errorf("count = %#v, want int64(0)", dest[0])
	}
	if dest[1] != nil {
		t.Errorf("sum = %#v, want nil", dest[1])
	}
	if err := r.Next(dest); err != io.EOF {
		t.Errorf("want EOF after the single row, got %v", err)
	}
}

func TestGroupedAggregateNotSynthesised(t *testing.T) {
	tbl := &gvizTable{Cols: []gvizCol{{ID: "A", Type: "string"}}}
	r := newRows(tbl, []resultCol{{Name: "dept"}}, false)
	if err := r.Next(make([]driver.Value, 1)); err != io.EOF {
		t.Errorf("GROUP BY with no matches must stay empty, got %v", err)
	}
}

// A read-only DSN must request read-only scopes: refusing writes in the driver
// alone would still hand out a credential that can write.
func TestConfigScopes(t *testing.T) {
	rw, err := ParseDSN("sheets://abc")
	if err != nil {
		t.Fatal(err)
	}
	if got := rw.scopes(); len(got) != 1 || got[0] != scope {
		t.Errorf("read-write scopes = %v", got)
	}

	ro, err := ParseDSN("sheets://abc?readonly=1")
	if err != nil {
		t.Fatal(err)
	}
	got := ro.scopes()
	if len(got) != 2 {
		t.Fatalf("read-only scopes = %v, want two", got)
	}
	for _, s := range got {
		if !strings.Contains(s, "readonly") {
			t.Errorf("scope %q is not read-only", s)
		}
	}
	// The Visualization endpoint rejects spreadsheets.readonly with 401 and
	// needs a Drive-level scope, so both must be present.
	if got[0] != readOnlyScope || got[1] != readOnlyDriveScope {
		t.Errorf("scopes = %v, want spreadsheets.readonly + drive.readonly", got)
	}
}

func TestParseAccessMode(t *testing.T) {
	cases := []struct {
		dsn  string
		want AccessMode
	}{
		{"sheets://a", AccessReadWrite},
		{"sheets://a?readonly=0", AccessReadWrite},
		{"sheets://a?readonly=false", AccessReadWrite},
		{"sheets://a?readonly=1", AccessStrictReadOnly},
		{"sheets://a?readonly=true", AccessStrictReadOnly},
		{"sheets://a?readonly=strict", AccessStrictReadOnly},
		{"sheets://a?readonly=data", AccessNoDataWrites},
		{"sheets://a?readonly=nowrites", AccessNoDataWrites},
	}
	for _, c := range cases {
		cfg, err := ParseDSN(c.dsn)
		if err != nil {
			t.Errorf("%s: %v", c.dsn, err)
			continue
		}
		if cfg.Access != c.want {
			t.Errorf("%s: access = %v, want %v", c.dsn, cfg.Access, c.want)
		}
	}
	if _, err := ParseDSN("sheets://a?readonly=maybe"); err == nil {
		t.Error("expected an error for an unknown readonly value")
	}
}

// readonly=data keeps a write-capable credential so joins still work, while
// refusing statements that would change a data tab.
func TestAccessModePermissions(t *testing.T) {
	cases := []struct {
		mode                AccessMode
		dataWrites, scratch bool
		strictScopes        bool
	}{
		{AccessReadWrite, true, true, false},
		{AccessNoDataWrites, false, true, false},
		{AccessStrictReadOnly, false, false, true},
	}
	for _, c := range cases {
		cfg := &Config{Access: c.mode}
		if got := cfg.allowsDataWrites(); got != c.dataWrites {
			t.Errorf("%v: allowsDataWrites = %v, want %v", c.mode, got, c.dataWrites)
		}
		if got := cfg.allowsScratchWrites(); got != c.scratch {
			t.Errorf("%v: allowsScratchWrites = %v, want %v", c.mode, got, c.scratch)
		}
		strict := len(cfg.scopes()) == 2
		if strict != c.strictScopes {
			t.Errorf("%v: read-only scopes = %v, want %v", c.mode, strict, c.strictScopes)
		}
	}
}
