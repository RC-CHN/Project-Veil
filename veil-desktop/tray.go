package main

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// desktopTray owns only the notification icon. Proxy lifetime belongs to App.
type desktopTray struct {
	mu         sync.Mutex
	show, exit *systray.MenuItem
	ready      atomic.Bool
	done       chan struct{}
}

func (a *App) startTray(context.Context) {
	a.trayOnce.Do(a.initTray)
}

func (a *App) initTray() {
	if !traySupported() {
		return
	}
	t := &desktopTray{done: make(chan struct{})}
	a.mu.Lock()
	a.tray = t
	a.mu.Unlock()
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		systray.SetOnTapped(a.showWindow)
		systray.Run(func() {
			systray.SetIcon(trayIcon)
			systray.SetTitle("Veil")
			systray.SetTooltip("Veil")
			t.mu.Lock()
			t.show = systray.AddMenuItem("Open Veil", "")
			systray.AddSeparator()
			t.exit = systray.AddMenuItem("Exit Veil…", "")
			t.mu.Unlock()
			a.updateTrayLanguage()
			t.ready.Store(true)
			go func() {
				for {
					select {
					case <-t.show.ClickedCh:
						a.showWindow()
					case <-t.exit.ClickedCh:
						a.showWindow()
						wailsruntime.EventsEmit(a.ctx, "quit-requested")
					case <-t.done:
						return
					}
				}
			}()
		}, func() { close(t.done) })
	}()
}

func (a *App) showWindow() {
	wailsruntime.WindowUnminimise(a.ctx)
	wailsruntime.WindowShow(a.ctx)
}

func (a *App) updateTrayLanguage() {
	a.mu.Lock()
	t, lang := a.tray, a.lang
	a.mu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.show == nil {
		return
	}
	if lang == "zh" {
		t.show.SetTitle("打开 Veil")
		t.exit.SetTitle("退出 Veil…")
	} else {
		t.show.SetTitle("Open Veil")
		t.exit.SetTitle("Exit Veil…")
	}
}

func (t *desktopTray) stop() {
	if t == nil || !t.ready.Load() {
		return
	}
	systray.Quit()
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
	}
}
