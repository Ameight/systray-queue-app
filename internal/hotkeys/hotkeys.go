package hotkeys

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.design/x/hotkey"
	"gopkg.in/yaml.v3"

	"github.com/Ameight/systray-queue-app/internal/util"
)

type HotkeyConfig struct {
	Enabled bool   `yaml:"enabled"`
	Combo   string `yaml:"combo"`
}

type KeyConfig struct {
	Version int                     `yaml:"version"`
	Hotkeys map[string]HotkeyConfig `yaml:"hotkeys"`
}

type Registered struct {
	Action string
	HK     *hotkey.Hotkey
}

func defaultKeyConfig() KeyConfig {
	return KeyConfig{
		Version: 1,
		Hotkeys: map[string]HotkeyConfig{
			"show_first":         {Enabled: true, Combo: "ctrl+alt+q"},
			"add_from_clipboard": {Enabled: true, Combo: "ctrl+alt+a"},
			"skip":               {Enabled: true, Combo: "ctrl+alt+s"},
			"complete":           {Enabled: true, Combo: "ctrl+alt+d"},
			"manage_queue":       {Enabled: true, Combo: "ctrl+alt+m"},
		},
	}
}

// LoadOrCreate loads the key config from dataDir, creating a default one if missing.
// Returns the config, config file path, and any error.
func LoadOrCreate(dataDir string) (KeyConfig, string, error) {
	path := filepath.Join(dataDir, "key-config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := defaultKeyConfig()
			out, _ := yaml.Marshal(cfg)
			if err := util.AtomicWriteFile(path, out, 0644); err != nil {
				return KeyConfig{}, path, err
			}
			return cfg, path, nil
		}
		return KeyConfig{}, path, err
	}

	var cfg KeyConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return KeyConfig{}, path, fmt.Errorf("failed to parse key-config.yaml: %w", err)
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if cfg.Hotkeys == nil {
		cfg.Hotkeys = map[string]HotkeyConfig{}
	}
	return cfg, path, nil
}

// Register registers global hotkeys and starts goroutines to dispatch actions.
func Register(cfg KeyConfig, actionFn map[string]func()) ([]Registered, error) {
	var regs []Registered

	for action, hc := range cfg.Hotkeys {
		if !hc.Enabled {
			continue
		}
		fn, ok := actionFn[action]
		if !ok {
			continue
		}

		mods, key, err := parseHotkeyCombo(hc.Combo)
		if err != nil {
			return nil, fmt.Errorf("hotkey %s (%q): %w", action, hc.Combo, err)
		}

		hk := hotkey.New(mods, key)
		if err := hk.Register(); err != nil {
			return nil, fmt.Errorf("failed to register hotkey %s (%q): %w", action, hc.Combo, err)
		}

		regs = append(regs, Registered{Action: action, HK: hk})

		go func(fn func(), hk *hotkey.Hotkey) {
			for range hk.Keydown() {
				fn()
			}
		}(fn, hk)
	}

	return regs, nil
}

// Unregister unregisters all registered hotkeys.
func Unregister(regs []Registered) {
	for _, r := range regs {
		_ = r.HK.Unregister()
	}
}

func parseHotkeyCombo(combo string) ([]hotkey.Modifier, hotkey.Key, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(combo)), "+")
	if len(parts) == 0 {
		return nil, 0, fmt.Errorf("empty combo")
	}

	var mods []hotkey.Modifier
	keyToken := strings.TrimSpace(parts[len(parts)-1])
	modTokens := parts[:len(parts)-1]

	for _, mt := range modTokens {
		mt = strings.TrimSpace(mt)
		switch mt {
		case "ctrl", "control":
			mods = append(mods, hotkey.ModCtrl)
		case "alt", "option":
			mods = append(mods, modAlt)
		case "shift":
			mods = append(mods, hotkey.ModShift)
		case "cmd", "command", "meta", "super", "win":
			mods = append(mods, modSuper)
		default:
			return nil, 0, fmt.Errorf("unknown modifier: %s", mt)
		}
	}

	if k, ok := parseKeyToken(keyToken); ok {
		return mods, k, nil
	}
	return nil, 0, fmt.Errorf("unknown key: %s", keyToken)
}

func parseKeyToken(t string) (hotkey.Key, bool) {
	if len(t) == 1 && t[0] >= 'a' && t[0] <= 'z' {
		return hotkey.Key(t[0] - 'a' + byte(hotkey.KeyA)), true
	}
	if len(t) == 1 && t[0] >= '0' && t[0] <= '9' {
		switch t[0] {
		case '0':
			return hotkey.Key0, true
		case '1':
			return hotkey.Key1, true
		case '2':
			return hotkey.Key2, true
		case '3':
			return hotkey.Key3, true
		case '4':
			return hotkey.Key4, true
		case '5':
			return hotkey.Key5, true
		case '6':
			return hotkey.Key6, true
		case '7':
			return hotkey.Key7, true
		case '8':
			return hotkey.Key8, true
		case '9':
			return hotkey.Key9, true
		}
	}

	switch t {
	case "space":
		return hotkey.KeySpace, true
	case "enter", "return":
		return hotkey.KeyReturn, true
	case "tab":
		return hotkey.KeyTab, true
	case "esc", "escape":
		return hotkey.KeyEscape, true
	}

	if strings.HasPrefix(t, "f") {
		n, err := strconv.Atoi(strings.TrimPrefix(t, "f"))
		if err == nil {
			switch n {
			case 1:
				return hotkey.KeyF1, true
			case 2:
				return hotkey.KeyF2, true
			case 3:
				return hotkey.KeyF3, true
			case 4:
				return hotkey.KeyF4, true
			case 5:
				return hotkey.KeyF5, true
			case 6:
				return hotkey.KeyF6, true
			case 7:
				return hotkey.KeyF7, true
			case 8:
				return hotkey.KeyF8, true
			case 9:
				return hotkey.KeyF9, true
			case 10:
				return hotkey.KeyF10, true
			case 11:
				return hotkey.KeyF11, true
			case 12:
				return hotkey.KeyF12, true
			}
		}
	}

	return 0, false
}
