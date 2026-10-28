package recws

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// kaServer is a test websocket server that records when each connection was
// accepted.
type kaServer struct {
	*httptest.Server

	mu      sync.Mutex
	accepts []time.Time
}

func (s *kaServer) URL() string { return "ws" + strings.TrimPrefix(s.Server.URL, "http") }

func (s *kaServer) acceptTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.accepts...)
}

// newKAServer starts a server. With pong false it never answers pings, like
// a peer that is gone. With every > 0 it also sends a text message at that
// interval.
func newKAServer(t *testing.T, pong bool, every time.Duration) *kaServer {
	t.Helper()
	s := &kaServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		s.mu.Lock()
		s.accepts = append(s.accepts, time.Now())
		s.mu.Unlock()
		if !pong {
			c.SetPingHandler(func(string) error { return nil })
		}
		stop := make(chan struct{})
		defer close(stop)
		if every > 0 {
			go func() {
				tk := time.NewTicker(every)
				defer tk.Stop()
				for {
					select {
					case <-stop:
						return
					case <-tk.C:
						if err := c.WriteMessage(websocket.TextMessage, []byte("tick")); err != nil {
							return
						}
					}
				}
			}()
		}
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func quietLogger() Logger {
	if testing.Verbose() {
		return slog.Default()
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readLoop keeps a reader on rc so control frames (pongs) are handled.
func readLoop(rc *RecConn, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		if _, _, err := rc.ReadMessage(); err != nil {
			time.Sleep(time.Millisecond)
		}
	}
}

func waitConnected(t *testing.T, rc *RecConn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !rc.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for connection")
		}
		time.Sleep(time.Millisecond)
	}
}

// A healthy, fast peer must never be judged dead. With the old loop (one
// ping per timeout, checked against that same timeout) this reconnected at
// random on localhost.
func TestKeepAlive_FastPeerNoFalseTimeout(t *testing.T) {
	srv := newKAServer(t, true, 0)

	const (
		timeout = 60 * time.Millisecond
		periods = 50
	)
	rc := &RecConn{
		KeepAliveTimeout: timeout,
		RecIntvlMin:      20 * time.Millisecond,
		RecIntvlMax:      100 * time.Millisecond,
		Logger:           quietLogger(),
	}
	rc.Dial(srv.URL(), nil)
	defer rc.Close()
	waitConnected(t, rc)

	stop := make(chan struct{})
	defer close(stop)
	go readLoop(rc, stop)

	time.Sleep(periods * timeout)
	if n := len(srv.acceptTimes()); n != 1 {
		t.Fatalf("healthy peer: want 1 connection over %d timeouts, got %d", periods, n)
	}
}

// A peer that stops answering pings must be redialed within about
// timeout + interval, and not before a full timeout from the connect.
func TestKeepAlive_DeadPeerReconnects(t *testing.T) {
	srv := newKAServer(t, false, 0)

	const (
		timeout  = 150 * time.Millisecond
		interval = 30 * time.Millisecond
		slack    = 100 * time.Millisecond // scheduling, -race, redial
	)
	rc := &RecConn{
		KeepAliveTimeout:      timeout,
		KeepAlivePingInterval: interval,
		RecIntvlMin:           20 * time.Millisecond,
		RecIntvlMax:           100 * time.Millisecond,
		Logger:                quietLogger(),
	}
	rc.Dial(srv.URL(), nil)
	defer rc.Close()

	stop := make(chan struct{})
	defer close(stop)
	go readLoop(rc, stop)

	deadline := time.Now().Add(5 * time.Second)
	var accepts []time.Time
	for {
		accepts = srv.acceptTimes()
		if len(accepts) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(accepts) < 2 {
		t.Fatalf("dead peer: no reconnect within 5s")
	}
	// The server stamps the accept a little before the client starts its
	// clock, so allow a few milliseconds under the timeout. Measuring from a
	// zero last-response time would redial at the first ping interval.
	gap := accepts[1].Sub(accepts[0])
	if gap < timeout-interval/2 {
		t.Fatalf("dead peer: redialed after %v, before the %v timeout", gap, timeout)
	}
	if gap > timeout+interval+slack {
		t.Fatalf("dead peer: redialed after %v, want <= %v", gap, timeout+interval+slack)
	}
}

// Any message read through RecConn counts as a sign of life, so a peer that
// streams data but never answers pings is not redialed.
func TestKeepAlive_MessagesCountAsResponse(t *testing.T) {
	srv := newKAServer(t, false, 5*time.Millisecond)

	const timeout = 50 * time.Millisecond
	rc := &RecConn{
		KeepAliveTimeout: timeout,
		RecIntvlMin:      20 * time.Millisecond,
		RecIntvlMax:      100 * time.Millisecond,
		Logger:           quietLogger(),
	}
	rc.Dial(srv.URL(), nil)
	defer rc.Close()
	waitConnected(t, rc)

	stop := make(chan struct{})
	defer close(stop)
	go readLoop(rc, stop)

	time.Sleep(20 * timeout)
	if n := len(srv.acceptTimes()); n != 1 {
		t.Fatalf("streaming peer: want 1 connection, got %d", n)
	}
}

// A caller's pong handler runs and does not replace keepalive's.
func TestKeepAlive_CallerPongHandlerKept(t *testing.T) {
	srv := newKAServer(t, true, 0)

	const timeout = 60 * time.Millisecond
	rc := &RecConn{
		KeepAliveTimeout: timeout,
		RecIntvlMin:      20 * time.Millisecond,
		RecIntvlMax:      100 * time.Millisecond,
		Logger:           quietLogger(),
	}
	rc.Dial(srv.URL(), nil)
	defer rc.Close()
	waitConnected(t, rc)

	var pongs atomic.Int32
	rc.SetPongHandler(func(string) error {
		pongs.Add(1)
		return nil
	})

	stop := make(chan struct{})
	defer close(stop)
	go readLoop(rc, stop)

	time.Sleep(15 * timeout)
	if pongs.Load() == 0 {
		t.Fatal("caller pong handler never ran")
	}
	if n := len(srv.acceptTimes()); n != 1 {
		t.Fatalf("caller pong handler: want 1 connection, got %d", n)
	}
}

func TestKeepAlive_PingIntervalDefault(t *testing.T) {
	for _, tc := range []struct {
		timeout, interval, want time.Duration
	}{
		{60 * time.Second, 0, 20 * time.Second},
		{60 * time.Second, 5 * time.Second, 5 * time.Second},
		{60 * time.Second, 90 * time.Second, 60 * time.Second},
		{2 * time.Nanosecond, 0, 2 * time.Nanosecond},
	} {
		rc := &RecConn{KeepAliveTimeout: tc.timeout, KeepAlivePingInterval: tc.interval}
		if got := rc.getKeepAlivePingInterval(); got != tc.want {
			t.Errorf("timeout %v interval %v: got %v, want %v", tc.timeout, tc.interval, got, tc.want)
		}
	}
}
