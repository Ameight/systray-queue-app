package app

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"

	"github.com/Ameight/systray-queue-app/internal/hotkeys"
	"github.com/Ameight/systray-queue-app/internal/manage"
	"github.com/Ameight/systray-queue-app/internal/queue"
	"github.com/Ameight/systray-queue-app/internal/ui"
	"github.com/Ameight/systray-queue-app/internal/util"
)

var favicon []byte

func Run(faviconData []byte) {
	favicon = faviconData
	runtime.LockOSThread()
	systray.Run(onReady, onExit)
}

var (
	q      *queue.TaskQueue
	mgr    *manage.Server
	hkRegs []hotkeys.Registered
)

// ── Timer state ───────────────────────────────────────────────────────────────

var (
	timerMu       sync.Mutex
	timerActive   bool
	timerPaused   bool
	timerEnd      time.Time
	timerSaved    time.Duration
	timerDuration = 25 * time.Minute
)

func timerToggle() {
	timerMu.Lock()
	defer timerMu.Unlock()
	if !timerActive {
		timerActive = true
		timerPaused = false
		timerEnd = time.Now().Add(timerDuration)
		return
	}
	if timerPaused {
		timerEnd = time.Now().Add(timerSaved)
		timerPaused = false
		return
	}
	timerSaved = time.Until(timerEnd)
	if timerSaved < 0 {
		timerSaved = 0
	}
	timerPaused = true
}

func timerStop() {
	timerMu.Lock()
	defer timerMu.Unlock()
	timerActive = false
	timerPaused = false
}

func timerSnapshot() (active, paused bool, remain time.Duration) {
	timerMu.Lock()
	defer timerMu.Unlock()
	active = timerActive
	paused = timerPaused
	if active {
		if paused {
			remain = timerSaved
		} else {
			remain = time.Until(timerEnd)
			if remain < 0 {
				remain = 0
			}
		}
	}
	return
}

// ── Formatting ────────────────────────────────────────────────────────────────

func fmtCountdown(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

func fmtElapsed(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "< 1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func taskPreview(text string) string {
	line := text
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)
	runes := []rune(line)
	if len(runes) > 45 {
		return string(runes[:45]) + "…"
	}
	return line
}

// ── OS notification ───────────────────────────────────────────────────────────

func sendNotification(title, body string) {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display notification %q with title %q sound name "Glass"`, body, title)
		_ = exec.Command("osascript", "-e", script).Run()
	}
}

// ── App ───────────────────────────────────────────────────────────────────────

func onReady() {
	dataDir, err := util.AppDataDir()
	if err != nil {
		log.Printf("appDataDir: %v", err)
		systray.Quit()
		return
	}

	q, err = queue.NewTaskQueue(dataDir)
	if err != nil {
		log.Printf("queue init: %v", err)
		systray.Quit()
		return
	}

	mgr = manage.New(q, dataDir, favicon)

	systray.SetIcon(ui.MakeTemplateIcon())
	systray.SetTitle("Queue")
	systray.SetTooltip("Queue")

	// ── Load config early (needed for menu order + timer duration) ────────
	cfg, cfgPath, cfgErr := hotkeys.LoadOrCreate(dataDir)
	timerDuration = cfg.TimerDuration()

	// ── Build menu in configured group order ──────────────────────────────
	//
	// Groups: "task" | "timer" | "actions" | "navigation" | "system"
	//
	// Items are created in the order specified by cfg.EffectiveTrayGroups(),
	// with separators between groups. Visibility is applied immediately.

	var (
		mTaskTitle   *systray.MenuItem
		mTimer       *systray.MenuItem
		mSkip        *systray.MenuItem
		mDone        *systray.MenuItem
		mAddQuick    *systray.MenuItem
		mAddAdvanced *systray.MenuItem
		mView        *systray.MenuItem
		mManage      *systray.MenuItem
		mSettings    *systray.MenuItem
		mQuit        *systray.MenuItem
	)

	// groupItems maps group ID → items in that group (for live visibility toggle).
	groupItems := map[string][]*systray.MenuItem{}

	groups := cfg.EffectiveTrayGroups()
	for i, g := range groups {
		if i > 0 {
			systray.AddSeparator()
		}
		var items []*systray.MenuItem
		switch g.ID {
		case "task":
			mTaskTitle = systray.AddMenuItem("No tasks", "Click to view current task")
			items = []*systray.MenuItem{mTaskTitle}
		case "timer":
			mTimer = systray.AddMenuItem("Start timer", "Start a focus timer")
			items = []*systray.MenuItem{mTimer}
		case "actions":
			mSkip = systray.AddMenuItem("Skip", "Move current task to the end")
			mDone = systray.AddMenuItem("Done", "Complete current task")
			items = []*systray.MenuItem{mSkip, mDone}
		case "navigation":
			mAddQuick = systray.AddMenuItem("Add task…", "Quick add")
			mAddAdvanced = systray.AddMenuItem("Add task (advanced)…", "Open advanced editor in browser")
			mView = systray.AddMenuItem("View current task…", "Open current task in browser")
			mManage = systray.AddMenuItem("Manage order…", "Reorder tasks")
			items = []*systray.MenuItem{mAddQuick, mAddAdvanced, mView, mManage}
		case "system":
			mSettings = systray.AddMenuItem("Settings…", "Configure hotkeys")
			mQuit = systray.AddMenuItem("Quit", "Quit")
			items = []*systray.MenuItem{mSettings, mQuit}
		}
		groupItems[g.ID] = items
		if !g.Visible {
			for _, item := range items {
				if item != nil {
					item.Hide()
				}
			}
		}
	}

	// applyVisibility applies show/hide for all groups from a fresh config.
	applyVisibility := func(newGroups []hotkeys.TrayGroupConfig) {
		for _, g := range newGroups {
			items := groupItems[g.ID]
			for _, item := range items {
				if item == nil {
					continue
				}
				if g.Visible {
					item.Show()
				} else {
					item.Hide()
				}
			}
		}
	}

	// ── refreshAll updates all dynamic tray content ───────────────────────

	refreshAll := func() {
		count := len(q.GetAll())
		task, hasTask := q.Peek()
		active, paused, remain := timerSnapshot()

		// Task title item
		if mTaskTitle != nil {
			if hasTask {
				mTaskTitle.SetTitle(taskPreview(task.Text))
				mTaskTitle.Enable()
			} else {
				mTaskTitle.SetTitle("No tasks")
				mTaskTitle.Disable()
			}
		}
		if mSkip != nil {
			if hasTask {
				mSkip.Enable()
			} else {
				mSkip.Disable()
			}
		}
		if mDone != nil {
			if hasTask {
				mDone.Enable()
			} else {
				mDone.Disable()
			}
		}

		// Timer item label
		if mTimer != nil {
			if hasTask {
				mTimer.Enable()
			} else {
				mTimer.Disable()
			}
			switch {
			case active && paused:
				mTimer.SetTitle("▶ Resume  " + fmtCountdown(remain))
			case active:
				mTimer.SetTitle("⏸ Pause  " + fmtCountdown(remain))
			default:
				mTimer.SetTitle("Start timer")
			}
		}

		// Menubar title & tooltip
		var titleStr string
		if active {
			titleStr = fmtCountdown(remain)
		}
		if hasTask && !task.StartedAt.IsZero() {
			elapsed := fmtElapsed(time.Since(task.StartedAt))
			if active {
				titleStr += "  " + elapsed
			} else {
				titleStr = elapsed
			}
			systray.SetTooltip(fmt.Sprintf("Tasks: %d · %s on current task", count, elapsed))
		} else {
			if !active {
				titleStr = "Queue"
			}
			systray.SetTooltip(fmt.Sprintf("Tasks: %d", count))
		}
		systray.SetTitle(titleStr)
	}
	refreshAll()

	// ── Quick add ─────────────────────────────────────────────────────────

	quickAdd := func() {
		text, ok, err := ui.QuickAddText()
		if err != nil {
			ui.Error("Add task", err.Error())
			return
		}
		if !ok {
			return
		}
		t := queue.Task{
			ID:        fmt.Sprintf("%d", timeNowNano()),
			Text:      text,
			CreatedAt: timeNow(),
		}
		if err := q.Enqueue(t); err != nil {
			ui.Error("Add task", err.Error())
			return
		}
		refreshAll()
	}

	// ── Hotkeys ───────────────────────────────────────────────────────────

	actions := map[string]func(){
		hotkeys.ActionShowFirst:        func() { _ = openURL("/view") },
		hotkeys.ActionAddQuick:         quickAdd,
		hotkeys.ActionManageQueue:      func() { _ = openURL("/") },
		hotkeys.ActionAddFromClipboard: func() { _ = openURL("/add") },
		hotkeys.ActionSkip:             func() { _ = q.Skip(); refreshAll() },
		hotkeys.ActionComplete:         func() { _, _ = q.Complete(); timerStop(); refreshAll() },
	}

	type menuItem struct {
		item   *systray.MenuItem
		base   string
		action string
	}
	hotkeyMenuItems := []menuItem{}
	if mView != nil {
		hotkeyMenuItems = append(hotkeyMenuItems, menuItem{mView, "Open current task in browser", hotkeys.ActionShowFirst})
	}
	if mAddQuick != nil {
		hotkeyMenuItems = append(hotkeyMenuItems, menuItem{mAddQuick, "Quick add", hotkeys.ActionAddQuick})
	}
	if mAddAdvanced != nil {
		hotkeyMenuItems = append(hotkeyMenuItems, menuItem{mAddAdvanced, "Open advanced editor in browser", hotkeys.ActionAddFromClipboard})
	}
	if mSkip != nil {
		hotkeyMenuItems = append(hotkeyMenuItems, menuItem{mSkip, "Move current task to the end", hotkeys.ActionSkip})
	}
	if mDone != nil {
		hotkeyMenuItems = append(hotkeyMenuItems, menuItem{mDone, "Complete current task", hotkeys.ActionComplete})
	}
	if mManage != nil {
		hotkeyMenuItems = append(hotkeyMenuItems, menuItem{mManage, "Reorder tasks", hotkeys.ActionManageQueue})
	}

	applyTooltips := func(c hotkeys.KeyConfig) {
		for _, m := range hotkeyMenuItems {
			tooltip := m.base
			if hc, ok := c.Hotkeys[m.action]; ok && hc.Enabled && hc.Combo != "" {
				tooltip += "  " + hotkeys.FormatCombo(hc.Combo)
			}
			m.item.SetTooltip(tooltip)
		}
	}

	if cfgErr != nil {
		ui.Error("Hotkeys", cfgErr.Error())
	} else {
		applyTooltips(cfg)
		hkRegs, err = hotkeys.Register(cfg, actions)
		if err != nil {
			ui.Error("Hotkeys", err.Error()+"\nConfig: "+cfgPath)
		}
	}

	mgr.SetReloadFn(func() error {
		hotkeys.Unregister(hkRegs)
		newCfg, _, err := hotkeys.LoadOrCreate(dataDir)
		if err != nil {
			return err
		}
		newRegs, err := hotkeys.Register(newCfg, actions)
		if err != nil {
			return err
		}
		hkRegs = newRegs
		applyTooltips(newCfg)
		applyVisibility(newCfg.EffectiveTrayGroups())
		timerMu.Lock()
		timerDuration = newCfg.TimerDuration()
		timerMu.Unlock()
		return nil
	})

	// ── Ticker ────────────────────────────────────────────────────────────

	stopTicker := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var expired bool
				timerMu.Lock()
				if timerActive && !timerPaused && time.Now().After(timerEnd) {
					timerActive = false
					expired = true
				}
				timerMu.Unlock()
				if expired {
					sendNotification("Queue Timer", "Время вышло! Сделай перерыв.")
				}
				refreshAll()
			case <-stopTicker:
				return
			}
		}
	}()

	// ── Menu event loop ───────────────────────────────────────────────────

	go func() {
		// Build channel cases dynamically so nil items are skipped.
		for {
			var cases []struct {
				ch   <-chan struct{}
				fn   func()
			}
			add := func(item *systray.MenuItem, fn func()) {
				if item != nil {
					cases = append(cases, struct {
						ch <-chan struct{}
						fn func()
					}{item.ClickedCh, fn})
				}
			}

			add(mTaskTitle, func() { _ = openURL("/view") })
			add(mTimer, func() { timerToggle(); refreshAll() })
			add(mSkip, func() { _ = q.Skip(); refreshAll() })
			add(mDone, func() { _, _ = q.Complete(); timerStop(); refreshAll() })
			add(mAddQuick, quickAdd)
			add(mAddAdvanced, func() { _ = openURL("/add") })
			add(mView, func() { _ = openURL("/view") })
			add(mManage, func() { _ = openURL("/") })
			add(mSettings, func() { _ = openURL("/settings") })

			// select requires static cases — fall back to individual goroutines
			// for variable items, use a fixed select on known channels.
			break
		}

		// Fixed select — items may be nil but their ClickedCh will never fire
		// if the item was never created (nil pointer guard applied above).
		nilCh := make(chan struct{}) // never fires

		ch := func(item *systray.MenuItem) <-chan struct{} {
			if item == nil {
				return nilCh
			}
			return item.ClickedCh
		}

		for {
			select {
			case <-ch(mTaskTitle):
				_ = openURL("/view")
			case <-ch(mTimer):
				timerToggle()
				refreshAll()
			case <-ch(mSkip):
				_ = q.Skip()
				refreshAll()
			case <-ch(mDone):
				_, _ = q.Complete()
				timerStop()
				refreshAll()
			case <-ch(mAddQuick):
				quickAdd()
			case <-ch(mAddAdvanced):
				_ = openURL("/add")
			case <-ch(mView):
				_ = openURL("/view")
			case <-ch(mManage):
				_ = openURL("/")
			case <-ch(mSettings):
				_ = openURL("/settings")
			case <-ch(mQuit):
				close(stopTicker)
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	hotkeys.Unregister(hkRegs)
}

func timeNow() time.Time { return time.Now() }
func timeNowNano() int64 { return time.Now().UnixNano() }

func openURL(path string) error {
	base, err := mgr.URL()
	if err != nil {
		ui.Error("Manage UI", err.Error())
		return err
	}
	return manage.OpenBrowser(strings.TrimRight(base, "/") + path)
}
