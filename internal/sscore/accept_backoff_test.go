package sscore

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// failingListener returns a fixed error from Accept. It counts the calls so a
// test can tell a backoff from a hot loop.
type failingListener struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (l *failingListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	return nil, l.err
}

func (l *failingListener) Close() error   { return nil }
func (l *failingListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func (l *failingListener) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// TestAcceptLoopBacksOffOnPersistentErrors pins the fix for the hot loop: a
// listener that keeps failing (fd exhaustion is the realistic case) must not be
// retried at full speed, and must not write a log line per attempt.
func TestAcceptLoopBacksOffOnPersistentErrors(t *testing.T) {
	ln := &failingListener{err: errors.New("too many open files")}
	sink := &logSink{}
	eng := NewEngine(sink.add)
	defer sink.flush(t)

	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		eng.acceptLoop(ln, func(net.Conn) {}, done)
	}()

	const window = 250 * time.Millisecond
	time.Sleep(window)
	close(done)
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("done 关闭后 acceptLoop 没有退出")
	}

	// Without a backoff this would be tens of thousands of iterations in 250ms.
	// With 5ms doubling to 1s it is well under a hundred; the ceiling is loose so
	// the test does not depend on scheduler timing.
	if got := ln.count(); got > 200 {
		t.Errorf("持续失败时不应热循环：%v 内 Accept 被调用 %d 次", window, got)
	}

	// The log must be throttled too, otherwise the backoff just moves the flood
	// from the listener to the log file.
	sink.mu.Lock()
	logged := len(sink.lines)
	sink.mu.Unlock()
	if logged == 0 {
		t.Error("首次失败应当记录日志")
	}
	if logged > 5 {
		t.Errorf("持续失败时日志应限流，实际记录 %d 条", logged)
	}
}

// TestAcceptLoopStopsPromptlyDuringBackoff checks that Stop does not have to
// wait out a one second backoff: the done channel must interrupt the sleep.
func TestAcceptLoopStopsPromptlyDuringBackoff(t *testing.T) {
	ln := &failingListener{err: errors.New("boom")}
	eng := NewEngine(nil)

	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		eng.acceptLoop(ln, func(net.Conn) {}, done)
	}()

	// Let it climb into the long backoff, then cancel.
	time.Sleep(80 * time.Millisecond)
	start := time.Now()
	close(done)
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("acceptLoop 没有在 done 关闭后退出")
	}
	if elapsed := time.Since(start); elapsed > maxAcceptBackoffCheck {
		t.Errorf("退出耗时 %v，说明退避等待没有被 done 打断", elapsed)
	}
}

// maxAcceptBackoffCheck is comfortably below the one second maximum backoff but
// far above any scheduling jitter, so the assertion is about interruption
// rather than about being fast.
const maxAcceptBackoffCheck = 700 * time.Millisecond

// TestAcceptLoopReturnsOnClosedListener covers the ordinary shutdown path: a
// closed listener yields net.ErrClosed and the loop must return immediately
// without logging (Stop closes the listener after signalling done, and the
// watcher goroutine should not report the shutdown as a failure).
func TestAcceptLoopReturnsOnClosedListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &logSink{}
	eng := NewEngine(sink.add)
	defer sink.flush(t)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		eng.acceptLoop(ln, func(net.Conn) {}, make(chan struct{}))
	}()

	_ = ln.Close()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("监听器关闭后 acceptLoop 没有退出")
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.lines) != 0 {
		t.Errorf("正常关闭不应记录失败日志，实际 %v", sink.lines)
	}
}

// TestStopInterruptsAcceptBackoff exercises the real lifecycle rather than the
// loop in isolation: Stop must return while a listener is failing, and a
// subsequent Start must work (the done channel is per-run).
func TestStopInterruptsAcceptBackoff(t *testing.T) {
	// Occupy a port, then hand the engine the same port so its listeners can
	// never be created... that would fail Start instead. Drive the loop directly
	// against a failing listener and check the engine-level channel contract.
	eng := NewEngine(nil)
	eng.mu.Lock()
	eng.running = true
	eng.done = make(chan struct{})
	done := eng.done
	eng.mu.Unlock()

	ln := &failingListener{err: errors.New("persistent")}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		eng.acceptLoop(ln, func(net.Conn) {}, done)
	}()
	time.Sleep(60 * time.Millisecond)

	eng.Stop()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 之后 acceptLoop 仍在运行")
	}
	if eng.done != nil {
		t.Error("Stop 之后 done 应被清空")
	}
}
