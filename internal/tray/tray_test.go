package tray

import (
	"strings"
	"testing"
)

// The Objective-C side parses these specs, so the wire format is pinned here.
// One "key|title" line per mode, one "key|title|command" line per copy item.
func TestEncodeMenuSpecs(t *testing.T) {
	modes, cmds := EncodeMenuSpecs(
		[]ModeOption{{Key: "auto", Title: "自动代理"}, {Key: "global", Title: "全局模式"}},
		[]ProxyCommand{
			{Key: "http", Title: "复制 HTTP 代理命令", Command: "export http_proxy=http://127.0.0.1:1087;export https_proxy=http://127.0.0.1:1087;"},
			{Key: "socks", Title: "复制 SOCKS5 代理命令", Command: "export http_proxy=socks5://127.0.0.1:1086;export https_proxy=socks5://127.0.0.1:1086;"},
		},
	)

	if want := "auto|自动代理\nglobal|全局模式"; modes != want {
		t.Errorf("modesSpec = %q，期望 %q", modes, want)
	}
	lines := strings.Split(cmds, "\n")
	if len(lines) != 2 {
		t.Fatalf("commandsSpec 应有 2 行，实际 %d: %q", len(lines), cmds)
	}
	// AppKit splits on the first two separators and joins the remainder, so a
	// line must carry at least three fields and the command must come out whole.
	for i, line := range lines {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			t.Fatalf("第 %d 行字段不足 3 个: %q", i+1, line)
		}
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			t.Fatalf("第 %d 行有空字段: %q", i+1, line)
		}
	}
	if got := strings.SplitN(lines[0], "|", 3)[2]; got != "export http_proxy=http://127.0.0.1:1087;export https_proxy=http://127.0.0.1:1087;" {
		t.Errorf("HTTP 命令被破坏: %q", got)
	}
}

// A command carrying "|" must not be truncated: AppKit joins everything after
// the second separator back together.
func TestEncodeMenuSpecsKeepsPipesInsideCommand(t *testing.T) {
	_, cmds := EncodeMenuSpecs(nil, []ProxyCommand{
		{Key: "http", Title: "复制 HTTP 代理命令", Command: "a|b|c"},
	})
	if cmds != "http|复制 HTTP 代理命令|a|b|c" {
		t.Fatalf("commandsSpec = %q", cmds)
	}
}

// Empty entries are dropped rather than handed to AppKit as blank menu items,
// and an empty command list yields an empty spec (which omits the whole
// copy section from the menu).
func TestEncodeMenuSpecsSkipsEmptyEntries(t *testing.T) {
	modes, cmds := EncodeMenuSpecs(
		[]ModeOption{{Key: "  ", Title: "没有键"}, {Key: "auto", Title: "自动代理"}},
		[]ProxyCommand{{Key: "http", Title: "复制 HTTP 代理命令", Command: ""}},
	)
	if modes != "auto|自动代理" {
		t.Errorf("modesSpec = %q，期望只留下有键的模式", modes)
	}
	if cmds != "" {
		t.Errorf("commandsSpec = %q，期望空字符串", cmds)
	}
}
