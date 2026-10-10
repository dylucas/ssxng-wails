package sscore

import (
	"sync/atomic"
)

// counters feeds the global traffic totals and, for the duration of one
// connection, the per-connection byte counts used in the debug log lines.
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
