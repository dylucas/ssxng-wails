package sscore

import (
	"sync"
	"sync/atomic"
	"time"
)

// counters feeds both the global traffic totals and the per-connection rows.
type counters struct {
	connUp, connDown atomic.Int64
	globUp, globDown *atomic.Int64
}

func (c *counters) addUp(n int64) {
	c.connUp.Add(n)
	if c.globUp != nil {
		c.globUp.Add(n)
	}
}

func (c *counters) addDown(n int64) {
	c.connDown.Add(n)
	if c.globDown != nil {
		c.globDown.Add(n)
	}
}

// ConnInfo describes one proxied connection for the UI.
type ConnInfo struct {
	ID      int64
	Target  string
	Started time.Time
	Up      int64
	Down    int64
	Active  bool
	Err     string
}

// connTable keeps the recently seen connections, newest first. It is a small
// fixed size ring: the UI only ever shows the last few dozen rows.
type connTable struct {
	mu     sync.Mutex
	nextID int64
	items  []*ConnInfo
	limit  int
}

func newConnTable(limit int) *connTable {
	if limit <= 0 {
		limit = 200
	}
	return &connTable{limit: limit}
}

// begin records a new connection and returns its id.
func (t *connTable) begin(target string) (int64, *ConnInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	info := &ConnInfo{ID: t.nextID, Target: target, Started: time.Now(), Active: true}
	t.items = append(t.items, info)
	if len(t.items) > t.limit {
		t.items = t.items[len(t.items)-t.limit:]
	}
	return info.ID, info
}

// end marks a connection as finished.
func (t *connTable) end(id int64, up, down int64, errMsg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, it := range t.items {
		if it.ID == id {
			it.Active = false
			it.Up = up
			it.Down = down
			it.Err = errMsg
			return
		}
	}
}

// snapshot returns the most recent connections, newest first.
func (t *connTable) snapshot(max int) []ConnInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ConnInfo, 0, max)
	for i := len(t.items) - 1; i >= 0 && len(out) < max; i-- {
		out = append(out, *t.items[i])
	}
	return out
}

// clear drops the history.
func (t *connTable) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.items = nil
}
