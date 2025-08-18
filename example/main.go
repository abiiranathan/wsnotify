package main

import (
	"context"
	_ "embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abiiranathan/wsnotify"
)

//go:embed index.html
var indexHTML []byte

func main() {
	// Create a new wsnotify server
	opts := wsnotify.ServerOptions{}
	opts.Defaults()
	server := wsnotify.NewServer(opts)

	server.SetDisconnectHandler(func(client *wsnotify.Client) {
		log.Printf("Client disconnected: %v\n", client.ID)
	})

	// Define a handler for incoming messages from clients
	server.SetMessageHandler(func(client *wsnotify.Client, msg wsnotify.IncomingMessage) {
		log.Printf("Received message from %s: type=%s, channel=%s", client.ID, msg.Type, msg.Channel)

		// We expect JSON messages for our chat protocol
		if msg.Type != wsnotify.MessageTypeJSON {
			return
		}

		data, ok := msg.Message.(map[string]any)
		if !ok {
			return // Ignore malformed messages
		}

		action, _ := data["action"].(string)
		switch action {
		case "subscribe":
			if ch, ok := data["channel"].(string); ok {
				channel := wsnotify.Channel(ch)
				if err := server.Subscribe(client, channel); err != nil {
					log.Printf("Subscription failed for %s to %s: %v", client.ID, ch, err)
				} else {
					log.Printf("Client %s subscribed to %s", client.ID, ch)
					// Confirm subscription to the client
					err = server.PublishToClient(client, channel,
						map[string]string{"status": "subscribed", "channel": ch},
						wsnotify.MessageTypeJSON, "", msg.MessageID)
					if err != nil {
						log.Println(err)
					}
				}
			}
		case "broadcast":
			// The client specifies the channel in the message envelope
			if msg.Channel != "" {
				// Broadcast the message to all subscribers of the channel
				err := server.Publish(wsnotify.Channel(msg.Channel), data)
				if err != nil {
					log.Println(err)
				}
			}
		}
	})

	// Set up HTTP routes
	mux := http.NewServeMux()
	mux.Handle("/ws", server.WebsocketHandler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, err := w.Write(indexHTML)
		if err != nil {
			log.Println(err)
		}
	})

	// Start the server and implement graceful shutdown
	httpServer := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		log.Println("WebSocket server starting on :8080")
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Wait for interrupt signal for graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("wsnotify server shutdown error: %v", err)
	}

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
	log.Println("Server gracefully stopped")
}
