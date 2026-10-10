package main

import (
	"os"
	"strings"
	"testing"
)

// writePACFile puts a minimal but valid generated PAC file in the store's
// directory, holding n rules.
func writePACFile(t *testing.T, path string, n int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("var rules = [")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"rule`)
		b.WriteByte(byte('a' + i%26))
		b.WriteString(`"`)
	}
	b.WriteString("];")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写入 PAC 文件: %v", err)
	}
}

// TestPACViewReadsTheRealFileOncePerState pins the contract between buildState
// and the PAC file: the count comes from the file on disk, tracks rewrites, and
// never asks the UI to display a stale number.
//
// This is the integration half of the pac.RuleCache tests: they prove the cache
// stops re-parsing, this proves it is wired to the right path and invalidated at
// the right time (the failure mode being a permanently frozen rule count).
func TestPACViewReadsTheRealFileOncePerState(t *testing.T) {
	app, store, _ := newTestApp(t)

	// No PAC file yet: the UI must say so rather than claim "0 条规则".
	view := app.buildState().PAC
	if view.RuleCount != 0 {
		t.Errorf("没有 PAC 文件时规则数应为 0，实际 %d", view.RuleCount)
	}
	if !strings.Contains(view.LastError, "尚未生成") {
		t.Errorf("缺少 PAC 文件时应提示先生成，实际 %q", view.LastError)
	}

	writePACFile(t, store.PACFilePath(), 5)
	view = app.buildState().PAC
	if view.RuleCount != 5 {
		t.Fatalf("规则数应为 5，实际 %d", view.RuleCount)
	}
	if view.LastError != "" {
		t.Errorf("有可用 PAC 文件时不应报错，实际 %q", view.LastError)
	}
	if view.Generated == 0 {
		t.Error("应当报告 PAC 文件的生成时间")
	}
	if view.Source == "" {
		t.Error("应当报告数据源")
	}

	// Repeated state pushes must keep answering from the same file. The count is
	// identical, but the point is that the path is right and the cache is warm:
	// a wrong path would flip the view back to "尚未生成".
	for i := 0; i < 5; i++ {
		if got := app.buildState().PAC.RuleCount; got != 5 {
			t.Fatalf("第 %d 次状态推送规则数应为 5，实际 %d", i+2, got)
		}
	}

	// A regenerated file must be picked up without a restart.
	writePACFile(t, store.PACFilePath(), 11)
	if got := app.buildState().PAC.RuleCount; got != 11 {
		t.Errorf("重写 PAC 后规则数应为 11，实际 %d", got)
	}

	// Invalidate (called by generatePAC) must force a re-read even when the file
	// looks unchanged to the stat-based check.
	app.pacRules.Invalidate()
	if got := app.buildState().PAC.RuleCount; got != 11 {
		t.Errorf("失效后规则数应为 11，实际 %d", got)
	}

	// An empty file is a distinct, reportable state.
	if err := os.WriteFile(store.PACFilePath(), nil, 0o600); err != nil {
		t.Fatalf("清空 PAC 文件: %v", err)
	}
	view = app.buildState().PAC
	if view.RuleCount != 0 {
		t.Errorf("空文件规则数应为 0，实际 %d", view.RuleCount)
	}
	if !strings.Contains(view.LastError, "为空") {
		t.Errorf("空文件应提示重新生成，实际 %q", view.LastError)
	}

	// Deleting it goes back to the "not generated yet" wording.
	if err := os.Remove(store.PACFilePath()); err != nil {
		t.Fatalf("删除 PAC 文件: %v", err)
	}
	view = app.buildState().PAC
	if !strings.Contains(view.LastError, "尚未生成") {
		t.Errorf("删除后应提示先生成，实际 %q", view.LastError)
	}
}

// TestPACSourcePrefersTheDownloadedList checks the 数据源 label without reading
// the list: it must report the download when the cache exists and is non-empty,
// and fall back to the local wording otherwise.
func TestPACSourcePrefersTheDownloadedList(t *testing.T) {
	app, store, sett := newTestApp(t)

	if got := app.pacSource(sett); got != "本地规则" {
		t.Errorf("没有下载缓存时应显示本地规则，实际 %q", got)
	}

	if err := os.WriteFile(store.GFWListPath(), []byte("Z2Z3bGlzdA=="), 0o600); err != nil {
		t.Fatalf("写入 gfwlist: %v", err)
	}
	got := app.pacSource(sett)
	if got == "本地规则" || !strings.Contains(got, "更新") {
		t.Errorf("有下载缓存时应显示来源与时间，实际 %q", got)
	}

	// A zero byte cache is not a usable source.
	if err := os.WriteFile(store.GFWListPath(), nil, 0o600); err != nil {
		t.Fatalf("清空 gfwlist: %v", err)
	}
	if got := app.pacSource(sett); got != "本地规则" {
		t.Errorf("空缓存应回退到本地规则，实际 %q", got)
	}
}
