// Package logbuf keeps the recent log lines shown in the UI and streams new
// ones to subscribers.
package logbuf

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Entry is one log line.
type Entry struct {
	Seq     int64
	Time    time.Time
	Level   string // debug, info, warn, error
	Scope   string // proxy, socks, http, helper, pac, app, ...
	Message string
}

// Buffer is a bounded, concurrency safe log ring.
type Buffer struct {
	mu    sync.Mutex
	items []Entry
	limit int
	subs  map[int]chan Entry
	next  int
	seq   int64

	// dropped counts entries a subscriber's channel was too full to accept. The
	// subscription exists to push lines to the UI, and the UI is allowed to miss
	// lines (it catches up with GetLogs); the ring and the log file are still
	// complete. Counting it here keeps that loss visible instead of silent.
	dropped atomic.Int64
}

// New creates a buffer holding at most limit lines.
func New(limit int) *Buffer {
	if limit <= 0 {
		limit = 500
	}
	return &Buffer{limit: limit, subs: make(map[int]chan Entry)}
}

// AddEntry appends a line, notifies subscribers without blocking, and returns
// the stored entry. Callers that need the sequence number or the normalised
// level (the app stamps it into its file log and the batch it pushes to the UI)
// would otherwise have to read the ring back with Entries(1), taking the lock a
// second time and allocating a slice for every single line.
func (b *Buffer) AddEntry(level, scope, message string) Entry {
	level = strings.ToLower(strings.TrimSpace(level))
	switch level {
	case "debug", "info", "warn", "error":
	default:
		level = "info"
	}
	e := Entry{Time: time.Now(), Level: level, Scope: scope, Message: message}

	b.mu.Lock()
	b.seq++
	e.Seq = b.seq
	b.items = append(b.items, e)
	if len(b.items) > b.limit {
		b.items = b.items[len(b.items)-b.limit:]
	}
	// Fan out while still holding the lock. The sends are non-blocking, so this
	// cannot stall a proxy goroutine logging on its hot path, and it makes
	// "cancel closes the channel" race-free: a subscriber is removed and closed
	// under the same lock, so no send can ever target a closed channel.
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// A slow subscriber must never block the caller: proxy goroutines log
			// on their hot path, and stalling there would stall traffic.
			b.dropped.Add(1)
		}
	}
	b.mu.Unlock()

	return e
}

// TakeDropped returns how many entries have been dropped since the previous
// call and resets the counter, so a caller can report the loss without
// repeating itself.
func (b *Buffer) TakeDropped() int64 { return b.dropped.Swap(0) }

// Entries returns up to limit of the most recent lines, oldest first.
func (b *Buffer) Entries(limit int) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 || limit > len(b.items) {
		limit = len(b.items)
	}
	out := make([]Entry, limit)
	copy(out, b.items[len(b.items)-limit:])
	return out
}

// Len is the number of buffered lines.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// LastSeq is the sequence number of the most recent line.
func (b *Buffer) LastSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Clear drops every buffered line.
func (b *Buffer) Clear() {
	b.mu.Lock()
	b.items = nil
	b.mu.Unlock()
}

// Subscribe returns a channel of new entries and a function to unsubscribe.
//
// The channel is buffered and deliveries are non-blocking: a subscriber that
// cannot keep up loses entries (counted by TakeDropped) rather than slowing the
// producer down. Unsubscribing removes the subscriber and closes the channel
// under the same lock AddEntry fans out with, so a caller may unsubscribe while
// other goroutines are still logging.
func (b *Buffer) Subscribe(capacity int) (<-chan Entry, func()) {
	if capacity <= 0 {
		capacity = 256
	}
	ch := make(chan Entry, capacity)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			close(ch)
			b.mu.Unlock()
		})
	}
}
