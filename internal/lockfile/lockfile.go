// Package lockfile implements the single-instance guard.
package lockfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrAlreadyRunning is returned when another instance holds the lock.
var ErrAlreadyRunning = errors.New("另一个 SSXNG 实例已在运行")

// Lock is an exclusive, advisory process lock backed by flock(2). The lock is
// released automatically when the process exits (even on a crash), so a stale
// file never blocks a restart.
type Lock struct {
	path string
	file *os.File
}

// Info is written into the lock file to help the UI point the user at the
// running instance.
type Info struct {
	PID  int    `json:"pid"`
	Path string `json:"path"`
}

// Acquire takes the lock at path, creating parent directories as needed.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrAlreadyRunning
	}
	// We own the lock: record who we are.
	exe, _ := os.Executable()
	_ = f.Truncate(0)
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	enc, _ := json.Marshal(Info{PID: os.Getpid(), Path: exe})
	if _, err := f.Write(append(enc, '\n')); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{path: path, file: f}, nil
}

// RunningInfo reports the PID recorded by the current lock holder, if any.
func RunningInfo(path string) (Info, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return Info{}, fmt.Errorf("锁文件损坏: %w", err)
	}
	return info, nil
}

// Release unlocks the lock file.
//
// The file itself is left on disk on purpose: removing it here would race with
// a new instance that acquired the lock immediately after this one unlocked and
// has already written its own PID. A stale file is harmless, Acquire takes it
// over.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	if cerr := l.file.Close(); err == nil {
		err = cerr
	}
	l.file = nil
	return err
}
