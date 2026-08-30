package sheetsql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// gvizEndpoint is Google's Visualization query endpoint. It is not part of the
// Sheets v4 API: it evaluates a SQL-like query server-side and does not count
// against Sheets API quota. It accepts the same OAuth bearer token.
const gvizEndpoint = "https://docs.google.com/spreadsheets/d/%s/gviz/tq"

type gvizCol struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	Pattern string `json:"pattern"`
}

type gvizCell struct {
	V any    `json:"v"`
	F string `json:"f"`
}

type gvizRow struct {
	C []*gvizCell `json:"c"`
}

type gvizTable struct {
	Cols             []gvizCol `json:"cols"`
	Rows             []gvizRow `json:"rows"`
	ParsedNumHeaders int       `json:"parsedNumHeaders"`
}

type gvizError struct {
	Reason          string `json:"reason"`
	Message         string `json:"message"`
	DetailedMessage string `json:"detailed_message"`
}

type gvizResponse struct {
	Version string      `json:"version"`
	Status  string      `json:"status"`
	Errors  []gvizError `json:"errors"`
	Table   gvizTable   `json:"table"`
}

// QueryError is returned when the Visualization endpoint rejects a query.
type QueryError struct {
	Reason  string
	Message string
	Detail  string
	Query   string
}

func (e *QueryError) Error() string {
	msg := e.Message
	if e.Detail != "" && e.Detail != e.Message {
		msg += ": " + e.Detail
	}
	return fmt.Sprintf("sheetsql: gviz rejected query (%s): %s [tq=%s]", e.Reason, msg, e.Query)
}

// unwrapJSONP strips the "/*O_o*/\ngoogle.visualization.Query.setResponse(...);"
// wrapper the endpoint always emits, even for errors.
func unwrapJSONP(b []byte) ([]byte, error) {
	s := string(b)
	const marker = "setResponse("
	i := strings.Index(s, marker)
	if i < 0 {
		if strings.Contains(s, "<html") || strings.Contains(s, "<HTML") {
			return nil, fmt.Errorf("sheetsql: gviz returned HTML, not data (sheet not shared with this account, or bad spreadsheet id)")
		}
		return nil, fmt.Errorf("sheetsql: unrecognised gviz response: %.200s", s)
	}
	s = s[i+len(marker):]
	j := strings.LastIndex(s, ")")
	if j < 0 {
		return nil, fmt.Errorf("sheetsql: truncated gviz response")
	}
	return []byte(s[:j]), nil
}

func (c *conn) gviz(ctx context.Context, sheet, tq string) (*gvizTable, error) {
	return c.gvizRange(ctx, sheet, tq, "")
}

// gvizRange runs a query against an explicit A1 range. Bounding the range
// matters for schema discovery: gviz reads the whole tab before applying
// "limit 0", which on a large sheet costs as much as a full scan.
func (c *conn) gvizRange(ctx context.Context, sheet, tq, a1 string) (*gvizTable, error) {
	if err := c.limiter.wait(ctx); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("tqx", "out:json")
	q.Set("headers", fmt.Sprint(c.cfg.HeaderRow))
	if sheet != "" {
		q.Set("sheet", sheet)
	}
	q.Set("tq", tq)
	if a1 != "" {
		q.Set("range", a1)
	}
	u := fmt.Sprintf(gvizEndpoint, c.cfg.SpreadsheetID) + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sheetsql: gviz request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sheetsql: gviz http %d: %.300s", resp.StatusCode, body)
	}

	raw, err := unwrapJSONP(body)
	if err != nil {
		return nil, err
	}
	var gr gvizResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, fmt.Errorf("sheetsql: decode gviz response: %w", err)
	}
	if gr.Status == "error" {
		e := &QueryError{Query: tq}
		if len(gr.Errors) > 0 {
			e.Reason, e.Message, e.Detail = gr.Errors[0].Reason, gr.Errors[0].Message, gr.Errors[0].DetailedMessage
		}
		return nil, e
	}
	return &gr.Table, nil
}
