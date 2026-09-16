//go:build linux

package compose

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"veil.local/node/internal/config"
	"veil.local/node/internal/platform/linux"
	"veil.local/node/internal/prepare"
)

func FileReader() config.FileReader { return linux.Reader{} }
func BundleWriter() prepare.Writer  { return linux.BundleWriter{} }
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
