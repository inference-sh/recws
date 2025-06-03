package examples

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/inference-sh/recws"
)

// Message represents a typed websocket message
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// ChatMessage represents a chat message payload
type ChatMessage struct {
	Text string `json:"text"`
}

func ExampleAdvanced() {
	// Create a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a custom slog logger
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Setup WebSocket connection with options
	ws := recws.RecConn{
		HandshakeTimeout: 10 * time.Second,
		RecIntvlMin:      2 * time.Second,
		RecIntvlMax:      30 * time.Second,
		RecIntvlFactor:   1.5,
		KeepAliveTimeout: 60 * time.Second,
		Logger:           logger, // slog.Logger implements our Logger interface
	}

	// Setup headers if needed
	headers := http.Header{}
	headers.Add("User-Agent", "RecWS-Client")

	// Connect to the WebSocket server
	ws.Dial("wss://echo.websocket.org", headers)

	// Handle OS signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start a goroutine to handle incoming messages
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if !ws.IsConnected() {
					logger.Warn("WebSocket disconnected, waiting for reconnection...")
					time.Sleep(time.Second)
					continue
				}

				var msg Message
				if err := ws.ReadJSON(&msg); err != nil {
					logger.Error("Error reading message", "error", err)
					continue
				}

				// Handle different message types
				switch msg.Type {
				case "chat":
					var chatMsg ChatMessage
					if err := json.Unmarshal(msg.Data, &chatMsg); err != nil {
						logger.Error("Error unmarshaling chat message", "error", err)
						continue
					}
					logger.Info("Received chat message", "text", chatMsg.Text)
				default:
					logger.Debug("Received unknown message type", "type", msg.Type)
				}
			}
		}
	}()

	// Start a goroutine to send periodic messages
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !ws.IsConnected() {
					continue
				}

				chatMsg := Message{
					Type: "chat",
					Data: json.RawMessage(`{"text":"Hello, Server!"}`),
				}

				if err := ws.WriteJSON(chatMsg); err != nil {
					logger.Error("Error sending message", "error", err)
				} else {
					logger.Debug("Sent chat message successfully")
				}
			}
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	logger.Info("Shutting down...")

	// Cleanup
	ticker.Stop()
	cancel()
	ws.Close()
	logger.Info("Shutdown complete")
}
