package hub

import (
	"testing"
	"time"
)

func TestPublishFanoutAndBacklog(t *testing.T) {
	h := New(Config{SweepInterval: time.Hour})
	defer h.Close()

	sub, backlog := h.Subscribe("t1")
	if len(backlog) != 0 {
		t.Fatalf("expected empty backlog, got %d", len(backlog))
	}

	h.Publish(Message{Table: "t1", Address: "cosmos1a", Text: "hi", UnixMs: time.Now().UnixMilli()})
	select {
	case m := <-sub.C():
		if m.Text != "hi" {
			t.Fatalf("got %q", m.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("no message delivered")
	}

	// A late subscriber receives the backlog.
	_, late := h.Subscribe("t1")
	if len(late) != 1 {
		t.Fatalf("expected 1 backlog message, got %d", len(late))
	}
}

func TestRingBufferBound(t *testing.T) {
	h := New(Config{MaxMessagesPerTable: 3, SweepInterval: time.Hour})
	defer h.Close()
	for range 10 {
		h.Publish(Message{Table: "t", Text: "m", UnixMs: time.Now().UnixMilli()})
	}
	_, backlog := h.Subscribe("t")
	if len(backlog) != 3 {
		t.Fatalf("expected ring bounded to 3, got %d", len(backlog))
	}
}

func TestSweepDropsExpiredAndIdle(t *testing.T) {
	now := time.Now()
	h := New(Config{MessageTTL: time.Minute, TableIdleTTL: time.Minute, SweepInterval: time.Hour})
	defer h.Close()
	h.now = func() time.Time { return now }

	h.Publish(Message{Table: "old", Text: "stale", UnixMs: now.Add(-2 * time.Minute).UnixMilli()})

	// Advance clock past both TTLs; the table has no subscribers, so it reaps.
	h.now = func() time.Time { return now.Add(10 * time.Minute) }
	h.sweep()

	tables, _ := h.Stats()
	if tables != 0 {
		t.Fatalf("expected idle table reaped, got %d tables", tables)
	}
}
