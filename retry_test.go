package sheetsql

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	rt := newRetryTransport(nil)
	rt.backoff = time.Millisecond
	c := &http.Client{Transport: rt}

	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("made %d calls, want 3", got)
	}
}

// A POST that fails with 5xx must not be replayed: the write may have applied.
func TestNoRetryOnServerErrorForWrites(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	rt := newRetryTransport(nil)
	rt.backoff = time.Millisecond
	c := &http.Client{Transport: rt}

	resp, err := c.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("POST retried %d times on 5xx; writes must not be replayed", got-1)
	}
}

// A GET may be retried on 5xx, since a read has no side effect.
func TestRetryOnServerErrorForReads(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	rt := newRetryTransport(nil)
	rt.backoff = time.Millisecond
	c := &http.Client{Transport: rt}

	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("made %d calls, want 2", got)
	}
}

// A retried POST must still send its body.
func TestRetryPreservesBody(t *testing.T) {
	var bodies []string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if atomic.AddInt32(&calls, 1) < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	rt := newRetryTransport(nil)
	rt.backoff = time.Millisecond
	c := &http.Client{Transport: rt}
	resp, err := c.Post(srv.URL, "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(bodies) != 2 || bodies[0] != `{"a":1}` || bodies[1] != `{"a":1}` {
		t.Errorf("bodies = %q, want the payload sent twice", bodies)
	}
}

func TestLimiterStartsBelowFull(t *testing.T) {
	l := newLimiter(60)
	if l.tokens >= 60 {
		t.Errorf("bucket starts at %v; a full bucket bursts into the next quota window", l.tokens)
	}
}
