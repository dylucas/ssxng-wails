// Package helperbin carries the privileged helper inside the app bundle so
// that a user never has to find it next to the app.
//
// scripts/build.sh builds cmd/ssxng-helper into bin/ssxng-helper *before*
// compiling the app, so the real binary is embedded into release builds. During
// development the file may be missing; Bytes then reports a clear error and the
// UI shows "助手未打包".
package helperbin

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
)

//go:embed all:bin
var dir embed.FS

// Filename is the embedded helper's base name.
const Filename = "ssxng-helper"

// Bytes returns the embedded helper binary.
func Bytes() ([]byte, error) {
	data, err := dir.ReadFile("bin/" + Filename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("助手未打包进应用，请重新运行 scripts/build.sh 构建")
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("内置助手为空，请重新运行 scripts/build.sh 构建")
	}
	return data, nil
}
