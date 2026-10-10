// Package launchd registers SSXNG as a login item through a per-user
// LaunchAgent.
//
// ShadowsocksX-NG uses SMLoginItemSetEnabled with a bundled helper application;
// a LaunchAgent reaches the same result with one small plist and no extra bundle.
package launchd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const launchctlPath = "/bin/launchctl"

// PlistPath returns ~/Library/LaunchAgents/<label>.plist.
func PlistPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(label) == "" {
		return "", errors.New("缺少 LaunchAgent 标签")
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// SetEnabled installs or removes the login item.
func SetEnabled(enabled bool, label, execPath string) error {
	if strings.TrimSpace(execPath) == "" {
		return errors.New("缺少可执行文件路径")
	}
	path, err := PlistPath(label)
	if err != nil {
		return err
	}

	if !enabled {
		// Unload first and ignore failures: the item may never have been loaded.
		_ = runLaunchctl("unload", "-w", path)
		_ = runLaunchctl("bootout", "gui/"+uid(), path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("无法删除登录项 %s: %w", path, err)
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("无法创建 LaunchAgents 目录: %w", err)
	}
	content := plistContent(label, execPath)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("无法写入登录项 %s: %w", path, err)
	}
	// Modern macOS prefers bootstrap; older releases only have load -w.
	if err := runLaunchctl("bootstrap", "gui/"+uid(), path); err != nil {
		if lerr := runLaunchctl("load", "-w", path); lerr != nil {
			return fmt.Errorf("无法加载登录项: %v", lerr)
		}
	}
	return nil
}

func runLaunchctl(args ...string) error {
	cmd := exec.Command(launchctlPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

func uid() string { return fmt.Sprint(os.Getuid()) }

// plistContent builds the LaunchAgent. LimitLoadToSessionType keeps it out of
// ssh/background sessions, and KeepAlive stays off so quitting from the menu
// bar does not restart the app immediately.
func plistContent(label, execPath string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + xmlEscape(label) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(execPath) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
	<key>LimitLoadToSessionType</key>
	<string>Aqua</string>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
