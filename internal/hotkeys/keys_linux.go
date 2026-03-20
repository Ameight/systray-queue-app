//go:build linux

package hotkeys

import "golang.design/x/hotkey"

// letterKey maps a lowercase letter to an X11 keysym.
// On Linux/X11, keysyms for letters match ASCII values (sequential).
func letterKey(c byte) (hotkey.Key, bool) {
	if c < 'a' || c > 'z' {
		return 0, false
	}
	return hotkey.Key(c - 'a' + byte(hotkey.KeyA)), true
}
