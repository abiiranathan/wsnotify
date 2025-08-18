package wsnotify

import (
	"sync"
	"time"
)

type pendingMessage struct {
	mu        sync.Mutex
	timer     *time.Timer
	messages  []any
	seen      map[string]struct{} // dedup by hash of JSON
	onFlushed func()              // called after flush completes
}

// newPending now takes the flush function directly.
func newPending(ttl time.Duration, onFlush func(messages []any)) *pendingMessage {
	pm := &pendingMessage{
		messages: make([]any, 0, 4),
		seen:     make(map[string]struct{}),
	}
	pm.timer = time.AfterFunc(ttl, func() {
		pm.mu.Lock()
		// Create a copy of the messages to process after unlocking.
		msgs := append([]any{}, pm.messages...)
		onFlushedCallback := pm.onFlushed
		pm.mu.Unlock()

		if len(msgs) > 0 {
			onFlush(msgs)
		}

		if onFlushedCallback != nil {
			onFlushedCallback()
		}
	})
	return pm
}

func (pm *pendingMessage) add(message any) {
	hash := fastHashJSON(message)
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Stop the timer if we've already been stopped (e.g., by a previous flush)
	if pm.timer == nil {
		return
	}

	if _, dup := pm.seen[hash]; !dup {
		pm.seen[hash] = struct{}{}
		pm.messages = append(pm.messages, message)
	}
}

func (pm *pendingMessage) stop() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.timer != nil {
		pm.timer.Stop()
		pm.timer = nil // Prevent further additions
	}
}

func (pm *pendingMessage) setOnFlushed(f func()) {
	pm.mu.Lock()
	pm.onFlushed = f
	pm.mu.Unlock()
}
