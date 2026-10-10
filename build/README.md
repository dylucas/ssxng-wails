# Build Directory

Wails 的构建资产目录。本项目只针对 macOS（Apple Silicon）。

目录结构：

* `bin` — 构建输出（`SSXNG.app`），由 `make build` / `wails build` 生成，已被 `.gitignore` 忽略。
* `darwin` — macOS 专用 plist。
* `appicon.png` — 应用图标源文件。

## macOS

`darwin` 目录存放 macOS 构建用的 plist，可按需修改；删除后再次 `wails build`
会回到 Wails 的默认模板。包含：

- `Info.plist` — `wails build`（正式构建）使用的主 plist。
- `Info.dev.plist` — `wails dev` 使用的 plist。

## 说明

本项目没有 `build/windows/` 目录，也不产出 Windows 安装包，因此不再保留
Wails 模板里关于 `icon.ico` / `installer/*` / `info.json` / `wails.exe.manifest`
的说明。
