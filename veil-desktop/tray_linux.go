package main

import (
	"context"
	_ "embed"
	"time"

	"github.com/godbus/dbus/v5"
)

//go:embed assets/tray.png
var trayIcon []byte

func traySupported() bool {
	_, err := dbus.SessionBus()
	return err == nil
}

func trayAvailable() bool {
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var value dbus.Variant
	err = conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher").CallWithContext(ctx,
		"org.freedesktop.DBus.Properties.Get", 0, "org.kde.StatusNotifierWatcher", "IsStatusNotifierHostRegistered").Store(&value)
	registered, _ := value.Value().(bool)
	return err == nil && registered
}
