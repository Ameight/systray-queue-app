//go:build linux

package hotkeys

import "golang.design/x/hotkey"

// modSuper maps "cmd/win/super/meta" to the Super key on Linux.
const modSuper = hotkey.ModCmd
