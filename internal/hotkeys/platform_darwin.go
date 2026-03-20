//go:build darwin

package hotkeys

import "golang.design/x/hotkey"

// modSuper maps "cmd/win/super/meta" to the Command key on macOS.
const modSuper = hotkey.ModCmd
