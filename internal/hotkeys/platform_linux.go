//go:build linux

package hotkeys

import "golang.design/x/hotkey"

const (
	modAlt   = hotkey.Mod1 // Mod1 = Alt on most Linux setups
	modSuper = hotkey.Mod4 // Mod4 = Super/Win key on most Linux setups
)
