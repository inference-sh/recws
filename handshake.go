package recws

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// handshakeBodyLimit bounds how much of a refused handshake's body is logged.
const handshakeBodyLimit = 256

// handshakeBackoff logs a failed dial with what the server actually answered
// and returns how long to wait before the next attempt.
//
// gorilla reports every non-101 answer as ErrBadHandshake, which hides the
// one thing an operator needs: the status. Worse, a client that retries a
// 429 on its own schedule feeds the very storm the server is refusing. So a
// 429 with Retry-After waits at least that long, and an answer that no retry
// can fix — bad credentials, a node the caller does not own, an unknown id —
// backs off to the ceiling instead of hammering. Everything else keeps the
// caller's backoff.
func (rc *RecConn) handshakeBackoff(err error, resp *http.Response, next time.Duration) time.Duration {
	if !errors.Is(err, websocket.ErrBadHandshake) || resp == nil {
		rc.Logger.Error("connection error", "error", err)
		return next
	}

	body := readHandshakeBody(resp)
	rc.Logger.Error("connection error", "error", err, "status", resp.StatusCode, "body", body)

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		if ra := retryAfter(resp, time.Now()); ra > next {
			return ra
		}
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		if ceiling := rc.getRecIntvlMax(); ceiling > next {
			return ceiling
		}
	}
	return next
}

// retryAfter reads an RFC 7231 Retry-After header in either of its forms
// (delay-seconds or HTTP-date). Zero when absent or unparseable.
func retryAfter(resp *http.Response, now time.Time) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// readHandshakeBody returns a trimmed prefix of the refused response's body,
// which gorilla has already buffered, and closes it.
func readHandshakeBody(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, handshakeBodyLimit))
	return strings.TrimSpace(string(b))
}

func (rc *RecConn) getRecIntvlMax() time.Duration {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.RecIntvlMax
}
