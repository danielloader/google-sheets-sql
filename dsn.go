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
	Access          AccessMode
	Scratch         string // sheet used to evaluate compiled formulas
	MaxRows         int    // ceiling on rows read back from a compiled formula
}

// AccessMode says what a connection is permitted to change.
type AccessMode int

const (
	// AccessReadWrite is the default: data writes and joins both allowed.
	AccessReadWrite AccessMode = iota

	// AccessNoDataWrites refuses INSERT, UPDATE and DELETE but keeps a
	// read-write credential, so joins still work. Joins write only to the
	// scratch sheet, never to a data tab. Enforced by the driver alone: the
	// token could write if something bypassed it.
	AccessNoDataWrites

	// AccessStrictReadOnly requests read-only OAuth scopes, so Google refuses
	// any write whatever the driver does. Joins are unavailable, because they
	// need to write a formula to the scratch sheet.
	AccessStrictReadOnly
)

func parseAccessMode(v string) (AccessMode, error) {
	switch strings.ToLower(v) {
	case "0", "false", "off", "":
		return AccessReadWrite, nil
	case "1", "true", "strict":
		return AccessStrictReadOnly, nil
	case "data", "nowrites", "no-writes":
		return AccessNoDataWrites, nil
	}
	return 0, fmt.Errorf("sheetsql: readonly must be one of 1/true/strict, data, or 0/false; got %q", v)
}

// allowsDataWrites reports whether INSERT, UPDATE and DELETE are permitted.
func (c *Config) allowsDataWrites() bool { return c.Access == AccessReadWrite }

// allowsScratchWrites reports whether a formula may be evaluated, which writes
// to the scratch sheet but never to a data tab.
func (c *Config) allowsScratchWrites() bool { return c.Access != AccessStrictReadOnly }

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
			m, err := parseAccessMode(v[0])
			if err != nil {
				return nil, err
			}
			cfg.Access = m
		default:
			return nil, fmt.Errorf("sheetsql: unknown DSN parameter %q", k)
		}
	}
	return cfg, nil
}
