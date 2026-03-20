//go:build darwin

package hotkeys

import "golang.design/x/hotkey"

const (
	modAlt   = hotkey.ModOption // macOS uses Option, not Alt
	modSuper = hotkey.ModCmd
)
