// Package sheetsql is a database/sql driver that treats a Google Spreadsheet as
// a database: one tab per table, the header row as the schema.
//
// SELECT statements are translated into the Google Visualization query language
// and evaluated by Google, so filtering, grouping, ordering and limiting all
// happen server-side rather than by fetching the sheet and scanning it locally.
package sheetsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/xwb1989/sqlparser"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

func init() { sql.Register("sheets", Driver{}) }

// debug echoes each translated gviz query and compiled formula to stderr.
var debug = os.Getenv("SHEETSQL_DEBUG") != ""

var stderr = os.Stderr

const (
	scope = "https://www.googleapis.com/auth/spreadsheets"
	// readOnlyScopes are requested for a read-only DSN so the credential itself
	// cannot write, rather than relying on the driver to refuse.
	//
	// Both are needed. spreadsheets.readonly covers the Sheets API, but the
	// Visualization endpoint is served from docs.google.com and rejects it with
	// 401; it accepts a Drive-level scope instead.
	readOnlyScope      = "https://www.googleapis.com/auth/spreadsheets.readonly"
	readOnlyDriveScope = "https://www.googleapis.com/auth/drive.readonly"
)

// scopes reports the OAuth scopes a configuration needs.
func (cfg *Config) scopes() []string {
	if cfg.Access == AccessStrictReadOnly {
		return []string{readOnlyScope, readOnlyDriveScope}
	}
	return []string{scope}
}

type Driver struct{}

var (
	_ driver.Driver        = Driver{}
	_ driver.DriverContext = Driver{}
)

func (d Driver) Open(dsn string) (driver.Conn, error) {
	c, err := d.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

func (d Driver) OpenConnector(dsn string) (driver.Connector, error) {
	cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return &connector{cfg: cfg, limiter: sharedLimiter(cfg.SpreadsheetID, cfg.RatePerMin)}, nil
}

type connector struct {
	cfg     *Config
	limiter *limiter

	mu     sync.Mutex
	client *http.Client
}

func (c *connector) Driver() driver.Driver { return Driver{} }

func (c *connector) httpClient(ctx context.Context) (*http.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	var (
		cl  *http.Client
		err error
	)
	if c.cfg.CredentialsFile != "" {
		b, rerr := os.ReadFile(c.cfg.CredentialsFile)
		if rerr != nil {
			return nil, fmt.Errorf("sheetsql: read credentials: %w", rerr)
		}
		jwt, jerr := google.JWTConfigFromJSON(b, c.cfg.scopes()...)
		if jerr != nil {
			return nil, fmt.Errorf("sheetsql: parse credentials: %w", jerr)
		}
		cl = jwt.Client(ctx)
	} else {
		cl, err = google.DefaultClient(ctx, c.cfg.scopes()...)
		if err != nil {
			return nil, fmt.Errorf("sheetsql: default credentials: %w", err)
		}
	}
	cl.Timeout = c.cfg.Timeout
	cl.Transport = newRetryTransport(cl.Transport)
	c.client = cl
	return cl, nil
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	cl, err := c.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	cn := &conn{
		cfg:     c.cfg,
		http:    cl,
		limiter: c.limiter,
		schemas: newSchemaCache(),
	}
	cn.pad = sharedPad(c.cfg.SpreadsheetID, cn.scratchSheet())
	return cn, nil
}

type conn struct {
	cfg     *Config
	http    *http.Client
	limiter *limiter
	schemas *schemaCache

	tabs    tabIndex
	pad     *scratchPad
	svcOnce sync.Once
	svc     *sheets.Service
	svcErr  error

	tx *sheetTx
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
)

// sheetsService is only needed for writes; reads never touch the Sheets v4 API.
func (c *conn) sheetsService(ctx context.Context) (*sheets.Service, error) {
	c.svcOnce.Do(func() {
		c.svc, c.svcErr = sheets.NewService(ctx, option.WithHTTPClient(c.http))
	})
	return c.svc, c.svcErr
}

func (c *conn) Close() error { return nil }

func (c *conn) Ping(ctx context.Context) error {
	sheet, err := c.resolveSheet(ctx, "")
	if err != nil {
		return err
	}
	_, err = c.gviz(ctx, sheet, "select * limit 0")
	return err
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	stmt, err := sqlparser.Parse(query)
	if err != nil {
		return nil, fmt.Errorf("sheetsql: parse: %w", err)
	}
	if u, ok := stmt.(*sqlparser.Union); ok {
		return c.queryUnion(ctx, u, args)
	}
	sel, ok := stmt.(*sqlparser.Select)
	if !ok {
		return nil, fmt.Errorf("sheetsql: %T is not a query; use Exec", stmt)
	}

	// Anything gviz cannot express on its own -- a join, or a HAVING clause --
	// is compiled into a spreadsheet formula instead.
	if needsFormula(sel) {
		return c.queryFormula(ctx, sel, args)
	}

	sheet, err := sheetFromSelect(sel)
	if err != nil {
		return nil, err
	}
	sheet, err = c.resolveSheet(ctx, sheet)
	if err != nil {
		return nil, err
	}

	s, err := c.schemas.get(ctx, c, sheet)
	if err != nil {
		return nil, err
	}
	tr := &translator{src: s, args: args}
	out, err := tr.translateSelect(sel)
	if err != nil {
		return nil, err
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[sheetsql] sheet=%q tq=%s\n", sheet, out.TQ)
	}
	tbl, err := c.gviz(ctx, sheet, out.TQ)
	if err != nil {
		return nil, err
	}
	return newRows(tbl, out.Cols, out.BareAggregate), nil
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if _, err := sqlparser.Parse(query); err != nil {
		return nil, fmt.Errorf("sheetsql: parse: %w", err)
	}
	return &stmt{c: c, query: query}, nil
}

type stmt struct {
	c     *conn
	query string
}

var (
	_ driver.Stmt             = (*stmt)(nil)
	_ driver.StmtQueryContext = (*stmt)(nil)
	_ driver.StmtExecContext  = (*stmt)(nil)
)

func (s *stmt) Close() error { return nil }

// NumInput returns -1 so database/sql does not try to count placeholders; the
// translator validates argument count when it resolves them.
func (s *stmt) NumInput() int { return -1 }

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), valuesToNamed(args))
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.query, args)
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.query, args)
}

func valuesToNamed(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

// errReadOnly is returned for any statement that would modify a data tab on a
// connection opened with readonly=1 or readonly=data.
var errReadOnly = errors.New("sheetsql: connection is read-only (remove readonly from the DSN to allow writes)")

func normaliseSheet(name string) string {
	// gviz and the values API disagree about quoting; keep the bare name and
	// quote only where A1 notation requires it.
	if strings.ContainsAny(name, " '!") {
		return "'" + strings.ReplaceAll(name, "'", "''") + "'"
	}
	return name
}
