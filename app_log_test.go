package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ssxng/internal/api"
	"ssxng/internal/logbuf"
)

// batchRecorder captures the batches the pump would have pushed to the
// frontend, replacing the Wails EventEmit call the test has no context for.
type batchRecorder struct {
	mu      sync.Mutex
	batches [][]api.LogEntry
}

func (r *batchRecorder) add(entries []api.LogEntry) {
	r.mu.Lock()
	// Copy: the pump reuses its slice between batches.
	cp := make([]api.LogEntry, len(entries))
	copy(cp, entries)
	r.batches = append(r.batches, cp)
	r.mu.Unlock()
}

func (r *batchRecorder) snapshot() [][]api.LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]api.LogEntry, len(r.batches))
	copy(out, r.batches)
	return out
}

// flatten returns every entry in delivery order.
func (r *batchRecorder) flatten() []api.LogEntry {
	var out []api.LogEntry
	for _, b := range r.snapshot() {
		out = append(out, b...)
	}
	return out
}

// withRecorder swaps the emit hook for a recorder and restores it afterwards.
// emitLogBatchFn is package state, so these tests must not run in parallel.
func withRecorder(t *testing.T) *batchRecorder {
	t.Helper()
	rec := &batchRecorder{}
	prev := emitLogBatchFn
	emitLogBatchFn = func(_ *App, entries []api.LogEntry) { rec.add(entries) }
	t.Cleanup(func() { emitLogBatchFn = prev })
	return rec
}

// TestLogPumpBatchesABurstInOrder is the core contract of the batched log path:
// a burst of a thousand lines still arrives completely, in sequence order, with
// no batch larger than the payload cap.
func TestLogPumpBatchesABurstInOrder(t *testing.T) {
	rec := withRecorder(t)

	a := &App{}
	ch := make(chan logbuf.Entry, 2000)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.logPump(ch, stop)
	}()

	const n = 1000
	for i := 1; i <= n; i++ {
		ch <- logbuf.Entry{Seq: int64(i), Level: "info", Scope: "proxy", Message: fmt.Sprintf("行 %d", i)}
	}
	// Close stop with the burst still queued: the shutdown path must drain it.
	close(stop)
	<-done

	batches := rec.snapshot()
	if len(batches) == 0 {
		t.Fatal("关闭时应把队列里的日志冲刷出去，实际一条批次都没有")
	}
	for i, b := range batches {
		if len(b) == 0 {
			t.Errorf("第 %d 个批次是空的；空批次不应该被推送", i)
		}
		if len(b) > logBatchMax {
			t.Errorf("第 %d 个批次 %d 条，超过上限 %d", i, len(b), logBatchMax)
		}
	}
	got := rec.flatten()
	if len(got) != n {
		t.Fatalf("应送出 %d 条，实际 %d", n, len(got))
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("第 %d 条序号错乱: 期望 %d，实际 %d", i, i+1, e.Seq)
		}
	}
	// A burst larger than one batch must actually be split, otherwise the cap
	// assertion above would pass vacuously with a single oversized batch.
	if n > logBatchMax && len(batches) < 2 {
		t.Errorf("%d 条应分成多个批次，实际只有 %d 个", n, len(batches))
	}
}

// TestLogPumpEmitsNothingWhenIdle guards the common case: a quiet app must not
// push empty log events at the batch rate.
func TestLogPumpEmitsNothingWhenIdle(t *testing.T) {
	rec := withRecorder(t)

	a := &App{}
	ch := make(chan logbuf.Entry, 4)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.logPump(ch, stop)
	}()

	// Long enough for several ticks to fire.
	time.Sleep(3*logBatchInterval + 20*time.Millisecond)
	close(stop)
	<-done

	if got := rec.flatten(); len(got) != 0 {
		t.Errorf("空闲时不应推送日志事件，实际 %d 条", len(got))
	}
}

// TestLogPumpFlushesTheTailOnStop pins the shutdown requirement: the last lines
// logged before quitting are the ones a user opens the log view to read.
func TestLogPumpFlushesTheTailOnStop(t *testing.T) {
	rec := withRecorder(t)

	a := &App{}
	ch := make(chan logbuf.Entry, 8)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.logPump(ch, stop)
	}()

	// Queued well inside one tick, so a correct pump can only deliver these
	// through the stop path.
	ch <- logbuf.Entry{Seq: 7, Level: "error", Scope: "proxy", Message: "最后一行"}
	close(stop)
	<-done

	got := rec.flatten()
	if len(got) != 1 || got[0].Message != "最后一行" {
		t.Fatalf("停止前应冲刷出尾部的 1 条，实际 %+v", got)
	}
	if got[0].Level != "error" || got[0].Scope != "proxy" {
		t.Errorf("尾部条目的级别/范围丢失: %+v", got[0])
	}
}

// TestLogfFeedsTheBatchPump exercises the real wiring end to end: logf writes
// through the ring and the subscription, and the pump turns it into batches.
func TestLogfFeedsTheBatchPump(t *testing.T) {
	rec := withRecorder(t)

	// Ring large enough that nothing is evicted while the burst is buffered.
	a := &App{logs: logbuf.New(4096)}
	stop := make(chan struct{})
	a.startLogPump(stop)

	const n = 300
	for i := 0; i < n; i++ {
		a.logf("info", "proxy", "连接 %d", i)
	}
	close(stop)
	a.loops.Wait()
	if a.logCancel != nil {
		a.logCancel()
		a.logCancel = nil
	}

	got := rec.flatten()
	if len(got) != n {
		t.Fatalf("logf 写入 %d 条，界面只收到 %d 条", n, len(got))
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("第 %d 条序号不连续: %d", i, e.Seq)
		}
		if e.Scope != "proxy" || e.Level != "info" {
			t.Fatalf("第 %d 条字段丢失: %+v", i, e)
		}
	}
	// The ring is the source of truth GetLogs reads back; it must hold the same
	// lines the batches carried.
	if stored := a.GetLogs(0); len(stored) != n {
		t.Errorf("环形缓冲应有 %d 条，实际 %d", n, len(stored))
	}
}

// TestLogPumpReportsDroppedBatches checks that a subscriber which cannot keep up
// is *visible*: the drop is counted and surfaced as a warning line rather than
// silently losing lines. It also pins the rate limit, because the warning itself
// travels the subscription it reports on — without the limit, a stuck subscriber
// would drop the warning and re-trigger it forever.
func TestLogPumpReportsDroppedBatches(t *testing.T) {
	rec := withRecorder(t)

	// The pump is deliberately not started, so the subscription buffer fills and
	// every further line is dropped.
	a := &App{logs: logbuf.New(4096)}
	ch, cancel := a.logs.Subscribe(4)
	defer cancel()

	for i := 0; i < 20; i++ {
		a.logf("info", "proxy", "连接 %d", i)
	}
	a.reportDroppedLogs()
	countWarns := func() int {
		n := 0
		for _, e := range a.logs.Entries(500) {
			if e.Level == "warn" && e.Scope == "app" {
				n++
			}
		}
		return n
	}
	if got := countWarns(); got != 1 {
		t.Fatalf("丢弃推送后应记一条 warn 日志，实际 %d 条", got)
	}
	// A second call inside the interval must not add another line...
	a.reportDroppedLogs()
	if got := countWarns(); got != 1 {
		t.Errorf("限流窗口内不应重复告警，实际 %d 条", got)
	}
	// ...and must not throw the pending count away: the suppressed call leaves
	// the counter alone, so the next allowed report covers the whole window.
	for i := 0; i < 5; i++ {
		a.logf("info", "proxy", "追加 %d", i)
	}
	a.lastLogDrop.Store(time.Now().Add(-2 * logDropWarnInterval).UnixNano())
	a.reportDroppedLogs()
	if got := countWarns(); got != 2 {
		t.Fatalf("限流窗口过后应再记一条 warn，实际 %d 条", got)
	}
	// The second report must carry the drops accumulated while it was throttled,
	// not just the first batch's count.
	last := a.logs.Entries(1)
	if len(last) != 1 {
		t.Fatalf("应能读回最后一条日志，实际 %d 条", len(last))
	}
	if want := "界面日志推送落后"; !strings.Contains(last[0].Message, want) {
		t.Errorf("告警内容不对: %q", last[0].Message)
	}
	if strings.Contains(last[0].Message, "1 条未实时显示") {
		t.Errorf("被限流期间的丢弃应累计上报，实际只报了 1 条: %q", last[0].Message)
	}
	// Nothing was pushed: this test never started the pump.
	if got := rec.flatten(); len(got) != 0 {
		t.Errorf("未启动 pump 时不应有推送，实际 %d 条", len(got))
	}
	drain(ch)
}

func drain(ch <-chan logbuf.Entry) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// TestFileLoggerReportsAnUnopenablePath covers the real failure the diagnostics
// row exists for: a path that cannot be created at all.
func TestFileLoggerReportsAnUnopenablePath(t *testing.T) {
	// A file where the directory should be makes MkdirAll fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	l := openFileLogger(filepath.Join(blocker, "ssxng.log"), 1024)

	path, failures, errText := l.Status()
	if errText == "" {
		t.Fatal("无法打开时应记录原因")
	}
	if path == "" {
		t.Error("状态里应带上路径")
	}
	if failures != 0 {
		t.Errorf("尚未写入时失败次数应为 0，实际 %d", failures)
	}
	// Writing must not panic and must stay silent to the caller.
	l.Write(logbuf.Entry{Level: "info", Scope: "app", Message: "line"})
	l.Close()
}

// TestFileLoggerCountsWriteFailures drives a write failure against a closed
// descriptor, which is the shape of the "disk went away" case.
func TestFileLoggerCountsWriteFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssxng.log")
	l := openFileLogger(path, 1<<20)
	if _, _, errText := l.Status(); errText != "" {
		t.Fatalf("正常打开不应报错: %s", errText)
	}
	l.Write(logbuf.Entry{Level: "info", Scope: "app", Message: "第一条"})

	// Close the underlying file behind the logger's back so the next write fails
	// the way a full disk does, without depending on disk quotas.
	l.mu.Lock()
	_ = l.f.Close()
	l.mu.Unlock()

	l.Write(logbuf.Entry{Level: "info", Scope: "app", Message: "失败的一条"})
	_, failures, errText := l.Status()
	if failures != 1 {
		t.Errorf("应记录 1 次写入失败，实际 %d（err=%q）", failures, errText)
	}
	if errText == "" {
		t.Error("写入失败时应记录原因")
	}
	// A second failure is counted too, so the user can tell it is persistent.
	l.Write(logbuf.Entry{Level: "info", Scope: "app", Message: "再失败一次"})
	if _, failures, _ = l.Status(); failures != 2 {
		t.Errorf("应累计到 2 次失败，实际 %d", failures)
	}
	l.Close()
}
