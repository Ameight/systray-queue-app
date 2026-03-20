//go:build windows

package hotkeys

import "golang.design/x/hotkey"

// modSuper maps "cmd/win/super/meta" to the Windows key on Windows.
const modSuper = hotkey.ModWin
