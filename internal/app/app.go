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
	timerMu     sync.Mutex
	timerActive bool
	timerPaused bool
	timerEnd    time.Time
	timerSaved  time.Duration // remaining duration saved on pause
)

const timerDuration = 25 * time.Minute

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

// timerSnapshot returns a consistent snapshot of timer state under a single lock.
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

	// ── Menu layout ───────────────────────────────────────────────────────
	//  [current task title]          ← top section
	//  [Start timer / ⏸ Pause ...]
	//  [Skip]
	//  [Done]
	//  ────────────────
	//  Add task…
	//  Add task (advanced)…
	//  View current task…
	//  Manage order…
	//  ────────────────
	//  Settings…
	//  Quit

	mTaskTitle := systray.AddMenuItem("No tasks", "Click to view current task")
	systray.AddSeparator()
	mTimer := systray.AddMenuItem("Start timer", "Start a 25-minute focus timer")
	mSkip := systray.AddMenuItem("Skip", "Move current task to the end")
	mDone := systray.AddMenuItem("Done", "Complete current task")
	systray.AddSeparator()
	mAddQuick := systray.AddMenuItem("Add task…", "Quick add")
	mAddAdvanced := systray.AddMenuItem("Add task (advanced)…", "Open advanced editor in browser")
	mView := systray.AddMenuItem("View current task…", "Open current task in browser")
	mManage := systray.AddMenuItem("Manage order…", "Reorder tasks")
	systray.AddSeparator()
	mSettings := systray.AddMenuItem("Settings…", "Configure hotkeys")
	mQuit := systray.AddMenuItem("Quit", "Quit")

	// ── Refresh ───────────────────────────────────────────────────────────

	refreshAll := func() {
		count := len(q.GetAll())
		task, hasTask := q.Peek()
		active, paused, remain := timerSnapshot()

		// Task title item
		if hasTask {
			mTaskTitle.SetTitle(taskPreview(task.Text))
			mTaskTitle.Enable()
			mSkip.Enable()
			mDone.Enable()
			mTimer.Enable()
		} else {
			mTaskTitle.SetTitle("No tasks")
			mTaskTitle.Disable()
			mSkip.Disable()
			mDone.Disable()
			mTimer.Disable()
		}

		// Timer item label
		switch {
		case active && paused:
			mTimer.SetTitle("▶ Resume  " + fmtCountdown(remain))
		case active:
			mTimer.SetTitle("⏸ Pause  " + fmtCountdown(remain))
		default:
			mTimer.SetTitle("Start timer")
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
	hotkeyMenuItems := []menuItem{
		{mView, "Open current task in browser", hotkeys.ActionShowFirst},
		{mAddQuick, "Quick add", hotkeys.ActionAddQuick},
		{mAddAdvanced, "Open advanced editor in browser", hotkeys.ActionAddFromClipboard},
		{mSkip, "Move current task to the end", hotkeys.ActionSkip},
		{mDone, "Complete current task", hotkeys.ActionComplete},
		{mManage, "Reorder tasks", hotkeys.ActionManageQueue},
	}

	applyTooltips := func(cfg hotkeys.KeyConfig) {
		for _, m := range hotkeyMenuItems {
			tooltip := m.base
			if hc, ok := cfg.Hotkeys[m.action]; ok && hc.Enabled && hc.Combo != "" {
				tooltip += "  " + hotkeys.FormatCombo(hc.Combo)
			}
			m.item.SetTooltip(tooltip)
		}
	}

	cfg, cfgPath, err := hotkeys.LoadOrCreate(dataDir)
	if err != nil {
		ui.Error("Hotkeys", err.Error())
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
				// Check timer expiry
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
		for {
			select {
			case <-mTaskTitle.ClickedCh:
				_ = openURL("/view")
			case <-mTimer.ClickedCh:
				timerToggle()
				refreshAll()
			case <-mSkip.ClickedCh:
				_ = q.Skip()
				refreshAll()
			case <-mDone.ClickedCh:
				_, _ = q.Complete()
				timerStop()
				refreshAll()
			case <-mAddQuick.ClickedCh:
				quickAdd()
			case <-mAddAdvanced.ClickedCh:
				_ = openURL("/add")
			case <-mView.ClickedCh:
				_ = openURL("/view")
			case <-mManage.ClickedCh:
				_ = openURL("/")
			case <-mSettings.ClickedCh:
				_ = openURL("/settings")
			case <-mQuit.ClickedCh:
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
