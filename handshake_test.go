package recws

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// refusingServer answers every upgrade with status and the given headers, and
// counts the attempts.
func refusingServer(t *testing.T, status int, header map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		http.Error(w, "refused", status)
	}))
	t.Cleanup(s.Close)
	return s, &attempts
}

// dialRefusing dials s with a short handshake timeout so Dial returns
// promptly (it otherwise blocks up to HandshakeTimeout while refused) and
// the timings below count from the first attempt.
func dialRefusing(t *testing.T, s *httptest.Server, min, max time.Duration) *RecConn {
	t.Helper()
	rc := &RecConn{
		HandshakeTimeout: 200 * time.Millisecond,
		RecIntvlMin:      min,
		RecIntvlMax:      max,
		RecIntvlFactor:   1.5,
		Logger:           slog.Default(),
	}
	rc.Dial("ws"+strings.TrimPrefix(s.URL, "http"), nil)
	t.Cleanup(rc.Close)
	return rc
}

// A 429 that says when to come back is honored: the client waits out
// Retry-After instead of retrying on its own (much shorter) backoff.
func TestHandshake_429HonorsRetryAfter(t *testing.T) {
	s, attempts := refusingServer(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "2"})
	dialRefusing(t, s, 50*time.Millisecond, 100*time.Millisecond)

	time.Sleep(1300 * time.Millisecond) // ~1.5s after the first attempt
	if n := attempts.Load(); n != 1 {
		t.Fatalf("expected a single attempt while Retry-After runs, got %d", n)
	}
	time.Sleep(1200 * time.Millisecond) // ~2.7s: Retry-After has elapsed
	if n := attempts.Load(); n < 2 {
		t.Fatalf("expected a retry after Retry-After elapsed, got %d attempts", n)
	}
}

// An answer no retry can fix backs off to the ceiling rather than hammering.
func TestHandshake_403BacksOffToCeiling(t *testing.T) {
	s, attempts := refusingServer(t, http.StatusForbidden, nil)
	dialRefusing(t, s, 50*time.Millisecond, 1*time.Second)

	time.Sleep(1300 * time.Millisecond) // ~1.5s after the first attempt
	if n := attempts.Load(); n > 2 {
		t.Fatalf("expected at most 2 attempts in 1.5s at a 1s ceiling, got %d", n)
	}
}

// A transient failure keeps the caller's backoff.
func TestHandshake_503KeepsBackoff(t *testing.T) {
	s, attempts := refusingServer(t, http.StatusServiceUnavailable, nil)
	dialRefusing(t, s, 50*time.Millisecond, 100*time.Millisecond)

	time.Sleep(1 * time.Second)
	if n := attempts.Load(); n < 5 {
		t.Fatalf("expected the fast backoff to keep retrying, got %d attempts", n)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{"-1", 0},
		{"soon", 0},
		{now.Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-90 * time.Second).UTC().Format(http.TimeFormat), 0},
	}
	for _, c := range cases {
		resp := &http.Response{Header: http.Header{}}
		if c.header != "" {
			resp.Header.Set("Retry-After", c.header)
		}
		if got := retryAfter(resp, now); got != c.want {
			t.Errorf("Retry-After %q: got %v want %v", c.header, got, c.want)
		}
	}
}
