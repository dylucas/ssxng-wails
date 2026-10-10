package main

import (
	"testing"
	"time"

	"ssxng/internal/proxyconf"
)

// stubHelperStatus swaps the package-level probe for a counting stub. All tests
// in this file must restore it, so they use this helper instead of assigning the
// variable directly.
func stubHelperStatus(t *testing.T, info proxyconf.HelperStatusInfo) *int {
	t.Helper()
	calls := 0
	orig := helperStatus
	helperStatus = func() proxyconf.HelperStatusInfo {
		calls++
		return info
	}
	t.Cleanup(func() { helperStatus = orig })
	return &calls
}

// TestHelperStatusIsCachedAcrossStatePushes is the regression test for the
// process-per-state-push problem: proxyconf.Status execs the setuid helper
// whenever it is installed, and buildState used to probe on every push.
func TestHelperStatusIsCachedAcrossStatePushes(t *testing.T) {
	app, _, _ := newTestApp(t)
	installed := proxyconf.HelperStatusInfo{
		Path: "/Library/Application Support/SSXNG/ssxng-helper",
		// Installed + a version is the case that actually forks.
		Installed: true, Version: "1.0.0", RequiredVersion: "1.0.0", VersionOK: true,
	}
	calls := stubHelperStatus(t, installed)

	for i := 0; i < 10; i++ {
		view := app.buildState().Helper
		if !view.Installed || view.Version != "1.0.0" {
			t.Fatalf("第 %d 次状态推送的助手信息不对: %+v", i+1, view)
		}
	}
	if *calls != 1 {
		t.Errorf("连续状态推送只应探测一次助手，实际 %d 次", *calls)
	}

	// A mode switch, a settings save and a server selection all funnel through
	// emitState/buildState; none of them should re-probe within the TTL.
	_ = app.buildState()
	_ = app.buildState()
	if *calls != 1 {
		t.Errorf("TTL 内不应重新探测，实际 %d 次", *calls)
	}
}

// TestHelperStatusRefreshForcesAProbe covers the 状态 page's refresh button: it
// must not be answered from the cache, or the user could not verify an install
// they just performed by hand.
func TestHelperStatusRefreshForcesAProbe(t *testing.T) {
	app, _, _ := newTestApp(t)
	calls := stubHelperStatus(t, proxyconf.HelperStatusInfo{Installed: true, Version: "1.0.0"})

	_ = app.buildState()
	if *calls != 1 {
		t.Fatalf("首次应探测一次，实际 %d 次", *calls)
	}
	_ = app.buildState()
	if *calls != 1 {
		t.Fatalf("缓存命中不应探测，实际 %d 次", *calls)
	}

	// HelperStatus (the bound method behind the UI refresh button) forces.
	if got := app.HelperStatus(); !got.Installed {
		t.Errorf("强制刷新应返回助手信息: %+v", got)
	}
	if *calls != 2 {
		t.Errorf("强制刷新应重新探测，实际 %d 次", *calls)
	}
	if got := app.HelperStatus(); !got.Installed {
		t.Errorf("第二次强制刷新应返回助手信息: %+v", got)
	}
	if *calls != 3 {
		t.Errorf("每次都强制刷新应重新探测，实际 %d 次", *calls)
	}
}

// TestHelperStatusCacheExpires proves the cache is a TTL and not a permanent
// latch: a helper removed behind the app's back must eventually be noticed.
func TestHelperStatusCacheExpires(t *testing.T) {
	app, _, _ := newTestApp(t)
	calls := stubHelperStatus(t, proxyconf.HelperStatusInfo{Installed: true, Version: "1.0.0"})

	_ = app.buildState()
	if *calls != 1 {
		t.Fatalf("首次应探测一次，实际 %d 次", *calls)
	}

	// Age the cache past the TTL rather than sleeping in the test.
	app.helperCache.mu.Lock()
	app.helperCache.at = time.Now().Add(-2 * helperProbeTTL)
	app.helperCache.mu.Unlock()

	_ = app.buildState()
	if *calls != 2 {
		t.Errorf("超过 TTL 后应重新探测，实际 %d 次", *calls)
	}
}

// TestInstallAndUninstallInvalidateTheHelperCache pins that the cache cannot
// hide the effect of the two operations that change the helper on disk.
func TestInstallAndUninstallInvalidateTheHelperCache(t *testing.T) {
	app, _, _ := newTestApp(t)
	// Warm the cache with "not installed".
	calls := stubHelperStatus(t, proxyconf.HelperStatusInfo{LastError: "助手未安装"})
	if got := app.buildState().Helper; got.Installed {
		t.Fatalf("预热状态应为未安装: %+v", got)
	}
	if *calls != 1 {
		t.Fatalf("预热应探测一次，实际 %d 次", *calls)
	}

	// Simulate the post-install state and invalidate, exactly as InstallHelper
	// does after proxyconf.InstallHelper succeeds.
	helperStatus = func() proxyconf.HelperStatusInfo {
		return proxyconf.HelperStatusInfo{Installed: true, Version: "1.1.0", VersionOK: true}
	}
	app.invalidateHelperCache()

	if got := app.buildState().Helper; !got.Installed || got.Version != "1.1.0" {
		t.Errorf("安装后应看到新版本，实际 %+v", got)
	}

	// And back to uninstalled.
	helperStatus = func() proxyconf.HelperStatusInfo { return proxyconf.HelperStatusInfo{LastError: "助手未安装"} }
	app.invalidateHelperCache()
	if got := app.buildState().Helper; got.Installed {
		t.Errorf("卸载后不应仍报告已安装: %+v", got)
	}
}

// TestHelperViewKeepsAppliedServices checks the refactor did not drop the
// "applied network services" half of the view, which comes from the applier
// rather than the probe.
func TestHelperViewKeepsAppliedServices(t *testing.T) {
	app, _, _ := newTestApp(t)
	stubHelperStatus(t, proxyconf.HelperStatusInfo{Installed: true, Version: "1.0.0", VersionOK: true})
	app.applier.record(appliedProxy{Mode: "global", Services: []string{"Wi-Fi", "Ethernet"}})

	view := app.buildState().Helper
	if len(view.Applied) != 2 || view.Applied[0] != "Wi-Fi" {
		t.Errorf("已应用的服务列表丢失: %+v", view.Applied)
	}
}
