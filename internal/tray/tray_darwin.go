//go:build darwin && cgo

// Package tray shows a macOS menu bar icon, mirroring ShadowsocksX-NG's status
// item: the current mode and server as a disabled first row, a proxy on/off
// switch, one radio item per enabled mode, and the window/quit commands.
package tray

/*
#cgo LDFLAGS: -framework Cocoa -framework AppKit -framework Foundation
#include <stdlib.h>
#include "tray_darwin.h"
*/
import "C"

import (
	"errors"
	"sync"
	"unsafe"
)

// Menu holds the callbacks and the state provider used by the status item.
type Menu struct {
	OnToggle func()
	OnMode   func(mode string)
	OnShow   func()
	OnQuit   func()

	// OnCopy runs after a copy-command item was clicked, with the text that was
	// copied. The pasteboard is already written by AppKit at that point, so
	// copying works even when no window exists; this hook only exists so the app
	// can log it and toast.
	OnCopy func(command string)

	// State returns the snapshot the menu should render.
	State func() Snapshot
}

// Tray is a live status item.
type Tray struct {
	mu      sync.Mutex
	menu    Menu
	running bool
}

var (
	currentMu sync.Mutex
	current   *Tray
)

// Start creates the status item. It returns an error when no menu bar is
// available (for example over SSH).
func Start(menu Menu) (*Tray, error) {
	if menu.State == nil {
		return nil, errors.New("缺少菜单状态回调")
	}
	t := &Tray{menu: menu}

	currentMu.Lock()
	if current != nil {
		currentMu.Unlock()
		return nil, errors.New("菜单栏图标已经存在")
	}
	current = t
	currentMu.Unlock()

	if C.ssxngTrayStart() != 0 {
		currentMu.Lock()
		current = nil
		currentMu.Unlock()
		return nil, errors.New("无法创建菜单栏图标（没有可用的菜单栏？）")
	}
	t.mu.Lock()
	t.running = true
	t.mu.Unlock()
	t.Refresh()
	return t, nil
}

// Running reports whether the icon is installed.
func (t *Tray) Running() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

// Refresh rebuilds the menu from the current state.
func (t *Tray) Refresh() {
	if t == nil || t.menu.State == nil {
		return
	}
	s := t.menu.State()
	// The menu labels live in Go (they are the same strings the window uses),
	// so they are handed to AppKit as "key|title" and "key|title|command" pairs.
	cModesStr, cCmdsStr := EncodeMenuSpecs(s.Modes, s.Commands)
	cTitle := C.CString(s.Title)
	cStatus := C.CString(s.Status)
	cMode := C.CString(s.CurrentMode)
	cModes := C.CString(cModesStr)
	cCmds := C.CString(cCmdsStr)
	defer func() {
		C.free(unsafe.Pointer(cTitle))
		C.free(unsafe.Pointer(cStatus))
		C.free(unsafe.Pointer(cMode))
		C.free(unsafe.Pointer(cModes))
		C.free(unsafe.Pointer(cCmds))
	}()
	C.ssxngTrayRefresh(cTitle, cStatus, cMode, cModes, cCmds, cBool(s.Enabled))
}

// Stop removes the icon.
func (t *Tray) Stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}
	t.running = false
	t.mu.Unlock()

	C.ssxngTrayStop()
	currentMu.Lock()
	if current == t {
		current = nil
	}
	currentMu.Unlock()
}

// SetDockIconVisible switches between a regular (dock icon) and an accessory
// (menu bar only) application.
func SetDockIconVisible(visible bool) error {
	if C.ssxngSetDockIcon(cBool(visible)) != 0 {
		return errors.New("无法切换 Dock 图标（可能不在图形会话中）")
	}
	return nil
}

func cBool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

//export ssxngTrayEvent
func ssxngTrayEvent(kind C.int, mode *C.char) {
	currentMu.Lock()
	t := current
	currentMu.Unlock()
	if t == nil {
		return
	}
	// Run the callback off the AppKit thread: it usually ends up touching the
	// application state, emitting events and refreshing this menu again.
	switch int(kind) {
	case 1:
		if t.menu.OnToggle != nil {
			go t.menu.OnToggle()
		}
	case 2:
		m := C.GoString(mode)
		if t.menu.OnMode != nil {
			go t.menu.OnMode(m)
		}
	case 3:
		if t.menu.OnShow != nil {
			go t.menu.OnShow()
		}
	case 4:
		if t.menu.OnQuit != nil {
			go t.menu.OnQuit()
		}
	case 5:
		// AppKit already put the text on the pasteboard; this is the callback that
		// lets the app log it and toast. Copy the string here, on this thread,
		// because the C buffer dies with the menu item's autorelease pool.
		if t.menu.OnCopy == nil {
			return
		}
		cmd := C.GoString(mode)
		go t.menu.OnCopy(cmd)
	}
}
