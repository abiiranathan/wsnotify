# wsnotify: A Production-Ready WebSocket Notification Service for Go

A robust, production-focused WebSocket messaging service for Go applications. It provides channel-based subscriptions, efficient message batching, backpressure control, and graceful shutdown, all without global state.

## Features

#### Core Functionality
- **Real-Time Messaging**: Bidirectional communication over WebSockets.
- **Channel-Based Pub/Sub**: Route messages efficiently to interested clients.
- **Flexible Message Types**: Natively supports JSON, Text, and Binary payloads.
- **Client Metadata**: Attach session data (like user IDs) to each connection.

#### Production Readiness
- **Efficient Message Batching**: Group high-frequency updates into single messages with configurable TTLs.
- **Backpressure Control**: Automatically disconnects or blocks slow clients to maintain server health.
- **Graceful Shutdown**: Ensures all connections are closed cleanly on server termination.
- **Configurable & Hardened**: Tune timeouts, buffer sizes, and message limits for your workload.
- **No Globals**: Designed for clean integration, testing, and running multiple instances.

#### Developer Experience
- **Simple API**: Easy to integrate with standard `net/http`.
- **Thread-Safe**: All operations are safe for concurrent use.
- **Secure by Default**: Sensible defaults, including same-origin policy for WebSocket upgrades.

## Installation

```bash
go get github.com/abiiranathan/wsnotify
```

## Quick Start: A Complete Chat Application

This example creates a fully functional web server that serves a chat client and handles WebSocket connections.

#### 1. Server Code (`main.go`)

Create a file `main.go` with the following content. This server will:
1.  Serve an `index.html` file at the root (`/`).
2.  Handle WebSocket connections at `/ws`.
3.  Process incoming messages to subscribe clients to channels and broadcast chat messages.

See the code at [Example(main.go)](./example/main.go)

#### 2. Run the Application
Save the code and run it:
```bash
cd example
go run main.go
```
Open your browser to `http://localhost:8080`. You can open multiple tabs to simulate different users.

## API Guide & Core Concepts

### Server Initialization

The server is configured via `ServerOptions`. Always call `Defaults()` to apply sensible production values first.

```go
opts := wsnotify.ServerOptions{
    // WebSocket I/O and keepalives
    WriteWait:   10 * time.Second,  // Max time to write a message to a client.
    PongWait:    60 * time.Second,  // Max time to wait for a pong response.
    PingPeriod:  54 * time.Second,  // Frequency of pings (must be < PongWait).
    ReadLimit:   1 << 20,           // 1MB max message size from a client.
    Compression: true,              // Enable per-message deflate compression.

    // Buffers and backpressure control
    SendBuffer:         256,                          // Outbound message queue size per client.
    DropOnBackpressure: true,                        // Disconnect slow clients if their queue is full.
    BackpressureReason: "going away: backpressure",  // Close reason sent to slow clients.
    CloseGrace:         200 * time.Millisecond,      // Time to allow the close frame to be sent.

    // Logging and Security
    Logger: log.New(os.Stdout, "ws: ", log.LstdFlags),
    CheckOrigin: func(r *http.Request) bool {
        // Allow connections from a specific origin
        // return r.Header.Get("Origin") == "https://myapp.com"
        return true // For development, allow all origins
    },
}
server := wsnotify.NewServer(opts)
```

### Publishing Messages

#### Broadcast to a Channel
Send a message to all clients subscribed to a channel. This is the most common use case.

```go
channel := wsnotify.Channel("news-updates")
message := map[string]string{"title": "New Feature!", "body": "Batching is now 2x faster."}

// Publish as a standard JSON message
err := server.Publish(channel, message)

// Publish with a specific type and IDs for tracking/replies
err = server.PublishWithType(channel, "Hello Text World", wsnotify.MessageTypeText, "msg-123", "reply-to-456")
```

#### Send to a Specific Client
Send a message directly to a single client, often used for replies or private notifications.

```go
// The `client` object is available in the MessageHandler
server.PublishToClient(client, channel, 
    map[string]string{"status": "your request was processed"}, 
    wsnotify.MessageTypeJSON, "", "client-msg-id-789")
```

### Batched Publishing

Batching is crucial for high-frequency events, like live cursors, real-time metrics, or game state updates. It groups multiple messages for a given key (`channel` + `ident`) into a single WebSocket frame.

- `channel`: The target channel for the final broadcast.
- `ident`: A unique identifier to group messages. For example, a document ID, a user ID, or a sensor ID.
- `name`: A descriptive name for the final batched payload.
- `ttl`: The time window during which messages are collected.

#### Example 1: Batching JSON Objects (e.g., Live Collaboration)
Imagine multiple users are editing a document. Instead of sending every keystroke, you can batch updates.

```go
docID := uint(12345)
channel := wsnotify.Channel("document-editors")

// User A moves their cursor
server.PublishWait(channel, 
    map[string]any{"user": "A", "x": 10, "y": 45},
    docID, "cursor-updates", 100*time.Millisecond)

// User B types a character
server.PublishWait(channel,
    map[string]any{"user": "B", "insert": "h", "pos": 82},
    docID, "cursor-updates", 100*time.Millisecond)

// After 100ms, ONE message is sent to the channel:
// {
//   "channel": "document-editors",
//   "message": {
//     "name": "cursor-updates",
//     "messages": [
//       { "user": "A", "x": 10, "y": 45 },
//       { "user": "B", "insert": "h", "pos": 82 }
//     ]
//   }, ...
// }
```

#### Example 2: Batching Strings (e.g., Log Aggregation)
`PublishWaitString` joins messages with newlines, useful for simple text aggregation.

```go
serverID := uint(1)
logChannel := wsnotify.Channel("server-logs")

server.PublishWaitString(logChannel, "INFO: User logged in", serverID, "Server-1 Logs", 30*time.Second)
server.PublishWaitString(logChannel, "WARN: High CPU usage detected", serverID, "Server-1 Logs", 30*time.Second)

// After 30s, one text message is sent:
// "Server-1 Logs\n\nINFO: User logged in\nWARN: High CPU usage detected"
```

#### Canceling a Batch
If the event you're batching for is no longer relevant (e.g., the user closes the document), you can cancel the pending flush.
```go
server.CancelPendingMessages(channel, docID)
```

### Client Management

#### Subscriptions
Manage a client's channel subscriptions. This is typically done inside the `MessageHandler` in response to a client's request.

```go
// Subscribe
err := server.Subscribe(client, wsnotify.Channel("chat-room-1"))

// Unsubscribe
server.Unsubscribe(client, wsnotify.Channel("chat-room-1"))
```

#### Client Metadata
Store arbitrary session data on a client connection. This is perfect for storing authenticated user information.

```go
// Inside your message handler, after authenticating the client:
client.SetMetadata("userID", 123)
client.SetMetadata("username", "alice")

// Later, you can retrieve it:
if userID, ok := client.GetMetadata("userID"); ok {
    log.Printf("Action performed by user: %d", userID)
}
```

### Server Monitoring

Get insight into the server's state at runtime.

```go
// Get the number of currently connected clients
count := server.GetConnectedClients()
log.Printf("Active connections: %d", count)

// Get a list of all client IDs
clientIDs := server.GetClientList()
log.Printf("Connected clients: %v", clientIDs)
```

### Message Format

#### Outgoing Payload (Server -> Client)

```json
{
    "channel": "chat-room-1",
    "type": "json",
    "time": "2025-08-18T10:30:00Z",
    "message": {
        "user": "alice",
        "text": "Hello!"
    },
    "message_id": "server-msg-123",
    "reply_to_id": "client-msg-456"
}
```

#### Incoming Message (Client -> Server)

Your client should send JSON messages in this format. The `channel` field is optional if you handle subscriptions via the message payload but required if you want to broadcast directly.

```json
{
    "type": "json",
    "channel": "chat-room-1",
    "message": {
        "action": "broadcast",
        "text": "Hello everyone!"
    },
    "message_id": "client-msg-456"
}
```

## Dependencies

- `github.com/gorilla/websocket`

## License

MIT
