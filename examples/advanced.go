package examples

import (
	"context"
	"encoding/json"
	"log"
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

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Setup WebSocket connection with options
	ws := recws.RecConn{
		HandshakeTimeout: 10 * time.Second,
		RecIntvlMin:      2 * time.Second,
		RecIntvlMax:      30 * time.Second,
		RecIntvlFactor:   1.5,
		KeepAliveTimeout: 60 * time.Second,
		Logger:           logger,
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
					log.Printf("WebSocket disconnected, waiting for reconnection...")
					time.Sleep(time.Second)
					continue
				}

				var msg Message
				if err := ws.ReadJSON(&msg); err != nil {
					log.Printf("Error reading message: %v", err)
					continue
				}

				// Handle different message types
				switch msg.Type {
				case "chat":
					var chatMsg ChatMessage
					if err := json.Unmarshal(msg.Data, &chatMsg); err != nil {
						log.Printf("Error unmarshaling chat message: %v", err)
						continue
					}
					log.Printf("Received chat message: %s", chatMsg.Text)
				default:
					log.Printf("Received unknown message type: %s", msg.Type)
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
					log.Printf("Error sending message: %v", err)
				} else {
					log.Printf("Sent chat message successfully")
				}
			}
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	log.Println("Shutting down...")

	// Cleanup
	ticker.Stop()
	cancel()
	ws.Close()
	log.Println("Shutdown complete")
}
