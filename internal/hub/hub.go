// Package hub holds the in-memory, per-table chat state. Nothing is persisted:
// messages live in a bounded ring buffer per table and are dropped when old, and
// whole table groups are reaped once they have been idle for too long.
package hub

import (
	"sync"
	"time"
)

// Message is a single chat line, already authenticated by the caller.
type Message struct {
	Table   string `json:"table"`
	Address string `json:"address"`
	Text    string `json:"text"`
	// UnixMs is the server receive time in milliseconds.
	UnixMs int64 `json:"ts"`
}

// Config tunes retention. Zero values fall back to sane defaults.
type Config struct {
	// MaxMessagesPerTable caps the ring buffer; older messages are overwritten.
	MaxMessagesPerTable int
	// MessageTTL drops messages older than this on read/broadcast.
	MessageTTL time.Duration
	// TableIdleTTL reaps a whole table group after this long with no activity.
	TableIdleTTL time.Duration
	// SweepInterval is how often the reaper runs.
	SweepInterval time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxMessagesPerTable <= 0 {
		c.MaxMessagesPerTable = 200
	}
	if c.MessageTTL <= 0 {
		c.MessageTTL = time.Hour
	}
	if c.TableIdleTTL <= 0 {
		c.TableIdleTTL = 2 * time.Hour
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = 5 * time.Minute
	}
	return c
}

// Subscriber receives broadcast messages for a table. Delivery is best-effort:
// a slow subscriber whose buffer is full is skipped rather than blocking others.
type Subscriber struct {
	ch chan Message
}

// C is the channel to read delivered messages from.
func (s *Subscriber) C() <-chan Message { return s.ch }

type table struct {
	messages    []Message
	subscribers map[*Subscriber]struct{}
	lastActive  time.Time
}

// Hub is the concurrency-safe registry of tables. The zero value is not usable;
// call New.
type Hub struct {
	cfg    Config
	mu     sync.Mutex
	tables map[string]*table
	now    func() time.Time
	stop   chan struct{}
}

// New returns a running Hub. Call Close to stop the background reaper.
func New(cfg Config) *Hub {
	h := &Hub{
		cfg:    cfg.withDefaults(),
		tables: make(map[string]*table),
		now:    time.Now,
		stop:   make(chan struct{}),
	}
	go h.reaper()
	return h
}

// Close stops the background reaper.
func (h *Hub) Close() { close(h.stop) }

func (h *Hub) getOrCreate(name string) *table {
	t := h.tables[name]
	if t == nil {
		t = &table{subscribers: make(map[*Subscriber]struct{}), lastActive: h.now()}
		h.tables[name] = t
	}
	return t
}

// Subscribe joins a table and returns a Subscriber plus the recent (non-expired)
// backlog so a new client can see context. Call Unsubscribe when done.
func (h *Hub) Subscribe(table string) (*Subscriber, []Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.getOrCreate(table)
	t.lastActive = h.now()
	sub := &Subscriber{ch: make(chan Message, 64)}
	t.subscribers[sub] = struct{}{}
	return sub, h.liveMessagesLocked(t)
}

// Unsubscribe removes a subscriber from a table.
func (h *Hub) Unsubscribe(table string, sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.tables[table]
	if t == nil {
		return
	}
	delete(t.subscribers, sub)
	close(sub.ch)
}

// Publish stores a message and fans it out to the table's subscribers.
func (h *Hub) Publish(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.getOrCreate(m.Table)
	t.lastActive = h.now()

	t.messages = append(t.messages, m)
	if len(t.messages) > h.cfg.MaxMessagesPerTable {
		// Drop the oldest, keeping the buffer bounded.
		t.messages = t.messages[len(t.messages)-h.cfg.MaxMessagesPerTable:]
	}

	for sub := range t.subscribers {
		select {
		case sub.ch <- m:
		default:
			// Slow consumer: skip rather than block the whole table.
		}
	}
}

func (h *Hub) liveMessagesLocked(t *table) []Message {
	cutoff := h.now().Add(-h.cfg.MessageTTL).UnixMilli()
	out := make([]Message, 0, len(t.messages))
	for _, m := range t.messages {
		if m.UnixMs >= cutoff {
			out = append(out, m)
		}
	}
	return out
}

// reaper periodically purges expired messages and idle tables.
func (h *Hub) reaper() {
	ticker := time.NewTicker(h.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			h.sweep()
		}
	}
}

func (h *Hub) sweep() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	msgCutoff := now.Add(-h.cfg.MessageTTL).UnixMilli()
	idleCutoff := now.Add(-h.cfg.TableIdleTTL)

	for name, t := range h.tables {
		// Reap idle tables with no live subscribers.
		if len(t.subscribers) == 0 && t.lastActive.Before(idleCutoff) {
			delete(h.tables, name)
			continue
		}
		// Purge expired messages in place.
		kept := t.messages[:0]
		for _, m := range t.messages {
			if m.UnixMs >= msgCutoff {
				kept = append(kept, m)
			}
		}
		t.messages = kept
	}
}

// Stats reports current table and subscriber counts (for health/observability).
func (h *Hub) Stats() (tables, subscribers int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.tables {
		subscribers += len(t.subscribers)
	}
	return len(h.tables), subscribers
}
