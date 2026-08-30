package sheetsql

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net/http"
	"time"
)

// retryTransport retries requests Google rejects for rate or availability
// reasons. Both engines go through one http.Client, so wrapping the transport
// covers the Sheets API and the Visualization endpoint alike.
//
// A 429 is safe to retry for any method: the request was refused, not applied.
// A 5xx is only retried for GET, because a write that timed out server-side may
// well have taken effect and replaying it would duplicate rows.
type retryTransport struct {
	base    http.RoundTripper
	max     int
	backoff time.Duration
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func newRetryTransport(base http.RoundTripper) *retryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &retryTransport{base: base, max: 5, backoff: 700 * time.Millisecond}
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil && req.GetBody == nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}

	var resp *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		r := req
		if body != nil {
			r = req.Clone(req.Context())
			r.Body = io.NopCloser(bytesReader(body))
		} else if req.GetBody != nil && attempt > 0 {
			nb, gerr := req.GetBody()
			if gerr != nil {
				return resp, err
			}
			r = req.Clone(req.Context())
			r.Body = nb
		}

		resp, err = t.base.RoundTrip(r)
		if attempt >= t.max || !t.shouldRetry(req, resp, err) {
			return resp, err
		}
		if resp != nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
		}
		if werr := sleepCtx(req.Context(), t.wait(attempt, resp)); werr != nil {
			return nil, werr
		}
	}
}

func (t *retryTransport) shouldRetry(req *http.Request, resp *http.Response, err error) bool {
	if err != nil {
		return false // transport errors may have applied a write; do not replay
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return true
	case resp.StatusCode >= 500 && req.Method == http.MethodGet:
		return true
	}
	return false
}

// wait honours Retry-After when present, otherwise backs off exponentially
// with jitter so concurrent connections do not retry in lockstep.
func (t *retryTransport) wait(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if v := resp.Header.Get("Retry-After"); v != "" {
			if d, err := time.ParseDuration(v + "s"); err == nil {
				return d
			}
		}
	}
	d := t.backoff << attempt
	return d + time.Duration(rand.Int63n(int64(d/2+1)))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
