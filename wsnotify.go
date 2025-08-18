// Package wsnotify provides a WebSocket-based real-time messaging service,
// supporting both immediate and batched message broadcasting to connected clients.
// Production-focused: no globals, per-channel subscriptions, backpressure control,
// graceful shutdown, configurable timings/limits, and safe batching.
package wsnotify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type MessageType string

const (
	MessageTypeText   MessageType = "text"
	MessageTypeBinary MessageType = "binary"
	MessageTypeJSON   MessageType = "json"
	MessageTypePing   MessageType = "ping"
	MessageTypePong   MessageType = "pong"
)

type Payload struct {
	Channel   string      `json:"channel"`
	Type      MessageType `json:"type"`
	Time      time.Time   `json:"time"`
	Message   any         `json:"message"`
	MessageID string      `json:"message_id,omitempty"`
	ReplyToID string      `json:"reply_to_id,omitempty"`
}

type IncomingMessage struct {
	Type      MessageType `json:"type"`
	Channel   string      `json:"channel,omitempty"`
	Message   any         `json:"message"`
	MessageID string      `json:"message_id,omitempty"`
	ReplyToID string      `json:"reply_to_id,omitempty"`
	ClientID  string      `json:"client_id,omitempty"`
}

type Channel string

func (c Channel) Valid() bool {
	return string(c) != ""
}

type MessageHandler func(client *Client, message IncomingMessage)

// ServerOptions configures server behavior.
type ServerOptions struct {
	// WebSocket I/O and keepalives
	WriteWait   time.Duration // write deadline per message
	PongWait    time.Duration // allowed time to read the next pong
	PingPeriod  time.Duration // frequency of pings (should be < PongWait)
	ReadLimit   int64         // maximum message size in bytes
	Compression bool          // enable permessage-deflate

	// Buffers and backpressure
	SendBuffer         int           // per-client outbound queue length
	DropOnBackpressure bool          // disconnect slow clients if queue full
	BackpressureReason string        // close reason sent to slow clients
	CloseGrace         time.Duration // time to allow close frame to flush

	// Logging
	Logger *log.Logger

	// Origin checker (nil = safe default: allow same-origin only)
	CheckOrigin func(r *http.Request) bool
}

// Defaults returns a hardened set of defaults.
func (o *ServerOptions) Defaults() {
	if o.WriteWait == 0 {
		o.WriteWait = 10 * time.Second
	}
	if o.PongWait == 0 {
		o.PongWait = 60 * time.Second
	}
	if o.PingPeriod == 0 {
		o.PingPeriod = 54 * time.Second // < PongWait
	}
	if o.ReadLimit == 0 {
		o.ReadLimit = 1 << 20 // 1 MiB
	}
	if o.SendBuffer == 0 {
		o.SendBuffer = 256
	}
	if o.BackpressureReason == "" {
		o.BackpressureReason = "going away: backpressure"
	}
	if o.CloseGrace == 0 {
		o.CloseGrace = 200 * time.Millisecond
	}
}

// Server is a WebSocket hub with channels and batching.
type Server struct {
	opts ServerOptions

	upgrader websocket.Upgrader

	// clients: all clients ever registered (for metrics & broadcast)
	clientsMu sync.RWMutex
	clients   map[*Client]struct{}
	// subs: per-channel subscribers
	subsMu sync.RWMutex
	subs   map[Channel]map[*Client]struct{}

	// batching
	pendingMu sync.Mutex
	pending   map[string]*pendingMessage // key -> aggregator

	// lifecycle
	closed      atomic.Bool
	connections atomic.Int64

	// handler for incoming messages
	messageHandler atomic.Value // MessageHandler

	// shutdown coordination
	wg sync.WaitGroup
}

// NewServer creates a new notifications server.
func NewServer(opts ServerOptions) *Server {
	opts.Defaults()

	s := &Server{
		opts:    opts,
		clients: make(map[*Client]struct{}),
		subs:    make(map[Channel]map[*Client]struct{}),
		pending: make(map[string]*pendingMessage),
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			if opts.CheckOrigin != nil {
				return opts.CheckOrigin(r)
			}
			// Safe default: allow if Origin host == Host header or Origin absent
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			// Very lightweight same-origin check
			// (You may replace with stricter policy or a whitelist.)
			return strings.Contains(origin, r.Host)
		},
		EnableCompression: opts.Compression,
	}
	return s
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logger != nil {
		s.opts.Logger.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// SetMessageHandler sets the handler for client-originated messages.
func (s *Server) SetMessageHandler(handler MessageHandler) {
	s.messageHandler.Store(handler)
}

func (s *Server) handler() MessageHandler {
	h, _ := s.messageHandler.Load().(MessageHandler)
	return h
}

// Client represents a single connection.
type Client struct {
	ID   string
	conn *websocket.Conn

	send      chan []byte
	closeOnce sync.Once
	closeCh   chan struct{}

	metadataMu sync.RWMutex
	metadata   map[string]any

	server *Server
}

// --- Public API -----------------------------------------------------------

// WebsocketHandler returns an http.Handler that upgrades and serves a client.
func (s *Server) WebsocketHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.closed.Load() {
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		conn, err := s.upgrader.Upgrade(w, r, nil)
		if err != nil {
			s.logf("websocket upgrade error: %v", err)
			return
		}
		client := s.newClient(conn)
		s.logf("client connected: %s", client.ID)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			client.serve()
		}()
	})
}

// Subscribe subscribes a client to a channel.
func (s *Server) Subscribe(c *Client, channel Channel) error {
	if !channel.Valid() {
		return fmt.Errorf("invalid channel: %s", channel)
	}

	name := channel
	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	m := s.subs[name]
	if m == nil {
		m = make(map[*Client]struct{})
		s.subs[name] = m
	}
	m[c] = struct{}{}
	return nil
}

// Unsubscribe removes a client from a channel.
func (s *Server) Unsubscribe(c *Client, channel Channel) {
	name := channel
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	if m := s.subs[name]; m != nil {
		delete(m, c)
		if len(m) == 0 {
			delete(s.subs, name)
		}
	}
}

// Publish broadcasts immediately to subscribers of channel.
func (s *Server) Publish(channel Channel, message any) error {
	return s.PublishWithType(channel, message, MessageTypeJSON, "", "")
}

func (s *Server) PublishWithType(channel Channel, message any, msgType MessageType, messageID, replyToID string) error {
	if !channel.Valid() {
		return fmt.Errorf("invalid channel: %s", channel)
	}
	payload := Payload{
		Channel:   string(channel),
		Type:      msgType,
		Time:      time.Now().UTC(),
		Message:   message,
		MessageID: messageID,
		ReplyToID: replyToID,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	s.broadcast(channel, b)
	return nil
}

// PublishToClient sends a message to a specific client.
func (s *Server) PublishToClient(client *Client, channel Channel, message any, msgType MessageType, messageID, replyToID string) error {
	if !channel.Valid() {
		return fmt.Errorf("invalid channel: %s", channel)
	}
	b, err := json.Marshal(Payload{
		Channel:   string(channel),
		Type:      msgType,
		Time:      time.Now().UTC(),
		Message:   message,
		MessageID: messageID,
		ReplyToID: replyToID,
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	return client.enqueueOrDrop(b, s.opts.DropOnBackpressure, s.opts.BackpressureReason)
}

// Batching -----------------------------------------------------------------

// PublishWait batches messages for (channel, ident) for ttl and then publishes.
func (s *Server) PublishWait(channel Channel, message any, ident uint, name string, ttl time.Duration) {
	s.PublishWaitWithType(channel, message, ident, name, ttl, MessageTypeJSON)
}

func (s *Server) PublishWaitWithType(channel Channel, message any, ident uint, name string, ttl time.Duration, msgType MessageType) {
	if !channel.Valid() {
		return
	}

	key := s.makeKey(channel, ident)
	s.pendingMu.Lock()
	pm, exists := s.pending[key]
	if !exists {
		// The flush logic is now a simple closure.
		onFlush := func(messages []any) {
			batched := map[string]any{
				"name":     name,
				"messages": messages,
			}
			_ = s.PublishWithType(channel, batched, msgType, "", "")
		}
		pm = newPending(ttl, onFlush)
		s.pending[key] = pm
	}
	s.pendingMu.Unlock()

	pm.add(message)

	pm.setOnFlushed(func() {
		s.pendingMu.Lock()
		delete(s.pending, key)
		s.pendingMu.Unlock()
	})
}

// PublishWaitString keeps your legacy text-join behavior.
func (s *Server) PublishWaitString(channel Channel, message string, ident uint, name string, ttl time.Duration) {
	if !channel.Valid() {
		return
	}

	key := s.makeKey(channel, ident)
	s.pendingMu.Lock()
	pm, exists := s.pending[key]
	if !exists {
		// The string-joining logic is now defined once, safely, inside this closure.
		onFlush := func(messages []any) {
			stringMessages := make([]string, len(messages))
			for i, m := range messages {
				if s, ok := m.(string); ok {
					stringMessages[i] = s
				} else {
					stringMessages[i] = fmt.Sprintf("%v", m)
				}
			}
			text := name + "\n\n" + strings.Join(stringMessages, "\n")
			_ = s.PublishWithType(channel, text, MessageTypeText, "", "")
		}
		pm = newPending(ttl, onFlush)
		s.pending[key] = pm
	}

	s.pendingMu.Unlock()

	pm.add(message)

	pm.setOnFlushed(func() {
		s.pendingMu.Lock()
		delete(s.pending, key)
		s.pendingMu.Unlock()
	})
}

func (s *Server) CancelPendingMessages(channel Channel, ident uint) {
	if !channel.Valid() {
		return
	}
	key := s.makeKey(channel, ident)
	s.pendingMu.Lock()
	if pm, ok := s.pending[key]; ok {
		pm.stop()
		delete(s.pending, key)
	}
	s.pendingMu.Unlock()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	// stop timers
	s.pendingMu.Lock()
	for k, pm := range s.pending {
		pm.stop()
		delete(s.pending, k)
	}
	s.pendingMu.Unlock()

	// close clients
	s.clientsMu.RLock()
	for c := range s.clients {
		c.closeWithReason(websocket.CloseGoingAway, "server shutdown")
	}
	s.clientsMu.RUnlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (s *Server) GetConnectedClients() int {
	return int(s.connections.Load())
}

func (s *Server) GetClientList() []string {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	ids := make([]string, 0, len(s.clients))
	for c := range s.clients {
		ids = append(ids, c.ID)
	}
	return ids
}

// --- internal -------------------------------------------------------------

func (s *Server) newClient(conn *websocket.Conn) *Client {
	c := &Client{
		ID:       generateClientID(),
		conn:     conn,
		send:     make(chan []byte, s.opts.SendBuffer),
		closeCh:  make(chan struct{}),
		metadata: make(map[string]any),
		server:   s,
	}
	s.clientsMu.Lock()
	s.clients[c] = struct{}{}
	s.clientsMu.Unlock()
	s.connections.Add(1)
	return c
}

func (s *Server) forgetClient(c *Client) {
	s.clientsMu.Lock()
	delete(s.clients, c)
	s.clientsMu.Unlock()

	// remove from all subscriptions
	s.subsMu.Lock()
	for ch, m := range s.subs {
		if _, ok := m[c]; ok {
			delete(m, c)
			if len(m) == 0 {
				delete(s.subs, ch)
			}
		}
	}
	s.subsMu.Unlock()

	s.connections.Add(^int64(0)) // -1
}

func (s *Server) broadcast(channel Channel, b []byte) {
	// copy subscribers to avoid holding lock while sending
	s.subsMu.RLock()
	targets := make([]*Client, 0, 8)
	if subs := s.subs[channel]; subs != nil {
		for c := range subs {
			targets = append(targets, c)
		}
	}
	s.subsMu.RUnlock()
	if len(targets) == 0 {
		return
	}
	for _, c := range targets {
		if err := c.enqueueOrDrop(b, s.opts.DropOnBackpressure, s.opts.BackpressureReason); err != nil {
			// already handled inside enqueueOrDrop
			s.logf("drop/bp client %s: %v", c.ID, err)
		}
	}
}

func (s *Server) makeKey(channel Channel, ident uint) string {
	return fmt.Sprintf("%s-%d", channel, ident)
}

// --- Client methods -------------------------------------------------------

func generateClientID() string {
	// monotonic-ish id; collision unlikely
	return fmt.Sprintf("c_%d_%d", time.Now().UnixNano(), runtime.NumGoroutine())
}

func (c *Client) GetMetadata(key string) (any, bool) {
	c.metadataMu.RLock()
	defer c.metadataMu.RUnlock()
	v, ok := c.metadata[key]
	return v, ok
}

func (c *Client) SetMetadata(key string, value any) {
	c.metadataMu.Lock()
	c.metadata[key] = value
	c.metadataMu.Unlock()
}

func (c *Client) serve() {
	defer c.server.forgetClient(c)
	defer c.conn.Close()

	// Reader
	go c.readPump()
	// Writer (blocks until closed)
	c.writePump()
}

func (c *Client) readPump() {
	defer c.closeWithReason(websocket.CloseNormalClosure, "readPump exit")

	c.conn.SetReadLimit(c.server.opts.ReadLimit)
	_ = c.conn.SetReadDeadline(time.Now().Add(c.server.opts.PongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(c.server.opts.PongWait))
	})

	for {
		msgType, data, err := c.conn.ReadMessage()
		if err != nil {
			// normal close or timeout
			return
		}
		switch msgType {
		case websocket.TextMessage:
			var incoming IncomingMessage
			if err := json.Unmarshal(data, &incoming); err != nil {
				c.sendError("invalid JSON format", "")
				continue
			}
			incoming.ClientID = c.ID
			if h := c.server.handler(); h != nil {
				h(c, incoming)
			}
		case websocket.BinaryMessage:
			incoming := IncomingMessage{
				Type:     MessageTypeBinary,
				Message:  data,
				ClientID: c.ID,
			}
			if h := c.server.handler(); h != nil {
				h(c, incoming)
			}
		case websocket.PingMessage:
			// reply with pong immediately (control frame)
			_ = c.conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(c.server.opts.WriteWait))
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(c.server.opts.PingPeriod)
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(c.server.opts.WriteWait))
			if !ok {
				// channel closed
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(c.server.opts.WriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.closeCh:
			// allow a short grace to flush a Close frame
			_ = c.conn.SetWriteDeadline(time.Now().Add(c.server.opts.CloseGrace))
			_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
			return
		}
	}
}

func (c *Client) enqueueOrDrop(b []byte, drop bool, reason string) error {
	select {
	case c.send <- b:
		return nil
	default:
		if drop {
			c.closeWithReason(websocket.CloseGoingAway, reason)
			return errors.New("backpressure: client queue full")
		}
		// Try a best-effort enqueue with timeout respecting WriteWait
		timer := time.NewTimer(c.server.opts.WriteWait)
		defer timer.Stop()
		select {
		case c.send <- b:
			return nil
		case <-timer.C:
			return errors.New("enqueue timeout")
		}
	}
}

type ErrorChannel struct{}

func (c *Client) sendError(msg, replyToID string) {
	errPayload := map[string]any{
		"error":     msg,
		"type":      "error",
		"time":      time.Now().UTC(),
		"reply_to":  replyToID,
		"client_id": c.ID,
	}
	b, _ := json.Marshal(errPayload)
	_ = c.enqueueOrDrop(b, true, "protocol error")
}

func (c *Client) closeWithReason(code int, reason string) {
	c.closeOnce.Do(func() {
		close(c.closeCh)
		// best effort control frame
		_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(100*time.Millisecond))
	})
}

func fastHashJSON(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
