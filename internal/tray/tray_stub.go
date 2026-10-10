//go:build !darwin || !cgo

package tray

import "errors"

// Menu holds the callbacks and the state provider used by the status item.
type Menu struct {
	OnToggle func()
	OnMode   func(mode string)
	OnShow   func()
	OnQuit   func()

	// State returns the tooltip title, whether the proxy is on, a status line,
	// the current mode and the modes that should be offered.
	State func() (title string, enabled bool, status string, currentMode string, modes []ModeOption)
}

// Tray is a live status item. On platforms without AppKit it is never created.
type Tray struct{}

// Start always fails on unsupported platforms.
func Start(menu Menu) (*Tray, error) {
	return nil, errors.New("当前平台不支持菜单栏图标")
}

// Running always reports false.
func (t *Tray) Running() bool { return false }

// Refresh does nothing.
func (t *Tray) Refresh() {}

// Stop does nothing.
func (t *Tray) Stop() {}

// SetDockIconVisible always fails on unsupported platforms.
func SetDockIconVisible(visible bool) error {
	return errors.New("当前平台不支持切换 Dock 图标")
}
