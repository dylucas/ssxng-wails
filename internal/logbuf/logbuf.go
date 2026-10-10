// Package logbuf keeps the recent log lines shown in the UI and streams new
// ones to subscribers.
package logbuf

import (
	"strings"
	"sync"
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
}

// New creates a buffer holding at most limit lines.
func New(limit int) *Buffer {
	if limit <= 0 {
		limit = 500
	}
	return &Buffer{limit: limit, subs: make(map[int]chan Entry)}
}

// Add appends a line and notifies subscribers without blocking.
func (b *Buffer) Add(level, scope, message string) {
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
	subs := make([]chan Entry, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- e:
		default: // a slow subscriber must never block the caller
		}
	}
}

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
			b.mu.Unlock()
			close(ch)
		})
	}
}
