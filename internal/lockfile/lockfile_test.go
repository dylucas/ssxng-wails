package lockfile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssxng.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("首次加锁应当成功: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("加锁应当写出文件: %v", err)
	}

	// The lock file records the owning process so the UI can tell the user which
	// instance is already running.
	info, err := RunningInfo(path)
	if err != nil {
		t.Fatalf("读取锁信息失败: %v", err)
	}
	if info.PID != os.Getpid() {
		t.Errorf("锁里的 PID 应为当前进程 %d，实际 %d", os.Getpid(), info.PID)
	}
	if info.Path == "" {
		t.Error("锁信息应当包含可执行文件路径")
	}

	if _, err := Acquire(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("第二次加锁应当返回 ErrAlreadyRunning，实际 %v", err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("释放锁失败: %v", err)
	}
	// The file stays behind on purpose (see Lock.Release); what matters is that
	// the flock is gone, so a new instance can take over.
	again, err := Acquire(path)
	if err != nil {
		t.Fatalf("释放后应当可以重新加锁: %v", err)
	}
	defer again.Release()
}

func TestAcquireTakesOverStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssxng.lock")
	// A PID that cannot exist (macOS pids are well below this) simulates a crash
	// that left the file behind.
	stale := Info{PID: 999999, Path: "/nonexistent/SSXNG"}
	raw, _ := json.Marshal(stale)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("残留锁应当可以被接管: %v", err)
	}
	defer lock.Release()
	if info, err := RunningInfo(path); err != nil || info.PID != os.Getpid() {
		t.Fatalf("接管后应当写入当前进程: %+v %v", info, err)
	}
}

func TestAcquireOverwritesCorruptLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssxng.lock")
	if err := os.WriteFile(path, []byte("这不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("损坏的锁文件不应阻塞启动: %v", err)
	}
	defer lock.Release()
}

func TestRunningInfoWithoutFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.lock")
	if _, err := RunningInfo(path); err == nil {
		t.Error("锁文件不存在时应当返回错误")
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssxng.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Errorf("重复释放不应报错: %v", err)
	}
}
