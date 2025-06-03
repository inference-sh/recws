<img width="150" src="https://raw.githubusercontent.com/inference-sh/recws/master/recws-logo.png" alt="logo">

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
}

// Connect (blocks until connection is established or timeout reached)
ws.Dial("ws://example.com/ws", nil)

// Send/receive messages
ws.WriteMessage(websocket.TextMessage, []byte("hello"))
```

## Examples

See the [examples directory](examples/) for complete working examples.


## Important Note

This library is designed to be used as a WebSocket client (the connecting end) that initiates connections to a WebSocket server. It is not meant to be used for implementing WebSocket server endpoints. If you're looking to implement a WebSocket server, please use the `gorilla/websocket` package directly instead.

## Examples

See the [examples directory](examples/) for complete working examples.

## Important Note

This library is designed to be used as a WebSocket client (the connecting end) that initiates connections to a WebSocket server. It is not meant to be used for implementing WebSocket server endpoints. If you're looking to implement a WebSocket server, please use the `gorilla/websocket` package directly instead.


### Logo Credits
- Logo by [Anastasia Marx](https://www.behance.net/AnastasiaMarx)
- Gopher by [Gophers](https://github.com/egonelbre/gophers)

## License

recws is open-source software licensed under the [MIT license](https://opensource.org/licenses/MIT).