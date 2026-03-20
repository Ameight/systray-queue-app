//go:build darwin

package hotkeys

import "golang.design/x/hotkey"

// letterKey maps a lowercase letter to the macOS Carbon key code.
// Carbon codes are based on physical keyboard position, NOT alphabetical order.
func letterKey(c byte) (hotkey.Key, bool) {
	switch c {
	case 'a':
		return hotkey.KeyA, true
	case 'b':
		return hotkey.KeyB, true
	case 'c':
		return hotkey.KeyC, true
	case 'd':
		return hotkey.KeyD, true
	case 'e':
		return hotkey.KeyE, true
	case 'f':
		return hotkey.KeyF, true
	case 'g':
		return hotkey.KeyG, true
	case 'h':
		return hotkey.KeyH, true
	case 'i':
		return hotkey.KeyI, true
	case 'j':
		return hotkey.KeyJ, true
	case 'k':
		return hotkey.KeyK, true
	case 'l':
		return hotkey.KeyL, true
	case 'm':
		return hotkey.KeyM, true
	case 'n':
		return hotkey.KeyN, true
	case 'o':
		return hotkey.KeyO, true
	case 'p':
		return hotkey.KeyP, true
	case 'q':
		return hotkey.KeyQ, true
	case 'r':
		return hotkey.KeyR, true
	case 's':
		return hotkey.KeyS, true
	case 't':
		return hotkey.KeyT, true
	case 'u':
		return hotkey.KeyU, true
	case 'v':
		return hotkey.KeyV, true
	case 'w':
		return hotkey.KeyW, true
	case 'x':
		return hotkey.KeyX, true
	case 'y':
		return hotkey.KeyY, true
	case 'z':
		return hotkey.KeyZ, true
	}
	return 0, false
}
