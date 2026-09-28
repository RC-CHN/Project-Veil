package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"veil-service/control"
	"veil-service/local"
	"veil/service"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is a thin desktop binding. All configuration and runtime state belong to
// the shared manager; the frontend never proxies traffic or spawns commands.
type App struct {
	manager *control.Manager
	lock    *os.File
	ctx     context.Context
	mu      sync.Mutex
	lang    string
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
	return &App{manager: m, lock: lock, lang: "en"}, nil
}

func (a *App) Request(q control.Request) control.Response {
	return a.manager.Handle(q)
}

func (a *App) SetLanguage(lang string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if lang == "zh" || lang == "en" {
		a.lang = lang
	}
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

func (a *App) ConfirmApply() (bool, error) {
	return a.confirm(a.text("Apply saved changes?", "应用已保存的更改？"),
		a.text("Active connections will close when the proxy restarts.", "代理将重新启动，当前连接会中断。"))
}

func (a *App) ConfirmDiscard() (bool, error) {
	return a.confirm(a.text("Discard unsaved changes?", "放弃未保存的更改？"),
		a.text("Your current edits will be replaced.", "当前编辑内容将被替换。"))
}

func (a *App) confirm(title, message string) (bool, error) {
	answer, err := wailsruntime.MessageDialog(a.ctx, wailsruntime.MessageDialogOptions{
		Type: wailsruntime.QuestionDialog, Title: title, Message: message,
		Buttons: []string{"No", "Yes"}, DefaultButton: "No", CancelButton: "No",
	})
	// Both supported Wails backends use system-localized Yes/No buttons and
	// return stable English identifiers, ignoring custom button labels.
	return answer == "Yes", err
}

func (a *App) beforeClose(context.Context) bool {
	if a.manager.Status().State != "running" {
		return false
	}
	ok, err := a.confirm(a.text("Quit Veil?", "退出 Veil？"),
		a.text("Quitting will stop the proxy and close active connections.", "退出后代理将停止，当前连接会中断。"))
	return err != nil || !ok
}

func (a *App) close() {
	a.manager.Close()
	a.lock.Close()
}
