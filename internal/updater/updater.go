package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Version is injected at build time:
//
//	go build -ldflags "-X github.com/Ameight/systray-queue-app/internal/updater.Version=v1.0.0"
var Version = "dev"

const githubRepo = "Ameight/systray-queue-app"

// UpdateInfo describes an available release.
type UpdateInfo struct {
	Version     string
	DownloadURL string // direct binary URL, empty if no asset for this platform
	PageURL     string // GitHub release page
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	HTMLURL string        `json:"html_url"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Check fetches the latest release from GitHub.
// Returns (nil, nil) when already up-to-date or no releases exist yet.
func Check() (*UpdateInfo, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil // no releases yet
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API: HTTP %d", resp.StatusCode)
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	current := strings.TrimPrefix(Version, "v")
	if !semverGT(latest, current) {
		return nil, nil // up to date
	}

	assetName := fmt.Sprintf("systray-queue-app-darwin-%s", runtime.GOARCH)
	var dlURL string
	for _, a := range rel.Assets {
		if a.Name == assetName {
			dlURL = a.BrowserDownloadURL
			break
		}
	}
	return &UpdateInfo{
		Version:     rel.TagName,
		DownloadURL: dlURL,
		PageURL:     rel.HTMLURL,
	}, nil
}

// Install downloads the new binary, atomically replaces the current
// executable, and relaunches the .app bundle (or bare binary).
// The caller must call systray.Quit() / os.Exit(0) after this returns nil.
func Install(info *UpdateInfo) error {
	if info.DownloadURL == "" {
		return fmt.Errorf("no binary for darwin/%s — download manually: %s", runtime.GOARCH, info.PageURL)
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable path: %w", err)
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	// Download new binary to a temp file in the same directory.
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(info.DownloadURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}

	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".update-*.tmp")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("write: %w", err)
	}
	tmp.Close()

	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	// Atomic replace.
	if err := os.Rename(tmpName, exePath); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}

	// Relaunch: prefer 'open Bundle.app', fall back to direct exec.
	if bundle := findBundle(exePath); bundle != "" {
		_ = exec.Command("open", bundle).Start()
	} else {
		_ = exec.Command(exePath).Start()
	}
	return nil
}

// findBundle walks up from exePath looking for a *.app directory.
func findBundle(exePath string) string {
	dir := exePath
	for i := 0; i < 5; i++ {
		dir = filepath.Dir(dir)
		if strings.HasSuffix(dir, ".app") {
			return dir
		}
	}
	return ""
}

// ── Semver helpers ────────────────────────────────────────────────────────────

func semverGT(a, b string) bool {
	if a == "" || b == "dev" || a == b {
		return false
	}
	pa, pb := parseSemver(a), parseSemver(b)
	for i := range pa {
		if pa[i] > pb[i] {
			return true
		}
		if pa[i] < pb[i] {
			return false
		}
	}
	return false
}

func parseSemver(s string) [3]int {
	var v [3]int
	parts := strings.SplitN(s, ".", 3)
	for i, p := range parts {
		if i >= 3 {
			break
		}
		fmt.Sscanf(p, "%d", &v[i]) //nolint:errcheck
	}
	return v
}
