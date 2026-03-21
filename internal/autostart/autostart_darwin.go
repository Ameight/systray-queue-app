package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "com.systray-queue-app.plist")
}

func IsEnabled() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}

func Enable(exePath string) error {
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.systray-queue-app</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
</dict>
</plist>
`, exePath)
	if err := os.MkdirAll(filepath.Dir(plistPath()), 0755); err != nil {
		return err
	}
	return os.WriteFile(plistPath(), []byte(content), 0644)
}

func Disable() error {
	err := os.Remove(plistPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
