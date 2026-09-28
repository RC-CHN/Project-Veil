package local

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"veil-service/internal/winsec"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func pipePath(path string) error {
	if !strings.HasPrefix(path, `\\.\pipe\`) || len(path) <= len(`\\.\pipe\`) || strings.ContainsAny(path[len(`\\.\pipe\`):], `/\`) {
		return errors.New(`control endpoint must be a local pipe: \\.\pipe\NAME`)
	}
	return nil
}

func Listen(path string) (net.Listener, error) {
	if err := pipePath(path); err != nil {
		return nil, err
	}
	sddl, err := winsec.Descriptor(false)
	if err != nil {
		return nil, err
	}
	// go-winio rejects remote clients and refuses to replace a live first pipe
	// instance. Byte mode preserves the existing newline-delimited protocol.
	return winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl})
}

func dial(ctx context.Context, path string) (net.Conn, error) {
	if err := pipePath(path); err != nil {
		return nil, err
	}
	c, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return nil, err
	}
	fd, ok := c.(interface{ Fd() uintptr })
	if !ok {
		c.Close()
		return nil, errors.New("pipe has no verifiable Windows handle")
	}
	if err := winsec.CheckHandle(windows.Handle(fd.Fd())); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Lock holds a nonshared handle. Windows releases it on close or process exit;
// no stale PID recovery is needed. The caller first verifies the private parent.
func Lock(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, ErrStateLocked
		}
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		f.Close()
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		f.Close()
		return nil, errors.New("state lock must not be a link")
	}
	return f, nil
}
