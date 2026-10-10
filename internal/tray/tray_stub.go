//go:build !darwin || !cgo

package tray

import "errors"

// Menu holds the callbacks and the state provider used by the status item.
type Menu struct {
	OnToggle func()
	OnMode   func(mode string)
	OnShow   func()
	OnQuit   func()
	OnCopy   func(command string)

	// State returns the snapshot the menu should render.
	State func() Snapshot
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
