//go:build linux

package compose

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"veil.local/node/internal/config"
	"veil.local/node/internal/platform/linux"
)

func FileReader() config.FileReader { return linux.Reader{} }
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
