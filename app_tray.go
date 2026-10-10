package main

import (
	"fmt"
	"strconv"
	"strings"

	"ssxng/internal/config"
	"ssxng/internal/tray"
)

// Menu bar integration.
// startTray creates the menu bar icon.

func (a *App) startTray() {
	t, err := tray.Start(tray.Menu{
		OnToggle: func() {
			sett := a.settings()
			if err := a.SetEnabled(!sett.Enabled); err != nil {
				a.toastError("app", err)
			}
		},
		OnMode: func(mode string) {
			if err := a.SetMode(mode); err != nil {
				a.toastError("app", err)
			}
		},
		OnShow: func() { a.ShowWindow() },
		OnQuit: func() { a.Quit() },
		OnCopy: func(command string) {
			// AppKit 已经写进剪贴板，这里只负责留下痕迹并提示。
			a.logf("info", "app", "菜单栏已复制命令行代理命令: %s", command)
			a.emitToast("success", "已复制到剪贴板")
		},
		State: a.trayState,
	})
	if err != nil {
		a.logf("warn", "app", "菜单栏图标不可用: %v", err)
		return
	}
	a.tray = t
}

// trayState feeds the menu bar with the current status line, mode list and the
// copy-command items.
func (a *App) trayState() tray.Snapshot {
	sett := a.settings()
	status := modeLabel(sett.Mode)
	if !sett.Enabled {
		status = "已关闭"
	} else if cur, ok := a.store.CurrentServer(); ok {
		status = fmt.Sprintf("%s · %s", status, cur.Title())
	}
	modes := make([]tray.ModeOption, 0, len(config.ModeOrder))
	for _, m := range enabledModes(sett) {
		modes = append(modes, tray.ModeOption{Key: m, Title: modeLabel(m)})
	}
	return tray.Snapshot{
		Title:       "SSXNG",
		Status:      status,
		Enabled:     sett.Enabled,
		CurrentMode: sett.Mode,
		Modes:       modes,
		Commands:    a.trayProxyCommands(),
	}
}

// trayProxyCommands is the menu bar's counterpart of the window's 命令行代理
// block. For any configuration the app itself can save (config.Validate rejects
// a blank address and a port outside 1024-65535) it produces exactly the text
// cliProxyCommands() in frontend/src/app-views.js produces — TestTrayProxyCommandsMatchTheWindow
// and the ui-check pin the same literals on either side. The two only diverge on
// a hand-edited config.json holding nonsense (port 0, whitespace address), where
// this falls back to the defaults instead of emitting a command that points
// nowhere.
func (a *App) trayProxyCommands() []tray.ProxyCommand {
	sett := a.settings()
	exportPair := func(scheme, address string, port int) string {
		host := strings.TrimSpace(address)
		if host == "" {
			host = "127.0.0.1"
		}
		url := scheme + "://" + host + ":" + strconv.Itoa(port)
		return "export http_proxy=" + url + ";export https_proxy=" + url + ";"
	}
	portOr := func(p, fallback int) int {
		if p < 1 || p > 65535 {
			return fallback
		}
		return p
	}

	// HTTP first: that is the order the window uses, and it matches SSX-NG's
	// "Copy HTTP Proxy Shell Export Command" being the primary one.
	cmds := make([]tray.ProxyCommand, 0, 2)
	if sett.HTTP.Enabled {
		cmds = append(cmds, tray.ProxyCommand{
			Key:     "http",
			Title:   "复制 HTTP 代理命令",
			Command: exportPair("http", sett.HTTP.Address, portOr(sett.HTTP.Port, 1087)),
		})
	}
	cmds = append(cmds, tray.ProxyCommand{
		Key:     "socks",
		Title:   "复制 SOCKS5 代理命令",
		Command: exportPair("socks5", sett.SOCKS.Address, portOr(sett.SOCKS.Port, 1086)),
	})
	return cmds
}
