package main

import (
	"os"
	"path/filepath"
	"veil-service/control"
)

func prepareWebview(dir string) error {
	// Wails v2 uses WebKitGTK's default context. Give this process its own data
	// and cache roots so wails:// pages from unrelated apps never share storage.
	for key, name := range map[string]string{"XDG_DATA_HOME": "webview", "XDG_CACHE_HOME": "cache"} {
		path, err := filepath.Abs(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if err := control.PrivateDir(path); err != nil {
			return err
		}
		if err := os.Setenv(key, path); err != nil {
			return err
		}
	}
	return nil
}
