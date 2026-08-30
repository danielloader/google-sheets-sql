package sheetsql

import (
	"context"
	"sync"
	"time"
)

// limiter is a token bucket sized to the Sheets per-user quota. database/sql's
// connection pool cannot throttle here: every "connection" is stateless HTTP,
// so SetMaxOpenConns bounds concurrency but not request rate.
type limiter struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rate   float64 // tokens per second
	last   time.Time
}

// limiters are shared process-wide per spreadsheet: the Sheets quota is per
// user, so two sql.DB handles onto the same document draw on one budget.
var (
	limiterMu  sync.Mutex
	limiterReg = map[string]*limiter{}
)

func sharedLimiter(spreadsheetID string, perMin int) *limiter {
	limiterMu.Lock()
	defer limiterMu.Unlock()
	if l, ok := limiterReg[spreadsheetID]; ok {
		return l
	}
	l := newLimiter(perMin)
	limiterReg[spreadsheetID] = l
	return l
}

func newLimiter(perMin int) *limiter {
	if perMin <= 0 {
		perMin = 60
	}
	return &limiter{
		// Start half full: a full bucket lets an idle process fire perMin
		// requests instantly, which Google's rolling window counts against the
		// following minute too.
		tokens: float64(perMin) / 2,
		max:    float64(perMin),
		rate:   float64(perMin) / 60,
		last:   time.Now(),
	}
}

func (l *limiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = min(l.max, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		deficit := (1 - l.tokens) / l.rate
		l.mu.Unlock()

		t := time.NewTimer(time.Duration(deficit * float64(time.Second)))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
