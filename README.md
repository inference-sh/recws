<img width="150" src="https://raw.githubusercontent.com/recws-org/recws/master/recws-logo.png" alt="logo">

# recws

Reconnecting WebSocket is a websocket client based on [gorilla/websocket](https://github.com/gorilla/websocket) that will automatically reconnect if the connection is dropped - thread safe!

[![GoDoc](https://godoc.org/github.com/inference-sh/recws?status.svg)](https://godoc.org/github.com/inference-sh/recws)
[![Go Report Card](https://goreportcard.com/badge/github.com/inference-sh/recws)](https://goreportcard.com/report/github.com/inference-sh/recws)
[![GitHub license](https://img.shields.io/github/license/Naereen/StrapDown.js.svg)](https://github.com/Naereen/StrapDown.js/blob/master/LICENSE)

## Features

- Automatic reconnection with configurable backoff
- Thread-safe operations
- Structured logging with configurable levels
- Robust keepalive mechanism
- Proper connection establishment with channel-based synchronization
- TLS support
- Proxy support
- Compression support (RFC 7692)
- Graceful shutdown support

## Installation

```bash
go get github.com/inference-sh/recws
```

## Quick Start

```go
import "github.com/inference-sh/recws"

// Create a new reconnecting websocket
ws := &recws.RecConn{
    KeepAliveTimeout: 30 * time.Second,
    LogLevel: recws.LogLevelInfo,  // Debug, Info, Warn, Error levels available
}

// Connect (blocks until connection is established or timeout reached)
ws.Dial("ws://example.com/ws", nil)

// Send/receive messages
ws.WriteMessage(websocket.TextMessage, []byte("hello"))
```

## Credits

This project is a fork of [recws-org/recws](https://github.com/recws-org/recws) with significant improvements.

### Logo Credits
- Logo by [Anastasia Marx](https://www.behance.net/AnastasiaMarx)
- Gopher by [Gophers](https://github.com/egonelbre/gophers)

## License

recws is open-source software licensed under the [MIT license](https://opensource.org/licenses/MIT).

