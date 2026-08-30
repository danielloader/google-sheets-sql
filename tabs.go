package sheetsql

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// tabIndex caches the spreadsheet's tab titles. gviz silently falls back to the
// first tab when the "sheet" parameter names something that does not exist, so
// every table name must be validated before a query is sent.
type tabIndex struct {
	mu     sync.Mutex
	titles []string
	byName map[string]string
}

func (c *conn) resolveSheet(ctx context.Context, name string) (string, error) {
	c.tabs.mu.Lock()
	loaded := c.tabs.byName != nil
	c.tabs.mu.Unlock()

	if !loaded {
		if err := c.loadTabs(ctx); err != nil {
			return "", err
		}
	}

	c.tabs.mu.Lock()
	defer c.tabs.mu.Unlock()
	if name == "" {
		if c.cfg.Sheet != "" {
			name = c.cfg.Sheet
		} else if len(c.tabs.titles) > 0 {
			return c.tabs.titles[0], nil
		}
	}
	if t, ok := c.tabs.byName[strings.ToLower(name)]; ok {
		return t, nil
	}
	return "", fmt.Errorf("sheetsql: no tab named %q in spreadsheet (tabs: %s)",
		name, strings.Join(c.tabs.titles, ", "))
}

func (c *conn) loadTabs(ctx context.Context) error {
	svc, err := c.sheetsService(ctx)
	if err != nil {
		return err
	}
	if err := c.limiter.wait(ctx); err != nil {
		return err
	}
	ss, err := svc.Spreadsheets.Get(c.cfg.SpreadsheetID).
		Fields("sheets.properties.title").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("sheetsql: list tabs: %w", err)
	}
	c.tabs.mu.Lock()
	defer c.tabs.mu.Unlock()
	c.tabs.byName = map[string]string{}
	c.tabs.titles = nil
	for _, s := range ss.Sheets {
		if s.Properties == nil {
			continue
		}
		t := s.Properties.Title
		c.tabs.titles = append(c.tabs.titles, t)
		c.tabs.byName[strings.ToLower(t)] = t
	}
	return nil
}

// sheetID returns the numeric gid for a tab, needed by structural batchUpdate
// requests such as row deletion.
func (c *conn) sheetID(ctx context.Context, title string) (int64, error) {
	svc, err := c.sheetsService(ctx)
	if err != nil {
		return 0, err
	}
	if err := c.limiter.wait(ctx); err != nil {
		return 0, err
	}
	ss, err := svc.Spreadsheets.Get(c.cfg.SpreadsheetID).
		Fields("sheets.properties(title,sheetId)").Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("sheetsql: look up tab id: %w", err)
	}
	for _, s := range ss.Sheets {
		if s.Properties != nil && strings.EqualFold(s.Properties.Title, title) {
			return s.Properties.SheetId, nil
		}
	}
	return 0, fmt.Errorf("sheetsql: no tab named %q", title)
}
