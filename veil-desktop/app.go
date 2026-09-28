package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"veil-desktop/internal/systemproxy"
	"veil-service/control"
	"veil-service/local"
	"veil/service"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is a thin desktop binding. All configuration and runtime state belong to
// the shared manager; the frontend never proxies traffic or spawns commands.
type App struct {
	manager    *control.Manager
	lock       *os.File
	ctx        context.Context
	mu         sync.Mutex
	lang       string
	tray       *desktopTray
	trayOnce   sync.Once
	quitting   atomic.Bool
	opMu       sync.Mutex
	proxy      *systemproxy.Controller
	activeHTTP bool
}

func openApp(dir string) (*App, error) {
	if err := control.PrivateDir(dir); err != nil {
		return nil, err
	}
	lock, err := local.Lock(filepath.Join(dir, ".lock"))
	if err != nil {
		return nil, err
	}
	m, err := control.Open(dir)
	if err != nil {
		lock.Close()
		return nil, err
	}
	proxy, err := systemproxy.Open(filepath.Join(dir, "system-proxy.json"), systemproxy.Native())
	if err != nil {
		m.Close()
		lock.Close()
		return nil, err
	}
	return &App{manager: m, lock: lock, lang: "en", proxy: proxy}, nil
}

func (a *App) Request(q control.Request) control.Response {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	wasRunning := a.manager.Status().State == "running"
	r := a.manager.Handle(q)
	if r.Error == nil && (q.Action == "restart" || (q.Action == "start" && !wasRunning)) {
		config := a.manager.Handle(control.Request{Version: 1, Action: "config"})
		var cfg struct{ Role, Inbound, Target string }
		if json.Unmarshal(config.Config, &cfg) == nil {
			a.activeHTTP = cfg.Role == "client" && cfg.Target == "" && (cfg.Inbound == "http" || cfg.Inbound == "mixed")
		}
	}
	return a.syncSystemProxy(q, r)
}

func (a *App) SetLanguage(lang string) {
	a.mu.Lock()
	if lang == "zh" || lang == "en" {
		a.lang = lang
	}
	a.mu.Unlock()
	a.updateTrayLanguage()
}

func (a *App) text(en, zh string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lang == "zh" {
		return zh
	}
	return en
}

func (a *App) Import() (string, error) {
	path, err := wailsruntime.OpenFileDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title:   a.text("Import connection", "导入连接配置"),
		Filters: []wailsruntime.FileFilter{{DisplayName: "JSON", Pattern: "*.json"}},
	})
	if err != nil || path == "" {
		return "", err
	}
	return readProfile(path)
}

func readProfile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, service.MaxConfigSize+1))
	if err != nil {
		return "", err
	}
	if len(b) > service.MaxConfigSize {
		return "", errors.New("configuration exceeds 1 MiB")
	}
	return string(b), nil
}

// Quit is called only after the application-owned confirmation panel accepts.
func (a *App) Quit() error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if err := a.proxy.Restore(); err != nil && !errors.Is(err, systemproxy.ErrChanged) {
		return err
	}
	a.quitting.Store(true)
	wailsruntime.Quit(a.ctx)
	return nil
}

func (a *App) beforeClose(context.Context) bool {
	if a.quitting.Load() {
		return false
	}
	a.mu.Lock()
	t := a.tray
	a.mu.Unlock()
	if t != nil && t.ready.Load() && trayAvailable() {
		wailsruntime.WindowHide(a.ctx)
	} else {
		// A desktop without a tray keeps a taskbar entry to reopen the window.
		wailsruntime.WindowMinimise(a.ctx)
	}
	return true
}

func (a *App) close() {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.mu.Lock()
	t := a.tray
	a.mu.Unlock()
	t.stop()
	if err := a.proxy.Restore(); err != nil && !errors.Is(err, systemproxy.ErrChanged) {
		fmt.Fprintln(os.Stderr, "restore system proxy:", err)
	}
	a.manager.Close()
	a.lock.Close()
}
