// Package recws provides websocket client based on gorilla/websocket
// that will automatically reconnect if the connection is dropped.
package recws

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
)

// Logger is the interface for logging operations
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// DefaultLogger returns a slog-based default logger
func DefaultLogger() Logger {
	return slog.Default()
}

// ErrNotConnected is returned when the application read/writes
// a message and the connection is closed
var ErrNotConnected = errors.New("websocket: not connected")

// The RecConn type represents a Reconnecting WebSocket connection.
type RecConn struct {
	// RecIntvlMin specifies the initial reconnecting interval,
	// default to 2 seconds
	RecIntvlMin time.Duration
	// RecIntvlMax specifies the maximum reconnecting interval,
	// default to 30 seconds
	RecIntvlMax time.Duration
	// RecIntvlFactor specifies the rate of increase of the reconnection
	// interval, default to 1.5
	RecIntvlFactor float64
	// HandshakeTimeout specifies the duration for the handshake to complete,
	// default to 2 seconds
	HandshakeTimeout time.Duration
	// Proxy specifies the proxy function for the dialer
	// defaults to ProxyFromEnvironment
	Proxy func(*http.Request) (*url.URL, error)
	// Client TLS config to use on reconnect
	TLSClientConfig *tls.Config
	// SubscribeHandler fires after the connection successfully establish.
	SubscribeHandler func() error
	// HeaderFunc, when set, supplies the request header for every dial
	// attempt, in place of the one given to Dial. A connection that outlives
	// its credential needs it: the header given to Dial is fixed for the
	// life of the connection, so a rotated token would be refused on every
	// reconnect. Set it before Dial; it is called from the dial goroutine.
	HeaderFunc func() http.Header
	// KeepAliveTimeout is how long the connection may go without a pong, or
	// any message read through RecConn, before it is closed and redialed.
	// Disabled if 0.
	KeepAliveTimeout time.Duration
	// KeepAlivePingInterval is how often a ping is sent while KeepAliveTimeout
	// is set. Defaults to KeepAliveTimeout/3; values above KeepAliveTimeout
	// are clamped to it. A dead peer is noticed within about
	// KeepAliveTimeout + KeepAlivePingInterval.
	KeepAlivePingInterval time.Duration
	// Compression enables per-message compression as defined in https://datatracker.ietf.org/doc/html/rfc7692
	Compression bool
	// Logger is the logger instance to use. If nil, DefaultLogger() will be used.
	Logger Logger

	isConnected  bool
	reconnecting atomic.Bool // set while a CloseAndReconnect is in flight
	mu           sync.RWMutex
	url          string
	reqHeader    http.Header
	httpResp     *http.Response
	dialErr      error
	dialer       *websocket.Dialer
	done         chan struct{} // Channel to signal goroutine shutdown

	keepAliveResp keepAliveResponse
	pongHandler   func(appData string) error // caller's, kept across reconnects

	*websocket.Conn
}

// CloseAndReconnect will try to reconnect.
// Safe to call concurrently — only the first caller triggers a reconnect,
// subsequent calls are no-ops until the reconnect completes.
func (rc *RecConn) CloseAndReconnect() {
	// A sync.Once replaced with a fresh one after the reconnect raced with
	// concurrent Do calls; a flag does the same job without the race.
	if !rc.reconnecting.CompareAndSwap(false, true) {
		return
	}
	rc.Logger.Info("closing connection and reconnecting")
	rc.Close()
	go func() {
		rc.connect()
		rc.reconnecting.Store(false) // reset for next disconnect
	}()
}

// setIsConnected sets state for isConnected
func (rc *RecConn) setIsConnected(state bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.isConnected = state
}

// Close closes the underlying network connection without
// sending or waiting for a close frame.
//
// It also forgets the last dial's answer. A closed connection has no
// handshake response and no dial error until its next attempt; keeping the
// old ones made a connection that was established and then dropped report
// the 101 that established it, which readers of GetHTTPResponse had to know
// to ignore.
func (rc *RecConn) Close() {
	rc.mu.Lock()
	if rc.done != nil {
		close(rc.done)
		rc.done = nil
	}
	if rc.Conn != nil {
		rc.Conn.Close()
	}
	rc.httpResp = nil
	rc.dialErr = nil
	rc.mu.Unlock()

	rc.setIsConnected(false)
}

// Shutdown gracefully closes the connection by sending the websocket.CloseMessage.
// The writeWait param defines the duration before the deadline of the write operation is hit.
func (rc *RecConn) Shutdown(writeWait time.Duration) {
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	err := rc.WriteControl(websocket.CloseMessage, msg, time.Now().Add(writeWait))
	if err != nil && err != websocket.ErrCloseSent {
		rc.Logger.Error("shutdown error", "error", err)
		rc.Close()
	}
}

// ReadMessage is a helper method for getting a reader
// using NextReader and reading from that reader to a buffer.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) ReadMessage() (messageType int, message []byte, err error) {
	err = ErrNotConnected
	if rc.IsConnected() {
		messageType, message, err = rc.Conn.ReadMessage()
		if err == nil {
			rc.keepAliveResp.setLastResponse()
		}
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return messageType, message, nil
		}
		if err != nil {
			rc.Logger.Error("read message error", "error", err)
			rc.CloseAndReconnect()
		}
	}
	return
}

// WriteMessage is a helper method for getting a writer using NextWriter,
// writing the message and closing the writer.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) WriteMessage(messageType int, data []byte) error {
	err := ErrNotConnected
	if rc.IsConnected() {
		rc.mu.Lock()
		err = rc.Conn.WriteMessage(messageType, data)
		rc.mu.Unlock()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil {
			rc.Logger.Error("write message error", "error", err)
			rc.CloseAndReconnect()
		}
	}
	return err
}

// WriteJSON writes the JSON encoding of v to the connection.
func (rc *RecConn) WriteJSON(v interface{}) error {
	err := ErrNotConnected
	if rc.IsConnected() {
		rc.mu.Lock()
		err = rc.Conn.WriteJSON(v)
		rc.mu.Unlock()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil {
			rc.Logger.Error("write json error", "error", err)
			rc.CloseAndReconnect()
		}
	}
	return err
}

// ReadJSON reads the next JSON-encoded message from the connection.
func (rc *RecConn) ReadJSON(v interface{}) error {
	err := ErrNotConnected
	if rc.IsConnected() {
		err = rc.Conn.ReadJSON(v)
		if err == nil {
			rc.keepAliveResp.setLastResponse()
		}
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil {
			rc.Logger.Error("read json error", "error", err)
			rc.CloseAndReconnect()
		}
	}
	return err
}

func (rc *RecConn) setURL(url string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.url = url
}

func (rc *RecConn) setReqHeader(reqHeader http.Header) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.reqHeader = reqHeader
}

// parseURL parses current url
func (rc *RecConn) parseURL(urlStr string) (string, error) {
	if urlStr == "" {
		return "", errors.New("dial: url cannot be empty")
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return "", errors.New("url: " + err.Error())
	}

	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", errors.New("url: websocket uris must start with ws or wss scheme")
	}

	if u.User != nil {
		return "", errors.New("url: user name and password are not allowed in websocket URIs")
	}

	return urlStr, nil
}

func (rc *RecConn) setDefaultRecIntvlMin() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.RecIntvlMin == 0 {
		rc.RecIntvlMin = 2 * time.Second
	}
}

func (rc *RecConn) setDefaultRecIntvlMax() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.RecIntvlMax == 0 {
		rc.RecIntvlMax = 30 * time.Second
	}
}

func (rc *RecConn) setDefaultRecIntvlFactor() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.RecIntvlFactor == 0 {
		rc.RecIntvlFactor = 1.5
	}
}

func (rc *RecConn) setDefaultHandshakeTimeout() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.HandshakeTimeout == 0 {
		rc.HandshakeTimeout = 2 * time.Second
	}
}

func (rc *RecConn) setDefaultProxy() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.Proxy == nil {
		rc.Proxy = http.ProxyFromEnvironment
	}
}

func (rc *RecConn) setDefaultDialer(tlsClientConfig *tls.Config, handshakeTimeout time.Duration, compression bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.dialer = &websocket.Dialer{
		HandshakeTimeout:  handshakeTimeout,
		Proxy:             rc.Proxy,
		TLSClientConfig:   tlsClientConfig,
		EnableCompression: compression,
	}
}

func (rc *RecConn) getHandshakeTimeout() time.Duration {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.HandshakeTimeout
}

func (rc *RecConn) getTLSClientConfig() *tls.Config {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.TLSClientConfig
}

func (rc *RecConn) SetTLSClientConfig(tlsClientConfig *tls.Config) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.TLSClientConfig = tlsClientConfig
}

func (rc *RecConn) setDefaultLogger() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.Logger == nil {
		rc.Logger = DefaultLogger()
	}
}

// Dial creates a new client connection
func (rc *RecConn) Dial(urlStr string, reqHeader http.Header) {
	// Config setup
	rc.setDefaultLogger() // Set default logger first
	urlStr, err := rc.parseURL(urlStr)
	if err != nil {
		rc.Logger.Error("connection failed to dial", "error", err)
	}

	rc.setURL(urlStr)
	rc.setReqHeader(reqHeader)
	rc.setDefaultRecIntvlMin()
	rc.setDefaultRecIntvlMax()
	rc.setDefaultRecIntvlFactor()
	rc.setDefaultHandshakeTimeout()
	rc.setDefaultProxy()
	rc.setDefaultDialer(rc.getTLSClientConfig(), rc.getHandshakeTimeout(), rc.Compression)

	connected := make(chan struct{})

	// Connect
	rc.Logger.Info("starting connection")
	go func() {
		rc.connect()
		close(connected)
	}()

	// Wait for either connection success or handshake timeout
	select {
	case <-connected:
	case <-time.After(rc.getHandshakeTimeout()):
		rc.Logger.Info("handshake timeout reached")
	}
}

// GetURL returns current connection url
func (rc *RecConn) GetURL() string {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.url
}

func (rc *RecConn) getBackoff() *backoff.Backoff {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return &backoff.Backoff{
		Min:    rc.RecIntvlMin,
		Max:    rc.RecIntvlMax,
		Factor: rc.RecIntvlFactor,
		Jitter: true,
	}
}

func (rc *RecConn) hasSubscribeHandler() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.SubscribeHandler != nil
}

func (rc *RecConn) getKeepAliveTimeout() time.Duration {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.KeepAliveTimeout
}

// getKeepAlivePingInterval returns KeepAlivePingInterval, defaulted to a
// third of the timeout and clamped to the timeout.
func (rc *RecConn) getKeepAlivePingInterval() time.Duration {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	timeout, interval := rc.KeepAliveTimeout, rc.KeepAlivePingInterval
	if interval <= 0 {
		interval = timeout / defaultPingDivisor
	}
	if interval > timeout {
		interval = timeout
	}
	if interval <= 0 {
		interval = timeout
	}
	return interval
}

// SetPongHandler sets the handler for pong messages received from the peer.
// It shadows the embedded websocket.Conn method: keepalive needs to see every
// pong, so the caller's handler is wrapped rather than allowed to replace
// keepalive's, and it is kept across reconnects. Calling SetPongHandler on
// the embedded Conn directly still replaces keepalive's handler for that
// connection.
func (rc *RecConn) SetPongHandler(h func(appData string) error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.pongHandler = h
	if rc.Conn != nil && rc.KeepAliveTimeout != 0 {
		rc.Conn.SetPongHandler(rc.wrapPongHandlerLocked())
	} else if rc.Conn != nil {
		rc.Conn.SetPongHandler(h)
	}
}

// wrapPongHandlerLocked returns a pong handler that records the pong for
// keepalive and then runs the caller's handler, if any. rc.mu must be held.
func (rc *RecConn) wrapPongHandlerLocked() func(string) error {
	user := rc.pongHandler
	return func(appData string) error {
		rc.keepAliveResp.setLastResponse()
		if user != nil {
			return user(appData)
		}
		return nil
	}
}

// keepAlive pings the peer every ping interval and redials when nothing has
// come back for the full timeout.
//
// It used to ping once per timeout and then check the last pong against
// that same timeout. The pong it checked was the one for the previous ping,
// which landed one round trip after the previous tick, so the check compared
// "timeout + ticker jitter - RTT" against "timeout": on a fast link it
// tripped at random on a healthy connection.
//
// Pongs are only handled while the application is reading. A reader that
// stops reading, or blocks for longer than the timeout between reads, starves
// pong handling and the connection is redialed even if the peer is fine.
func (rc *RecConn) keepAlive() {
	timeout := rc.getKeepAliveTimeout()
	interval := rc.getKeepAlivePingInterval()

	// The connect time counts as the last response.
	rc.keepAliveResp.reset()

	rc.mu.Lock()
	if rc.done != nil {
		close(rc.done)
	}
	rc.done = make(chan struct{})
	done := rc.done // Store in local var to prevent race conditions
	conn := rc.Conn // ping this connection only, never its successor
	conn.SetPongHandler(rc.wrapPongHandlerLocked())
	rc.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			// WriteControl may be called concurrently with other writes,
			// so the ping does not wait behind a slow WriteMessage.
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(interval)); err != nil {
				rc.Logger.Error("ping error", "error", err)
			}

			select {
			case <-done:
				return
			case <-ticker.C:
			}

			if since := time.Since(rc.keepAliveResp.getLastResponse()); since > timeout {
				rc.Logger.Info("keepalive timeout reached, closing connection and reconnecting",
					"timeout", timeout, "interval", interval, "since_last_response", since)
				rc.CloseAndReconnect()
				return
			}
		}
	}()
}

func (rc *RecConn) connect() {
	b := rc.getBackoff()
	rand.Seed(time.Now().UTC().UnixNano())

	for {
		nextItvl := b.Duration()
		wsConn, httpResp, err := rc.dialer.Dial(rc.url, rc.dialHeader())

		rc.mu.Lock()
		rc.Conn = wsConn
		rc.dialErr = err
		rc.isConnected = err == nil
		rc.httpResp = httpResp
		rc.mu.Unlock()

		if err == nil {
			rc.Logger.Info("connection was successfully established", "url", rc.url)

			if rc.hasSubscribeHandler() {
				if err := rc.SubscribeHandler(); err != nil {
					rc.Logger.Error("SubscribeHandler failed", "error", err)
					rc.Close()
					rc.Logger.Info("connection will try again", "delay", nextItvl)
					time.Sleep(nextItvl)
					continue
				}
				rc.Logger.Info("SubscribeHandler was successfully established", "url", rc.url)
			}

			if rc.getKeepAliveTimeout() != 0 {
				rc.keepAlive()
			}
			return
		}

		nextItvl = rc.handshakeBackoff(err, httpResp, nextItvl)
		rc.Logger.Info("connection will try again", "delay", nextItvl)
		time.Sleep(nextItvl)
	}
}

// dialHeader is the request header for the next dial attempt: HeaderFunc's
// when one is set, otherwise the header given to Dial.
func (rc *RecConn) dialHeader() http.Header {
	if rc.HeaderFunc != nil {
		return rc.HeaderFunc()
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.reqHeader
}

// GetHTTPResponse returns the http response from the handshake.
// Useful when WebSocket handshake fails,
// so that callers can handle redirects, authentication, etc.
func (rc *RecConn) GetHTTPResponse() *http.Response {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.httpResp
}

// GetDialError returns the last dialer error.
// nil on successful connection.
func (rc *RecConn) GetDialError() error {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.dialErr
}

// IsConnected returns the WebSocket connection state
func (rc *RecConn) IsConnected() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.isConnected
}
