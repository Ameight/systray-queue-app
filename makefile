export GOPROXY = https://proxy.golang.org

APP        = systray-queue-app
BUNDLE     = Queue.app
BUNDLE_ID  = com.ameight.queue
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    = -X github.com/Ameight/systray-queue-app/internal/updater.Version=$(VERSION)

.PHONY: build bundle dmg release run clean

# ── Build binary ──────────────────────────────────────────────────────────────

build:
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
	go build -ldflags "$(LDFLAGS)" -o $(APP) .

build-amd64:
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 \
	go build -ldflags "$(LDFLAGS)" -o $(APP)-amd64 .

build-universal: build build-amd64
	lipo -create -output $(APP)-universal $(APP) $(APP)-amd64
	rm $(APP)-amd64

# ── .app bundle ───────────────────────────────────────────────────────────────
# Creates Queue.app in the current directory.

bundle: build
	@echo "Building bundle: $(BUNDLE) ($(VERSION))"
	@rm -rf $(BUNDLE)
	@mkdir -p $(BUNDLE)/Contents/MacOS
	@mkdir -p $(BUNDLE)/Contents/Resources
	@cp $(APP) $(BUNDLE)/Contents/MacOS/$(APP)
	@chmod +x $(BUNDLE)/Contents/MacOS/$(APP)
	@sed \
		-e 's/__VERSION__/$(VERSION)/g' \
		-e 's/__BUNDLE_ID__/$(BUNDLE_ID)/g' \
		macos/Info.plist > $(BUNDLE)/Contents/Info.plist
	@echo "  → $(BUNDLE)/Contents/Info.plist (version=$(VERSION))"

# ── DMG ───────────────────────────────────────────────────────────────────────
# Creates a drag-to-install DMG: Queue-v1.0.0-arm64.dmg

DMG_NAME = Queue-$(VERSION)-$(shell go env GOARCH).dmg

dmg: bundle
	@echo "Creating DMG: $(DMG_NAME)"
	@rm -f $(DMG_NAME)
	@hdiutil create \
		-volname "Queue $(VERSION)" \
		-srcfolder $(BUNDLE) \
		-ov -format UDZO \
		$(DMG_NAME)
	@echo "  → $(DMG_NAME)"

# ── GitHub release assets ─────────────────────────────────────────────────────
# Produces the bare binary used by the auto-updater:
#   systray-queue-app-darwin-arm64
#   systray-queue-app-darwin-amd64

release-assets: build build-amd64
	@cp $(APP)       systray-queue-app-darwin-arm64
	@cp $(APP)-amd64 systray-queue-app-darwin-amd64
	@echo "Release assets ready:"
	@echo "  systray-queue-app-darwin-arm64"
	@echo "  systray-queue-app-darwin-amd64"

# ── Dev helpers ───────────────────────────────────────────────────────────────

run:
	go run .

clean:
	rm -f $(APP) $(APP)-amd64 $(APP)-universal
	rm -rf $(BUNDLE)
	rm -f *.dmg systray-queue-app-darwin-*
