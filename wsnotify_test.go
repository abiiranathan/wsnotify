package wsnotify

import (
	"context"
	"fmt"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Test utilities
func newTestServer(t *testing.T) *Server {
	opts := ServerOptions{
		WriteWait:          100 * time.Millisecond,
		PongWait:           500 * time.Millisecond,
		PingPeriod:         400 * time.Millisecond,
		ReadLimit:          1024,
		SendBuffer:         10,
		DropOnBackpressure: true,
		CloseGrace:         50 * time.Millisecond,
		Logger:             log.New(&testLogger{t}, "test: ", 0),
	}
	return NewServer(opts)
}

type testLogger struct {
	t *testing.T
}

func (tl *testLogger) Write(p []byte) (n int, err error) {
	tl.t.Log(string(p))
	return len(p), nil
}

func newTestConnection(t *testing.T, server *Server) (*websocket.Conn, *httptest.Server) {
	httpServer := httptest.NewServer(server.WebsocketHandler())

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v\n", err)
	}

	return conn, httpServer
}

func TestServerCreation(t *testing.T) {
	server := newTestServer(t)

	if server == nil {
		t.Fatal("Server should not be nil")
	}

	if server.GetConnectedClients() != 0 {
		t.Errorf("Expected 0 connected clients, got %d", server.GetConnectedClients())
	}

	clients := server.GetClientList()
	if len(clients) != 0 {
		t.Errorf("Expected empty client list, got %v", clients)
	}
}

func TestServerDefaults(t *testing.T) {
	opts := ServerOptions{}
	opts.Defaults()

	if opts.WriteWait != 10*time.Second {
		t.Errorf("Expected WriteWait 10s, got %v", opts.WriteWait)
	}
	if opts.PongWait != 60*time.Second {
		t.Errorf("Expected PongWait 60s, got %v", opts.PongWait)
	}
	if opts.PingPeriod != 54*time.Second {
		t.Errorf("Expected PingPeriod 54s, got %v", opts.PingPeriod)
	}
	if opts.ReadLimit != 1<<20 {
		t.Errorf("Expected ReadLimit 1MB, got %d", opts.ReadLimit)
	}
	if opts.SendBuffer != 256 {
		t.Errorf("Expected SendBuffer 256, got %d", opts.SendBuffer)
	}
	if opts.BackpressureReason != "going away: backpressure" {
		t.Errorf("Expected default backpressure reason, got %s", opts.BackpressureReason)
	}
}

func TestClientConnection(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	// Give server time to register client
	time.Sleep(10 * time.Millisecond)

	if server.GetConnectedClients() != 1 {
		t.Errorf("Expected 1 connected client, got %d", server.GetConnectedClients())
	}

	clients := server.GetClientList()
	if len(clients) != 1 {
		t.Errorf("Expected 1 client in list, got %d", len(clients))
	}

	if !strings.HasPrefix(clients[0], "c_") {
		t.Errorf("Expected client ID to start with 'c_', got %s", clients[0])
	}
}

func TestChannelValidation(t *testing.T) {
	tests := []struct {
		channel Channel
		valid   bool
	}{
		{Channel("valid-channel"), true},
		{Channel(""), false},
		{Channel("test123"), true},
		{Channel("chat-room-1"), true},
	}

	for _, tt := range tests {
		if tt.channel.Valid() != tt.valid {
			t.Errorf("Channel(%q).Valid() = %v, want %v", tt.channel, !tt.valid, tt.valid)
		}
	}
}

func TestSubscriptionAndUnsubscription(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	// Get the client
	clients := server.GetClientList()
	if len(clients) != 1 {
		t.Fatalf("Expected 1 client, got %d", len(clients))
	}

	// Find the client object
	server.clientsMu.RLock()
	var client *Client
	for c := range server.clients {
		client = c
		break
	}
	server.clientsMu.RUnlock()

	if client == nil {
		t.Fatal("Could not find client")
	}

	channel := Channel("test-channel")

	// Test subscription
	err := server.Subscribe(client, channel)
	if err != nil {
		t.Errorf("Subscribe failed: %v", err)
	}

	// Verify subscription exists
	server.subsMu.RLock()
	subs, exists := server.subs[channel]
	server.subsMu.RUnlock()

	if !exists {
		t.Error("Channel subscription not found")
	}
	if _, subscribed := subs[client]; !subscribed {
		t.Error("Client not subscribed to channel")
	}

	// Test unsubscription
	server.Unsubscribe(client, channel)

	// Verify subscription removed
	server.subsMu.RLock()
	subs, exists = server.subs[channel]
	server.subsMu.RUnlock()

	if exists && len(subs) > 0 {
		t.Error("Client still subscribed after unsubscribe")
	}
}

func TestInvalidChannelSubscription(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	server.clientsMu.RLock()
	var client *Client
	for c := range server.clients {
		client = c
		break
	}
	server.clientsMu.RUnlock()

	// Test invalid channel
	err := server.Subscribe(client, Channel(""))
	if err == nil {
		t.Error("Expected error for invalid channel, got nil")
	}
}

func TestMessagePublishing(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	// Get client and subscribe to channel
	server.clientsMu.RLock()
	var client *Client
	for c := range server.clients {
		client = c
		break
	}
	server.clientsMu.RUnlock()

	channel := Channel("test-channel")
	err := server.Subscribe(client, channel)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	// Set up message receiver
	receivedCh := make(chan Payload, 1)
	go func() {
		for {
			var payload Payload
			err := conn.ReadJSON(&payload)
			if err != nil {
				return
			}
			receivedCh <- payload
		}
	}()

	// Publish message
	testMessage := map[string]string{"test": "message"}
	err = server.Publish(channel, testMessage)
	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}

	// Verify message received
	select {
	case payload := <-receivedCh:
		if payload.Channel != string(channel) {
			t.Errorf("Expected channel %s, got %s", channel, payload.Channel)
		}
		if payload.Type != MessageTypeJSON {
			t.Errorf("Expected type %s, got %s", MessageTypeJSON, payload.Type)
		}

		msgMap, ok := payload.Message.(map[string]any)
		if !ok {
			t.Errorf("Expected message to be map, got %T", payload.Message)
		} else if msgMap["test"] != "message" {
			t.Errorf("Expected test=message, got %v", msgMap)
		}

	case <-time.After(1 * time.Second):
		t.Error("Timeout waiting for message")
	}
}

func TestMessageTypes(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	server.clientsMu.RLock()
	var client *Client
	for c := range server.clients {
		client = c
		break
	}
	server.clientsMu.RUnlock()

	channel := Channel("test-channel")
	if err := server.Subscribe(client, channel); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	receivedCh := make(chan Payload, 3)
	go func() {
		for {
			var payload Payload
			err := conn.ReadJSON(&payload)
			if err != nil {
				return
			}
			receivedCh <- payload
		}
	}()

	// Test different message types
	tests := []struct {
		message any
		msgType MessageType
	}{
		{"text message", MessageTypeText},
		{map[string]string{"key": "value"}, MessageTypeJSON},
		{[]byte("binary data"), MessageTypeBinary},
	}

	for _, tt := range tests {
		err := server.PublishWithType(channel, tt.message, tt.msgType, "", "")
		if err != nil {
			t.Errorf("PublishWithType failed: %v", err)
		}

		select {
		case payload := <-receivedCh:
			if payload.Type != tt.msgType {
				t.Errorf("Expected type %s, got %s", tt.msgType, payload.Type)
			}
		case <-time.After(200 * time.Millisecond):
			t.Errorf("Timeout waiting for %s message", tt.msgType)
		}
	}
}

func TestMessageBatching(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	server.clientsMu.RLock()
	var client *Client
	for c := range server.clients {
		client = c
		break
	}
	server.clientsMu.RUnlock()

	channel := Channel("batch-channel")
	if err := server.Subscribe(client, channel); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	receivedCh := make(chan Payload, 1)
	go func() {
		for {
			var payload Payload
			err := conn.ReadJSON(&payload)
			if err != nil {
				return
			}
			receivedCh <- payload
		}
	}()

	// Send multiple messages that should batch
	ttl := 100 * time.Millisecond
	for i := range 3 {
		server.PublishWait(channel, map[string]int{"count": i}, 1, "test-batch", ttl)
	}

	// Wait for batch to flush
	select {
	case payload := <-receivedCh:
		batchMap, ok := payload.Message.(map[string]any)
		if !ok {
			t.Fatalf("Expected batched message to be map, got %T", payload.Message)
		}

		if batchMap["name"] != "test-batch" {
			t.Errorf("Expected batch name 'test-batch', got %v", batchMap["name"])
		}

		messages, ok := batchMap["messages"].([]any)
		if !ok {
			t.Errorf("Expected messages array, got %T", batchMap["messages"])
		}

		if len(messages) != 3 {
			t.Errorf("Expected 3 batched messages, got %d", len(messages))
		}

	case <-time.After(200 * time.Millisecond):
		t.Error("Timeout waiting for batched message")
	}
}

func TestMessageBatchingDeduplication(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	server.clientsMu.RLock()
	var client *Client
	for c := range server.clients {
		client = c
		break
	}
	server.clientsMu.RUnlock()

	channel := Channel("dedup-channel")
	if err := server.Subscribe(client, channel); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	receivedCh := make(chan Payload, 1)
	go func() {
		for {
			var payload Payload
			err := conn.ReadJSON(&payload)
			if err != nil {
				return
			}
			receivedCh <- payload
		}
	}()

	// Send duplicate messages
	ttl := 100 * time.Millisecond
	duplicateMsg := map[string]string{"key": "duplicate"}

	for range 5 {
		server.PublishWait(channel, duplicateMsg, 1, "dedup-test", ttl)
	}

	// Wait for batch
	select {
	case payload := <-receivedCh:
		batchMap := payload.Message.(map[string]any)
		messages := batchMap["messages"].([]any)

		// Should only have 1 unique message despite 5 identical sends
		if len(messages) != 1 {
			t.Errorf("Expected 1 deduplicated message, got %d", len(messages))
		}

	case <-time.After(200 * time.Millisecond):
		t.Error("Timeout waiting for deduplicated batch")
	}
}

func TestStringBatching(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	// Use defer for cleanup
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	// Wait a moment for the server to register the new client.
	time.Sleep(20 * time.Millisecond)

	var client *Client
	server.clientsMu.RLock()
	for c := range server.clients {
		client = c
		break
	}
	server.clientsMu.RUnlock()
	if client == nil {
		t.Fatal("Test client was not registered by the server")
	}

	channel := Channel("string-channel")
	if err := server.Subscribe(client, channel); err != nil {
		t.Fatal(err)
	}

	receivedCh := make(chan Payload, 1)
	go func() {
		// This goroutine must exit cleanly when the connection closes.
		defer close(receivedCh)

		for {
			var payload Payload
			err := conn.ReadJSON(&payload)
			if err != nil {
				// Check for a normal close; this is expected when the test ends.
				if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					t.Errorf("unexpected websocket close error: %v", err)
				}
				// Any error (including a clean close) means we stop reading.
				return
			}
			receivedCh <- payload
		}
	}()

	ttl := 100 * time.Millisecond // Use a shorter TTL for faster tests
	messages := []string{"line 1", "line 2", "line 3"}

	for _, msg := range messages {
		server.PublishWaitString(channel, msg, 1, "String Batch", ttl)
	}

	// The timeout must be longer than the TTL to avoid a race condition.
	timeout := ttl + 50*time.Millisecond

	select {
	case payload, ok := <-receivedCh:
		// Check if the channel was closed by the reader goroutine on error.
		if !ok {
			t.Fatal("receive channel was closed unexpectedly")
		}

		if payload.Type != MessageTypeText {
			t.Errorf("Expected MessageTypeText, got %s", payload.Type)
		}

		text, ok := payload.Message.(string)
		if !ok {
			t.Fatalf("Expected string message, got %T", payload.Message)
		}

		// It's possible for the lines to be in a different order if the test runs fast enough
		// for `add` calls to interleave. A more robust check would not depend on order.
		// But for this case, checking for contains is good enough.
		expectedPrefix := "String Batch\n\n"
		if !strings.HasPrefix(text, expectedPrefix) {
			t.Errorf("Expected prefix %q, got %q", expectedPrefix, text)
		}
		for _, line := range messages {
			if !strings.Contains(text, line) {
				t.Errorf("Expected batched message to contain %q, but it did not. Got: %s", line, text)
			}
		}

	case <-time.After(timeout):
		t.Error("Timeout waiting for string batch")
	}
}

func TestCancelPendingMessages(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	channel := Channel("cancel-channel")

	// Add pending messages
	server.PublishWait(channel, "msg1", 1, "test", 1*time.Second)
	server.PublishWait(channel, "msg2", 1, "test", 1*time.Second)

	// Verify pending exists
	server.pendingMu.Lock()
	key := server.makeKey(channel, 1)
	_, exists := server.pending[key]
	server.pendingMu.Unlock()

	if !exists {
		t.Error("Expected pending message to exist")
	}

	// Cancel
	server.CancelPendingMessages(channel, 1)

	// Verify pending removed
	server.pendingMu.Lock()
	_, exists = server.pending[key]
	server.pendingMu.Unlock()

	if exists {
		t.Error("Expected pending message to be cancelled")
	}
}

func TestMessageHandler(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	var receivedMessages []IncomingMessage
	var mu sync.Mutex

	server.SetMessageHandler(func(client *Client, message IncomingMessage) {
		mu.Lock()
		receivedMessages = append(receivedMessages, message)
		mu.Unlock()
	})

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	// Send JSON message from client
	outgoing := IncomingMessage{
		Type:      MessageTypeJSON,
		Channel:   "test-channel",
		Message:   map[string]string{"hello": "world"},
		MessageID: "test-123",
	}

	err := conn.WriteJSON(outgoing)
	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Wait for handler
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(receivedMessages) != 1 {
		t.Errorf("Expected 1 received message, got %d", len(receivedMessages))
	}

	if len(receivedMessages) > 0 {
		msg := receivedMessages[0]
		if msg.Type != MessageTypeJSON {
			t.Errorf("Expected JSON type, got %s", msg.Type)
		}
		if msg.Channel != "test-channel" {
			t.Errorf("Expected test-channel, got %s", msg.Channel)
		}
		if msg.MessageID != "test-123" {
			t.Errorf("Expected message ID test-123, got %s", msg.MessageID)
		}
		if !strings.HasPrefix(msg.ClientID, "c_") {
			t.Errorf("Expected client ID to be set, got %s", msg.ClientID)
		}
	}
}

func TestClientMetadata(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	server.clientsMu.RLock()
	var client *Client
	for c := range server.clients {
		client = c
		break
	}
	server.clientsMu.RUnlock()

	// Test setting and getting metadata
	client.SetMetadata("user_id", "alice")
	client.SetMetadata("role", "admin")

	userID, exists := client.GetMetadata("user_id")
	if !exists {
		t.Error("Expected user_id metadata to exist")
	}
	if userID != "alice" {
		t.Errorf("Expected user_id=alice, got %v", userID)
	}

	role, exists := client.GetMetadata("role")
	if !exists {
		t.Error("Expected role metadata to exist")
	}
	if role != "admin" {
		t.Errorf("Expected role=admin, got %v", role)
	}

	// Test non-existent key
	_, exists = client.GetMetadata("nonexistent")
	if exists {
		t.Error("Expected nonexistent key to not exist")
	}
}

func TestPublishToClient(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	// Create two connections
	conn1, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn1.Close(); err != nil {
			t.Errorf("close error: %v", err)
		}
	}()

	conn2, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := conn2.Close(); err != nil {
			t.Errorf("close error: %v", err)
		}
	}()

	time.Sleep(20 * time.Millisecond)

	if server.GetConnectedClients() != 2 {
		t.Fatalf("Expected 2 clients, got %d", server.GetConnectedClients())
	}

	// Get clients
	clients := make([]*Client, 0, 2)
	server.clientsMu.RLock()
	for c := range server.clients {
		clients = append(clients, c)
	}
	server.clientsMu.RUnlock()

	targetClient := clients[0]
	channel := Channel("direct-channel")

	// Set up receiver on first connection
	receivedCh := make(chan Payload, 1)
	go func() {
		for {
			var payload Payload
			err := conn1.ReadJSON(&payload)
			if err != nil {
				return
			}
			receivedCh <- payload
		}
	}()

	// Send message to specific client
	err = server.PublishToClient(targetClient, channel, "direct message",
		MessageTypeText, "msg-direct", "")
	if err != nil {
		t.Fatalf("PublishToClient failed: %v", err)
	}

	// Verify only target client receives message
	select {
	case payload := <-receivedCh:
		if payload.Message != "direct message" {
			t.Errorf("Expected 'direct message', got %v", payload.Message)
		}
		if payload.MessageID != "msg-direct" {
			t.Errorf("Expected message ID 'msg-direct', got %s", payload.MessageID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Timeout waiting for direct message")
	}
}

func TestBackpressureHandling(t *testing.T) {
	// Create server with small buffer and backpressure enabled
	opts := ServerOptions{
		SendBuffer:         2, // Very small buffer
		DropOnBackpressure: true,
		BackpressureReason: "test backpressure",
		WriteWait:          10 * time.Millisecond,
		CloseGrace:         10 * time.Millisecond,
	}
	opts.Defaults()

	server := NewServer(opts)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()

	time.Sleep(10 * time.Millisecond)

	server.clientsMu.RLock()
	var client *Client
	for c := range server.clients {
		client = c
		break
	}
	server.clientsMu.RUnlock()

	channel := Channel("backpressure-channel")
	if err := server.Subscribe(client, channel); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	// Fill up the buffer by sending many messages quickly
	// Don't read from connection to simulate slow client
	for i := range 10 {
		if err := server.Publish(channel, fmt.Sprintf("message %d", i)); err != nil {
			t.Fatalf("failed to publish message: %v\n", err)
		}
	}

	// Give time for backpressure to kick in
	time.Sleep(100 * time.Millisecond)

	// Client should be disconnected due to backpressure
	if server.GetConnectedClients() != 0 {
		t.Errorf("Expected client to be disconnected due to backpressure")
	}

	// Connection should be closed
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Error("Expected connection to be closed due to backpressure")
	}
}

func TestGracefulShutdown(t *testing.T) {
	server := newTestServer(t)

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()

	time.Sleep(10 * time.Millisecond)

	if server.GetConnectedClients() != 1 {
		t.Fatalf("Expected 1 client, got %d", server.GetConnectedClients())
	}

	// Shutdown server
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := server.Shutdown(ctx)
	if err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}

	// Connection should be closed
	_, _, readErr := conn.ReadMessage()
	if readErr == nil {
		t.Error("Expected connection to be closed after shutdown")
	}

	// Subsequent connections should be rejected
	_, _, dialErr := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws", nil)
	if dialErr == nil {
		t.Error("Expected new connections to be rejected after shutdown")
	}
}

func TestConcurrentOperations(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	const numClients = 10
	const numMessages = 50

	var wg sync.WaitGroup
	var connectedClients atomic.Int32

	// Create multiple clients concurrently
	for i := range numClients {
		wg.Add(1)
		go func(clientNum int) {
			defer wg.Done()

			conn, httpServer := newTestConnection(t, server)
			defer httpServer.Close()
			defer func() {
				if err := conn.Close(); err != nil {
					t.Errorf("close with err: %v", err)
				}
			}()

			connectedClients.Add(1)
			defer connectedClients.Add(-1)

			// Send messages concurrently
			channel := Channel(fmt.Sprintf("test-channel-%d", clientNum%3))
			for j := 0; j < numMessages; j++ {
				message := map[string]any{
					"client": clientNum,
					"msg":    j,
				}
				server.PublishWait(channel, message, uint(clientNum),
					"concurrent-test", 50*time.Millisecond)
			}

			time.Sleep(100 * time.Millisecond) // Keep connection alive
		}(i)
	}

	// Wait for all clients to connect
	time.Sleep(100 * time.Millisecond)

	if int(connectedClients.Load()) != numClients {
		t.Errorf("Expected %d connected clients, got %d",
			numClients, connectedClients.Load())
	}

	wg.Wait()

	// Give time for cleanup
	time.Sleep(50 * time.Millisecond)

	if server.GetConnectedClients() != 0 {
		t.Errorf("Expected 0 clients after test, got %d",
			server.GetConnectedClients())
	}
}

func TestInvalidMessages(t *testing.T) {
	server := newTestServer(t)
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("error shutting down server")
		}
	}()

	var errorReceived atomic.Bool
	server.SetMessageHandler(func(client *Client, message IncomingMessage) {
		// Should not be called for invalid JSON
		t.Error("Handler should not be called for invalid JSON")
	})

	conn, httpServer := newTestConnection(t, server)
	defer httpServer.Close()
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close with err: %v", err)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	// Set up error message receiver
	go func() {
		for {
			var errorMsg map[string]any
			err := conn.ReadJSON(&errorMsg)
			if err != nil {
				return
			}
			if errorMsg["type"] == "error" {
				errorReceived.Store(true)
				break
			}
		}
	}()

	// Send invalid JSON
	err := conn.WriteMessage(websocket.TextMessage, []byte("{invalid json"))
	if err != nil {
		t.Fatalf("Failed to send invalid JSON: %v", err)
	}

	// Wait for error response
	time.Sleep(100 * time.Millisecond)

	if !errorReceived.Load() {
		t.Error("Expected to receive error message for invalid JSON")
	}
}
