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
	// KeepAliveTimeout is an interval for sending ping/pong messages
	// disabled if 0
	KeepAliveTimeout time.Duration
	// Compression enables per-message compression as defined in https://datatracker.ietf.org/doc/html/rfc7692
	Compression bool
	// Logger is the logger instance to use. If nil, DefaultLogger() will be used.
	Logger Logger

	isConnected bool
	mu          sync.RWMutex
	url         string
	reqHeader   http.Header
	httpResp    *http.Response
	dialErr     error
	dialer      *websocket.Dialer
	done        chan struct{} // Channel to signal goroutine shutdown

	*websocket.Conn
}

// CloseAndReconnect will try to reconnect.
func (rc *RecConn) CloseAndReconnect() {
	rc.Logger.Info("closing connection and reconnecting")
	rc.Close()
	go rc.connect()
}

// setIsConnected sets state for isConnected
func (rc *RecConn) setIsConnected(state bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.isConnected = state
}

// Close closes the underlying network connection without
// sending or waiting for a close frame.
func (rc *RecConn) Close() {
	rc.mu.Lock()
	if rc.done != nil {
		close(rc.done)
		rc.done = nil
	}
	if rc.Conn != nil {
		rc.Conn.Close()
	}
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
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return messageType, message, nil
		}
		if err != nil {
			rc.Logger.Error("read message error", "error", err)
			rc.Logger.Info("closing connection and reconnecting")
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
			rc.Logger.Info("closing connection and reconnecting")
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
			rc.Logger.Info("closing connection and reconnecting")
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
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil {
			rc.Logger.Error("read json error", "error", err)
			rc.Logger.Info("closing connection and reconnecting")
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
		rc.Logger.Info("connection established")
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

func (rc *RecConn) writeControlPingMessage() error {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	return rc.Conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second))
}

func (rc *RecConn) keepAlive() {
	var (
		keepAliveResponse = new(keepAliveResponse)
		ticker            = time.NewTicker(rc.getKeepAliveTimeout())
	)

	// Reset the keep-alive state
	keepAliveResponse.reset()

	rc.mu.Lock()
	if rc.done != nil {
		close(rc.done)
	}
	rc.done = make(chan struct{})
	done := rc.done // Store in local var to prevent race conditions
	rc.Conn.SetPongHandler(func(msg string) error {
		keepAliveResponse.setLastResponse()
		return nil
	})
	rc.mu.Unlock()

	go func() {
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			default:
				if !rc.IsConnected() {
					continue
				}

				if err := rc.writeControlPingMessage(); err != nil {
					rc.Logger.Error("ping error", "error", err)
				}

				select {
				case <-done:
					return
				case <-ticker.C:
					if time.Since(keepAliveResponse.getLastResponse()) > rc.getKeepAliveTimeout() {
						rc.Logger.Info("keepalive timeout reached, closing connection and reconnecting")
						rc.Logger.Info("keepalive timeout", "timeout", rc.getKeepAliveTimeout())
						rc.Logger.Info("time since last response", "duration", time.Since(keepAliveResponse.getLastResponse()))
						rc.CloseAndReconnect()
						return
					}
				}
			}
		}
	}()
}

func (rc *RecConn) connect() {
	b := rc.getBackoff()
	rand.Seed(time.Now().UTC().UnixNano())

	for {
		nextItvl := b.Duration()
		wsConn, httpResp, err := rc.dialer.Dial(rc.url, rc.reqHeader)

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
					rc.Logger.Error("connect handler failed", "error", err)
				}
				rc.Logger.Info("connect handler was successfully established", "url", rc.url)
			}

			if rc.getKeepAliveTimeout() != 0 {
				rc.keepAlive()
			}
			return
		}

		rc.Logger.Error("connection error", "error", err)
		rc.Logger.Info("connection will try again", "delay", nextItvl)
		time.Sleep(nextItvl)
	}
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
