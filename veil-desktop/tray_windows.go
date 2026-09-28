package main

import _ "embed"

//go:embed assets/tray.ico
var trayIcon []byte

func traySupported() bool { return true }
func trayAvailable() bool { return true }
