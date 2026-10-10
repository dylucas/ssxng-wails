package pac

import (
	"os"
	"sync"
	"time"
)

// ruleCountParse is the parsing step Count runs when a file has changed. It is
// a variable so tests can count how often the expensive path is taken.
var ruleCountParse = RuleCountFromPAC

// RuleCache memoises the rule count of a generated PAC file.
//
// The UI asks for the rule count on every state push (App.buildState ->
// pacView), and emitState runs on every mode switch, settings save and server
// selection. Parsing the whole embedded JSON rule array each time is thousands
// of allocations for a number that only changes when the PAC file is
// regenerated, so the count is kept until the file's mtime/size change.
//
// A zero-value RuleCache is ready to use.
type RuleCache struct {
	mu    sync.Mutex
	path  string
	mod   time.Time
	size  int64
	count int
	valid bool
}

// Count returns the number of rules in the PAC file at path, re-reading it only
// when the file is new or has been modified since the last call. A missing or
// unreadable file yields 0; the error is deliberately not surfaced because the
// caller (pacView) reports file problems separately, from its own read.
func (c *RuleCache) Count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		// Drop the memo: the file may come back with a different identity.
		c.valid = false
		return 0
	}
	if c.valid && c.path == path && c.size == info.Size() && c.mod.Equal(info.ModTime()) {
		return c.count
	}

	data, err := os.ReadFile(path)
	if err != nil {
		c.valid = false
		return 0
	}
	count := ruleCountParse(data)
	// Record the stat we already took rather than re-statting after the read:
	// if the file changed while we were reading it, a later call simply sees a
	// different mtime/size and parses again, which is the safe direction.
	c.path, c.mod, c.size = path, info.ModTime(), info.Size()
	c.count, c.valid = count, true
	return count
}

// Invalidate drops the memoised count, forcing the next Count to re-read. It is
// called after the PAC file is regenerated so the UI never shows a stale count
// even if the new file happens to land on the same mtime/size.
func (c *RuleCache) Invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}
