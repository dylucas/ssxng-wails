package main

import (
	"embed"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/src
var assets embed.FS

func main() {
	// go-shadowsocks2 keeps a per-process salt replay filter. That filter exists
	// so a *server* can reject a client that reuses a salt; for this client it
	// can only ever cost a spurious "repeated salt" failure (the filter's false
	// positive rate) and it would make the built-in self test fail, because that
	// test runs a Shadowsocks server inside this same process. Disable it unless
	// the user asked for something explicitly - we never override their setting.
	if os.Getenv("SHADOWSOCKS_SF_CAPACITY") == "" {
		_ = os.Setenv("SHADOWSOCKS_SF_CAPACITY", "-1")
	}

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "SSXNG",
		Width:     980,
		Height:    680,
		MinWidth:  880,
		MinHeight: 580,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 24, G: 26, B: 31, A: 1},
		OnStartup:        app.startup,
		OnDomReady:       app.domReady,
		OnBeforeClose:    app.beforeClose,
		OnShutdown:       app.shutdown,
		Mac: &mac.Options{
			TitleBar: mac.TitleBarHiddenInset(),
			About: &mac.AboutInfo{
				Title:   "SSXNG",
				Message: "Shadowsocks 客户端（Apple Silicon）\n基于 Wails + Go 构建\n协议实现来自 go-shadowsocks2（Apache-2.0）\nPAC 模板来自 ShadowsocksX-NG / gfwlist2pac（GPL-3.0）",
			},
		},
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
