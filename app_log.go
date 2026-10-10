package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"ssxng/internal/api"
	"ssxng/internal/logbuf"
)

// Logs: the bound log accessors and the optional log file.
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
}

func openFileLogger(path string, max int64) *fileLogger {
	l := &fileLogger{path: path, max: max}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return l
	}
	if info, err := os.Stat(path); err == nil && info.Size() > max {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return l
	}
	l.f = f
	if info, err := f.Stat(); err == nil {
		l.size = info.Size()
	}
	return l
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
			return
		}
		if _, err := l.f.Seek(0, 0); err != nil {
			return
		}
		l.size = 0
	}
	n, err := l.f.WriteString(line)
	if err == nil {
		l.size += int64(n)
	}
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
