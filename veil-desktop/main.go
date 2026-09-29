package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"veil-service/webui"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend
var assets embed.FS

//go:embed assets/veil.svg
var icon []byte

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		showStartupError(err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("state-dir", "", "private desktop state directory")
	showVersion := flag.Bool("version", false, "print release version")
	flag.Parse()
	if *showVersion {
		fmt.Println("Veil Desktop " + version)
		return nil
	}
	if *dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		*dir = filepath.Join(base, "Veil", "desktop")
	}
	absoluteDir, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	*dir = absoluteDir
	a, err := openApp(*dir)
	if err != nil {
		return err
	}
	defer a.close()
	if err := prepareWebview(*dir); err != nil {
		return err
	}
	return wails.Run(&options.App{
		Title: "Veil", Width: 1040, Height: 850, MinWidth: 640, MinHeight: 600,
		BackgroundColour: options.NewRGB(246, 248, 247),
		AssetServer:      &assetserver.Options{Assets: assets, Handler: webui.Handler()}, Bind: []interface{}{a},
		OnStartup:     func(ctx context.Context) { a.ctx = ctx },
		OnBeforeClose: a.beforeClose,
		OnDomReady:    a.startTray,
		Linux: &linux.Options{Icon: icon, ProgramName: "net.projectveil.Veil",
			WebviewGpuPolicy: linux.WebviewGpuPolicyNever},
		Windows: &windows.Options{WebviewUserDataPath: filepath.Join(*dir, "webview")},
	})
}
