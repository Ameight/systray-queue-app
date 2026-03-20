package app

import (
	"fmt"
	"log"
	"runtime"
	"strings"
	"time"

	"github.com/getlantern/systray"

	"github.com/Ameight/systray-queue-app/internal/hotkeys"
	"github.com/Ameight/systray-queue-app/internal/manage"
	"github.com/Ameight/systray-queue-app/internal/queue"
	"github.com/Ameight/systray-queue-app/internal/ui"
	"github.com/Ameight/systray-queue-app/internal/util"
)

func Run() {
	runtime.LockOSThread()
	systray.Run(onReady, onExit)
}

var (
	q      *queue.TaskQueue
	mgr    *manage.Server
	hkRegs []hotkeys.Registered
)

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

	mgr = manage.New(q, dataDir)

	systray.SetIcon(ui.MakeTemplateIcon())
	systray.SetTitle("Queue")
	systray.SetTooltip("Queue")

	mAddQuick := systray.AddMenuItem("Add task…", "Quick add")
	mAddAdvanced := systray.AddMenuItem("Add task (advanced)…", "Open advanced editor in browser")
	mView := systray.AddMenuItem("View current task…", "Open current task in browser")
	mManage := systray.AddMenuItem("Manage order…", "Reorder tasks")
	systray.AddSeparator()
	mSkip := systray.AddMenuItem("Skip", "Move current task to the end")
	mDone := systray.AddMenuItem("Done", "Complete current task")
	systray.AddSeparator()
	mSettings := systray.AddMenuItem("Settings…", "Configure hotkeys")
	mQuit := systray.AddMenuItem("Quit", "Quit")

	updateTooltip := func() {
		count := len(q.GetAll())
		systray.SetTooltip(fmt.Sprintf("Tasks: %d", count))
	}
	updateTooltip()

	actions := map[string]func(){
		"show_first":         func() { _ = openURL("/view") },
		"manage_queue":       func() { _ = openURL("/") },
		"add_from_clipboard": func() { _ = openURL("/add") },
		"skip":               func() { _ = q.Skip(); updateTooltip() },
		"complete":           func() { _, _ = q.Complete(); updateTooltip() },
	}

	// menuHotkeys maps hotkey action → menu item tooltip base text.
	// Used to show the hotkey combo in the tray menu on hover.
	type menuItem struct {
		item    *systray.MenuItem
		base    string
		action  string
	}
	hotkeyMenuItems := []menuItem{
		{mView, "Open current task in browser", "show_first"},
		{mAddAdvanced, "Open advanced editor in browser", "add_from_clipboard"},
		{mSkip, "Move current task to the end", "skip"},
		{mDone, "Complete current task", "complete"},
		{mManage, "Reorder tasks", "manage_queue"},
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

	go func() {
		for {
			select {
			case <-mAddQuick.ClickedCh:
				text, ok, err := ui.QuickAddText()
				if err != nil {
					ui.Error("Add task", err.Error())
					continue
				}
				if !ok {
					continue
				}
				t := queue.Task{
					ID:        fmt.Sprintf("%d", timeNowNano()),
					Text:      text,
					CreatedAt: timeNow(),
				}
				if err := q.Enqueue(t); err != nil {
					ui.Error("Add task", err.Error())
				}
				updateTooltip()
			case <-mAddAdvanced.ClickedCh:
				_ = openURL("/add")
			case <-mView.ClickedCh:
				_ = openURL("/view")
			case <-mManage.ClickedCh:
				_ = openURL("/")
			case <-mSkip.ClickedCh:
				_ = q.Skip()
				updateTooltip()
			case <-mDone.ClickedCh:
				_, _ = q.Complete()
				updateTooltip()
			case <-mSettings.ClickedCh:
				_ = openURL("/settings")
			case <-mQuit.ClickedCh:
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
