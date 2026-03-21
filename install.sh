#!/bin/bash
set -e

APP_NAME="Queue"
BUNDLE_NAME="${APP_NAME}.app"
EXE_NAME="systray-queue-app"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Install to ~/Applications (no admin rights needed)
INSTALL_DIR="$HOME/Applications"
APP_BUNDLE="$INSTALL_DIR/$BUNDLE_NAME"

echo "Installing $APP_NAME..."

# Check that the binary is next to this script
if [ ! -f "$SCRIPT_DIR/$EXE_NAME" ]; then
    echo "ERROR: $EXE_NAME not found next to install.sh"
    echo "Make sure both files are in the same folder."
    exit 1
fi

# Create ~/Applications if it doesn't exist
mkdir -p "$INSTALL_DIR"

# Remove old bundle if present
rm -rf "$APP_BUNDLE"

# Build .app bundle structure
mkdir -p "$APP_BUNDLE/Contents/MacOS"
mkdir -p "$APP_BUNDLE/Contents/Resources"

# Copy binary
cp "$SCRIPT_DIR/$EXE_NAME" "$APP_BUNDLE/Contents/MacOS/$EXE_NAME"
chmod +x "$APP_BUNDLE/Contents/MacOS/$EXE_NAME"

# Write Info.plist
cat > "$APP_BUNDLE/Contents/Info.plist" << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple Computer//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>Queue</string>
	<key>CFBundleDisplayName</key>
	<string>Queue</string>
	<key>CFBundleIdentifier</key>
	<string>com.example.systray-queue</string>
	<key>CFBundleVersion</key>
	<string>1.0</string>
	<key>CFBundleShortVersionString</key>
	<string>1.0</string>
	<key>CFBundleExecutable</key>
	<string>systray-queue-app</string>
	<key>LSMinimumSystemVersion</key>
	<string>10.13</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>NSMicrophoneUsageDescription</key>
	<string>Queue uses the microphone to record voice notes.</string>
</dict>
</plist>
EOF

echo ""
echo "Installed to: $APP_BUNDLE"
echo ""

# Ask to start now
read -r -p "Start the app now? [Y/n] " answer
answer="${answer:-Y}"
if [[ "$answer" =~ ^[Yy]$ ]]; then
    open "$APP_BUNDLE"
    echo "App started. Look for the icon in the menu bar."
fi

echo ""
echo "To enable autostart: open the app, go to Settings and check \"Autostart\"."
echo "To launch later: open ~/Applications/$BUNDLE_NAME"
echo ""
