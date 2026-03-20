//go:build windows

package hotkeys

import "golang.design/x/hotkey"

// letterKey maps a lowercase letter to a Windows virtual key code.
// On Windows, VK codes for letters are sequential ASCII uppercase (0x41–0x5A).
func letterKey(c byte) (hotkey.Key, bool) {
	if c < 'a' || c > 'z' {
		return 0, false
	}
	return hotkey.Key(c - 'a' + byte(hotkey.KeyA)), true
}
