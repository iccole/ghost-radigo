package server

import "sync"

// client holds the per-listener receive channel.
type client struct {
	ch chan []byte
}

// Broadcast fans out byte writes to all currently registered listeners.
// Each listener has a buffered channel; if it's full the chunk is dropped
// for that listener (they're too slow) rather than stalling the whole stream.
type Broadcast struct {
	mu      sync.RWMutex
	clients map[uint64]*client
	next    uint64
}

func newBroadcast() *Broadcast {
	return &Broadcast{clients: make(map[uint64]*client)}
}

// Register creates a new listener and returns its id and receive channel.
func (b *Broadcast) Register() (uint64, <-chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	c := &client{ch: make(chan []byte, 64)}
	b.clients[id] = c
	return id, c.ch
}

// Deregister removes the listener and closes its channel.
func (b *Broadcast) Deregister(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.clients[id]; ok {
		close(c.ch)
		delete(b.clients, id)
	}
}

// Write sends a copy of p to every registered listener.
// It satisfies io.Writer so the Mixer can write directly to the server.
func (b *Broadcast) Write(p []byte) (int, error) {
	// Make one copy shared across all listeners — each only reads it.
	chunk := make([]byte, len(p))
	copy(chunk, p)

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, c := range b.clients {
		select {
		case c.ch <- chunk:
		default:
			// Listener is behind; drop this chunk for them.
		}
	}
	return len(p), nil
}
