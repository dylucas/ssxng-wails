package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"ssxng/internal/api"
	"ssxng/internal/logbuf"
)

// Logs: the bound log accessors, the batch pump that feeds the UI, and the
// optional log file.
// GetLogs returns log lines after afterSeq (or the most recent 500).

// Batching parameters for pushing log lines to the frontend.
const (
	// logBatchInterval is how long lines may sit before being pushed. 100ms is
	// below the threshold where a human notices a log line "not appearing" but
	// collapses a burst of connection logging into one event.
	logBatchInterval = 100 * time.Millisecond
	// logBatchMax caps one event's payload. A larger burst is split across
	// several events rather than truncated, so no line is lost at this layer.
	logBatchMax = 256
	// logChanCapacity bounds the subscription buffer. It is the only place lines
	// can be lost on the way to the UI: at the batch interval above it absorbs
	// thousands of lines per second, and the ring plus the log file stay
	// complete regardless (the frontend can always catch up with GetLogs).
	logChanCapacity = 1024

	// logDropWarnInterval bounds how often a "the UI log fell behind" warning is
	// emitted. The warning travels the same subscription it reports on, so on a
	// subscriber that is permanently stuck an unthrottled warning would itself
	// be dropped and re-trigger the next one, forever.
	logDropWarnInterval = 5 * time.Second
)

// startLogPump subscribes to the log ring and pushes batches to the frontend.
// It runs until stop is closed or the subscription ends.
func (a *App) startLogPump(stop <-chan struct{}) {
	if a.logs == nil {
		return
	}
	ch, cancel := a.logs.Subscribe(logChanCapacity)
	a.logCancel = cancel
	a.loops.Add(1)
	go func() {
		defer a.loops.Done()
		a.logPump(ch, stop)
	}()
}

// logPump coalesces the subscription into EventsEmit calls.
//
// Every exit path flushes what it has: the last few lines before shutdown are
// exactly the ones a user opens the log view to read.
func (a *App) logPump(ch <-chan logbuf.Entry, stop <-chan struct{}) {
	ticker := time.NewTicker(logBatchInterval)
	defer ticker.Stop()

	var pending []logbuf.Entry
	flush := func() {
		if len(pending) == 0 {
			a.reportDroppedLogs()
			return
		}
		for start := 0; start < len(pending); start += logBatchMax {
			end := start + logBatchMax
			if end > len(pending) {
				end = len(pending)
			}
			a.emitLogBatch(pending[start:end])
		}
		// Reuse the backing array: this runs on every tick and the batch is
		// transient. The entries themselves are values, so nothing is retained.
		pending = pending[:0]
		a.reportDroppedLogs()
	}

	for {
		select {
		case <-stop:
			// Drain whatever the logger managed to queue before we stopped.
			for {
				select {
				case e := <-ch:
					pending = append(pending, e)
					continue
				default:
				}
				break
			}
			flush()
			return
		case e, ok := <-ch:
			if !ok {
				flush()
				return
			}
			pending = append(pending, e)
		case <-ticker.C:
			flush()
		}
	}
}

// emitLogBatch converts and sends one batch. It is a variable so tests can
// capture batches without a Wails context.
var emitLogBatchFn = func(a *App, entries []api.LogEntry) {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, EventLogs, entries)
}

func (a *App) emitLogBatch(entries []logbuf.Entry) {
	out := make([]api.LogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, a.toLogEntry(e))
	}
	emitLogBatchFn(a, out)
}

// reportDroppedLogs makes a dropped UI batch observable. It logs through the
// normal path (so the line lands in the ring and the file) and is a no-op when
// nothing was dropped, which is the normal case.
//
// The report is rate limited: see logDropWarnInterval. When it is suppressed the
// counter is left alone, so the next report covers everything dropped in the
// meantime instead of throwing that number away.
func (a *App) reportDroppedLogs() {
	if a.logs == nil {
		return
	}
	now := time.Now().UnixNano()
	if prev := a.lastLogDrop.Load(); prev != 0 && now-prev < int64(logDropWarnInterval) {
		return
	}
	n := a.logs.TakeDropped()
	if n <= 0 {
		return
	}
	a.lastLogDrop.Store(now)
	a.logf("warn", "app", "界面日志推送落后，%d 条未实时显示（可在日志页刷新补全）", n)
}

// GetLogs returns log lines after afterSeq (or the most recent 500).
func (a *App) GetLogs(afterSeq int64) []api.LogEntry {
	entries := a.logs.Entries(500)
	out := make([]api.LogEntry, 0, len(entries))
	for _, e := range entries {
		if afterSeq > 0 && e.Seq <= afterSeq {
			continue
		}
		out = append(out, a.toLogEntry(e))
	}
	return out
}

// ClearLogs empties the in-app log.
func (a *App) ClearLogs() {
	a.logs.Clear()
	a.logf("info", "app", "日志已清空")
}

// fileLogger appends log lines to a file, rotating by truncation.
type fileLogger struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64

	// openErr records why the file could not be opened (bad permissions, full
	// disk, missing directory), and failures/ lastErr record write errors. A
	// logger that silently drops every line is worse than no logger, so the
	// diagnostics view reads these back. They are never reported through logf:
	// that writes to this very logger and would recurse.
	openErr  string
	failures int64
	lastErr  string
}

func openFileLogger(path string, max int64) *fileLogger {
	l := &fileLogger{path: path, max: max}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		l.openErr = err.Error()
		return l
	}
	if info, err := os.Stat(path); err == nil && info.Size() > max {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		l.openErr = err.Error()
		return l
	}
	l.f = f
	if info, err := f.Stat(); err == nil {
		l.size = info.Size()
	}
	return l
}

// Status reports the logger's health for the diagnostics view: whether it is
// writing, and the last failure if it is not.
func (l *fileLogger) Status() (path string, failures int64, lastErr string) {
	if l == nil {
		return "", 0, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.lastErr
	if err == "" {
		err = l.openErr
	}
	return l.path, l.failures, err
}

func (l *fileLogger) Write(e logbuf.Entry) {
	if l == nil || l.f == nil {
		return
	}
	line := fmt.Sprintf("%s [%s] %s: %s\n", e.Time.Format("2006-01-02 15:04:05"), strings.ToUpper(e.Level), e.Scope, e.Message)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > l.max {
		if err := l.f.Truncate(0); err != nil {
			l.recordFailure(err)
			return
		}
		if _, err := l.f.Seek(0, 0); err != nil {
			l.recordFailure(err)
			return
		}
		l.size = 0
	}
	n, err := l.f.WriteString(line)
	if err != nil {
		l.recordFailure(err)
		return
	}
	l.size += int64(n)
}

// recordFailure is called with l.mu held.
func (l *fileLogger) recordFailure(err error) {
	l.failures++
	l.lastErr = err.Error()
}

func (l *fileLogger) Close() {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Close()
	l.f = nil
}
