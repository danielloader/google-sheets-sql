package sheetsql

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config is a parsed DSN.
//
// Accepted forms:
//
//	sheets://<spreadsheetID>?credentials=/path/key.json&sheet=employees
//	<spreadsheetID>?credentials=/path/key.json
//	https://docs.google.com/spreadsheets/d/<spreadsheetID>/edit#gid=0?credentials=...
type Config struct {
	SpreadsheetID   string
	Sheet           string // default tab when a query names no table
	CredentialsFile string
	HeaderRow       int           // rows to treat as headers; gviz "headers" param
	Timeout         time.Duration // per-request
	RatePerMin      int           // client-side request budget
	ReadOnly        bool
	Scratch         string // sheet used to evaluate compiled formulas
	MaxRows         int    // ceiling on rows read back from a compiled formula
}

func ParseDSN(dsn string) (*Config, error) {
	cfg := &Config{HeaderRow: 1, Timeout: 180 * time.Second, RatePerMin: 60, MaxRows: 50000}

	raw := strings.TrimPrefix(dsn, "sheets://")
	idPart, query, _ := strings.Cut(raw, "?")

	// Tolerate a pasted browser URL.
	if strings.Contains(idPart, "docs.google.com") || strings.Contains(idPart, "/d/") {
		if i := strings.Index(idPart, "/d/"); i >= 0 {
			rest := idPart[i+3:]
			if j := strings.IndexAny(rest, "/#"); j >= 0 {
				rest = rest[:j]
			}
			idPart = rest
		}
	}
	idPart = strings.Trim(idPart, "/")
	if idPart == "" {
		return nil, fmt.Errorf("sheetsql: DSN has no spreadsheet id")
	}
	cfg.SpreadsheetID = idPart

	vals, err := url.ParseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("sheetsql: bad DSN parameters: %w", err)
	}
	for k, v := range vals {
		if len(v) == 0 {
			continue
		}
		switch strings.ToLower(k) {
		case "credentials", "credentials_file":
			cfg.CredentialsFile = v[0]
		case "sheet", "tab":
			cfg.Sheet = v[0]
		case "header", "headers", "header_row":
			n, err := strconv.Atoi(v[0])
			if err != nil {
				return nil, fmt.Errorf("sheetsql: header must be an integer: %q", v[0])
			}
			cfg.HeaderRow = n
		case "timeout":
			d, err := time.ParseDuration(v[0])
			if err != nil {
				return nil, fmt.Errorf("sheetsql: bad timeout: %w", err)
			}
			cfg.Timeout = d
		case "rate_per_min", "rate":
			n, err := strconv.Atoi(v[0])
			if err != nil {
				return nil, fmt.Errorf("sheetsql: bad rate: %w", err)
			}
			cfg.RatePerMin = n
		case "scratch":
			cfg.Scratch = v[0]
		case "max_rows", "maxrows":
			n, err := strconv.Atoi(v[0])
			if err != nil {
				return nil, fmt.Errorf("sheetsql: bad max_rows: %w", err)
			}
			cfg.MaxRows = n
		case "readonly", "read_only":
			cfg.ReadOnly = v[0] == "1" || strings.EqualFold(v[0], "true")
		default:
			return nil, fmt.Errorf("sheetsql: unknown DSN parameter %q", k)
		}
	}
	return cfg, nil
}
