package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

func desktopPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "autostart", "systray-queue-app.desktop")
}

func IsEnabled() bool {
	_, err := os.Stat(desktopPath())
	return err == nil
}

func Enable(exePath string) error {
	content := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=systray-queue-app
Exec=%s
Hidden=false
NoDisplay=false
X-GNOME-Autostart-enabled=true
`, exePath)
	if err := os.MkdirAll(filepath.Dir(desktopPath()), 0755); err != nil {
		return err
	}
	return os.WriteFile(desktopPath(), []byte(content), 0644)
}

func Disable() error {
	err := os.Remove(desktopPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
