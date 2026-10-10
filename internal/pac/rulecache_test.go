package pac

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePACFile writes a minimal but valid generated PAC file holding n rules.
func writePACFile(t *testing.T, path string, n int) {
	t.Helper()
	rules := make([]byte, 0, 32*n)
	rules = append(rules, "var rules = ["...)
	for i := 0; i < n; i++ {
		if i > 0 {
			rules = append(rules, ',')
		}
		rules = append(rules, `"r`...)
		rules = append(rules, byte('a'+i%26))
		rules = append(rules, `"`...)
	}
	rules = append(rules, "];"...)
	if err := os.WriteFile(path, rules, 0o600); err != nil {
		t.Fatalf("写入 PAC 文件: %v", err)
	}
}

// countParseCalls swaps the parsing hook for a counting wrapper and restores it.
func countParseCalls(t *testing.T) *int {
	t.Helper()
	calls := 0
	orig := ruleCountParse
	ruleCountParse = func(content []byte) int {
		calls++
		return orig(content)
	}
	t.Cleanup(func() { ruleCountParse = orig })
	return &calls
}

func TestRuleCacheParsesOncePerFileState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gfwlist.js")
	writePACFile(t, path, 5)
	calls := countParseCalls(t)

	var c RuleCache
	if got := c.Count(path); got != 5 {
		t.Fatalf("规则数应为 5，实际 %d", got)
	}
	if *calls != 1 {
		t.Fatalf("首次调用应解析一次，实际 %d 次", *calls)
	}

	// Repeated calls with an unchanged file are the whole point: buildState runs
	// on every state push, and each one used to re-parse thousands of rules.
	for i := 0; i < 20; i++ {
		if got := c.Count(path); got != 5 {
			t.Fatalf("第 %d 次调用规则数应为 5，实际 %d", i+2, got)
		}
	}
	if *calls != 1 {
		t.Errorf("文件未变时不应重新解析，实际解析 %d 次", *calls)
	}
}

func TestRuleCacheReparsesAfterRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gfwlist.js")
	writePACFile(t, path, 3)
	calls := countParseCalls(t)

	var c RuleCache
	if got := c.Count(path); got != 3 {
		t.Fatalf("规则数应为 3，实际 %d", got)
	}

	// A regeneration changes both size and mtime; the count must follow.
	writePACFile(t, path, 7)
	if got := c.Count(path); got != 7 {
		t.Errorf("重写后规则数应为 7，实际 %d", got)
	}
	if *calls != 2 {
		t.Errorf("重写后应重新解析（解析 %d 次）", *calls)
	}
}

// A file rewritten in place with the same size must still be noticed: the cache
// keys on mtime as well, and Invalidate exists for the case where even that
// looks unchanged.
func TestRuleCacheHonoursInvalidateAndMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gfwlist.js")
	writePACFile(t, path, 4)
	calls := countParseCalls(t)

	var c RuleCache
	if got := c.Count(path); got != 4 {
		t.Fatalf("规则数应为 4，实际 %d", got)
	}

	// Same rule count, different bytes: bump the mtime explicitly so this does
	// not depend on filesystem timestamp resolution.
	writePACFile(t, path, 4)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("调整 mtime: %v", err)
	}
	if got := c.Count(path); got != 4 {
		t.Errorf("mtime 变化后规则数应为 4，实际 %d", got)
	}
	if *calls != 2 {
		t.Errorf("mtime 变化后应重新解析（解析 %d 次）", *calls)
	}

	// Invalidate forces a re-read even when mtime and size look identical.
	c.Invalidate()
	if got := c.Count(path); got != 4 {
		t.Errorf("失效后规则数应为 4，实际 %d", got)
	}
	if *calls != 3 {
		t.Errorf("失效后应重新解析（解析 %d 次）", *calls)
	}
}

func TestRuleCacheMissingAndEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gfwlist.js")
	calls := countParseCalls(t)

	var c RuleCache
	if got := c.Count(path); got != 0 {
		t.Errorf("文件不存在时应为 0，实际 %d", got)
	}
	if *calls != 0 {
		t.Errorf("文件不存在时不应解析（解析 %d 次）", *calls)
	}

	// A zero byte file is a legitimate state ("PAC 文件为空" in the UI) and must
	// not be counted as having rules.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("写入空文件: %v", err)
	}
	if got := c.Count(path); got != 0 {
		t.Errorf("空文件应为 0 条规则，实际 %d", got)
	}

	// A file with no rules array at all is not an error either.
	if err := os.WriteFile(path, []byte("// nothing here\n"), 0o600); err != nil {
		t.Fatalf("写入无规则文件: %v", err)
	}
	if got := c.Count(path); got != 0 {
		t.Errorf("无规则数组时应为 0，实际 %d", got)
	}
}

// A cache warmed on one path must not answer for another (the config directory
// is configurable, and a cached count from the old one would be wrong).
func TestRuleCacheTracksPath(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.js")
	second := filepath.Join(dir, "b.js")
	writePACFile(t, first, 2)
	writePACFile(t, second, 9)

	var c RuleCache
	if got := c.Count(first); got != 2 {
		t.Fatalf("第一个文件应为 2 条，实际 %d", got)
	}
	if got := c.Count(second); got != 9 {
		t.Errorf("换路径后应读新文件（9 条），实际 %d", got)
	}
	if got := c.Count(first); got != 2 {
		t.Errorf("切回第一个文件应为 2 条，实际 %d", got)
	}
}

// The zero value must be usable, since App holds the cache by value.
func TestRuleCacheZeroValueIsUsable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gfwlist.js")
	writePACFile(t, path, 1)

	var c RuleCache
	if got := c.Count(path); got != 1 {
		t.Fatalf("零值缓存应能工作，实际 %d", got)
	}
	c.Invalidate()
	if got := c.Count(path); got != 1 {
		t.Errorf("失效后仍应返回 1，实际 %d", got)
	}
}
