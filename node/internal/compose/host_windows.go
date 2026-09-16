//go:build windows

package compose

import (
	"context"
	"os"
	"os/signal"
	"veil.local/node/internal/config"
	"veil.local/node/internal/platform/windows"
)

func FileReader() config.FileReader { return windows.Reader{} }
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
