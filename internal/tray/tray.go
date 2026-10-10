package tray

import "strings"

// ModeOption is one entry of the mode submenu.
type ModeOption struct {
	Key   string
	Title string
}

// ProxyCommand is one "copy the shell export command" entry of the menu bar.
// Command is written to the pasteboard verbatim, so the app generates it from
// the same saved settings the window's 命令行代理 block uses (see
// App.trayProxyCommands); the two must stay in step.
type ProxyCommand struct {
	Key     string
	Title   string
	Command string
}

// Snapshot is everything the status item renders. It is rebuilt on every
// Refresh, so the menu always reflects the current settings.
type Snapshot struct {
	// Title becomes the status item tooltip.
	Title string
	// Status is the disabled first row, e.g. "全局代理 · 节点名".
	Status string
	// Enabled drives the 开启/关闭代理 label.
	Enabled bool
	// CurrentMode gets the radio mark in the mode list.
	CurrentMode string
	// Modes are the modes the user allowed in the settings.
	Modes []ModeOption
	// Commands are the copy-command items, already formatted text.
	Commands []ProxyCommand
}

// EncodeMenuSpecs renders the menu contents as the newline-joined strings the
// Objective-C side parses: "key|title" per mode and "key|title|command" per copy
// item, with empty entries skipped. It lives here rather than inline in the cgo
// file so that the wire format AppKit depends on is covered by ordinary Go
// tests (see tray_test.go) instead of only being exercised by hand.
//
// AppKit splits each command line on the first two "|" and joins the rest back
// together, so a title must not contain "|" (none do) but a command may.
func EncodeMenuSpecs(modes []ModeOption, commands []ProxyCommand) (modesSpec, commandsSpec string) {
	modeLines := make([]string, 0, len(modes))
	for _, m := range modes {
		if strings.TrimSpace(m.Key) == "" {
			continue
		}
		modeLines = append(modeLines, m.Key+"|"+m.Title)
	}
	commandLines := make([]string, 0, len(commands))
	for _, c := range commands {
		if strings.TrimSpace(c.Command) == "" {
			continue
		}
		commandLines = append(commandLines, c.Key+"|"+c.Title+"|"+c.Command)
	}
	return strings.Join(modeLines, "\n"), strings.Join(commandLines, "\n")
}
