package systemproxy

import (
	"errors"
	"runtime"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

type wininet struct{}

func Native() Backend        { return wininet{} }
func (wininet) Name() string { return "wininet" }

var (
	internet    = windows.NewLazySystemDLL("wininet.dll")
	queryOption = internet.NewProc("InternetQueryOptionW")
	setOption   = internet.NewProc("InternetSetOptionW")
	globalFree  = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalFree")
)

type connectionOption struct {
	ID    uint32
	Value uintptr
}
type connectionOptions struct {
	Size       uint32
	Connection *uint16
	Count      uint32
	Error      uint32
	Options    *connectionOption
}

func optionList(options []connectionOption) connectionOptions {
	return connectionOptions{Size: uint32(unsafe.Sizeof(connectionOptions{})), Count: uint32(len(options)), Options: &options[0]}
}

func (wininet) Read() (Snapshot, error) {
	// FLAGS_UI preserves the user's configured auto-detection choice, not its
	// current connection result. WinINet owns returned strings until GlobalFree.
	options := []connectionOption{{ID: 10}, {ID: 2}, {ID: 3}, {ID: 4}}
	list := optionList(options)
	size := list.Size
	ok, _, err := queryOption.Call(0, 75, uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&size)))
	if ok == 0 {
		return nil, err
	}
	defer func() {
		for _, o := range options[1:] {
			if o.Value != 0 {
				globalFree.Call(o.Value)
			}
		}
	}()
	s := Snapshot{"flags": strconv.FormatUint(uint64(options[0].Value), 10)}
	for i, key := range []string{"server", "bypass", "pac"} {
		// Interpret the native union in place; do not round-trip an allocated
		// pointer through Go uintptr arithmetic.
		text := *(**uint16)(unsafe.Pointer(&options[i+1].Value))
		s[key] = windows.UTF16PtrToString(text)
	}
	return s, nil
}
func (wininet) Write(s Snapshot) error {
	flags, err := strconv.ParseUint(s["flags"], 10, 32)
	if err != nil {
		return err
	}
	options := []connectionOption{{ID: 1, Value: uintptr(flags)}, {ID: 2}, {ID: 3}, {ID: 4}}
	strings := make([]*uint16, 3)
	for i, key := range []string{"server", "bypass", "pac"} {
		value, ok := s[key]
		if !ok {
			return errors.New("incomplete Windows proxy snapshot")
		}
		strings[i], err = windows.UTF16PtrFromString(value)
		if err != nil {
			return err
		}
		options[i+1].Value = uintptr(unsafe.Pointer(strings[i]))
	}
	list := optionList(options)
	ok, _, err := setOption.Call(0, 75, uintptr(unsafe.Pointer(&list)), uintptr(list.Size))
	runtime.KeepAlive(strings)
	runtime.KeepAlive(options)
	if ok == 0 {
		return err
	}
	// Broadcast the change to WinINet clients and refresh their cached settings.
	for _, option := range []uintptr{39, 37} {
		if ok, _, err = setOption.Call(0, option, 0, 0); ok == 0 {
			return err
		}
	}
	return nil
}
func (wininet) Manual(s Snapshot, endpoint string) Snapshot {
	s["flags"] = "3" // DIRECT | PROXY; PAC/WPAD stay disabled while Veil owns it.
	s["server"] = "http=" + endpoint + ";https=" + endpoint
	s["pac"] = ""
	return s
}
func (wininet) Direct(s Snapshot) Snapshot {
	s["flags"] = "1"
	s["server"] = ""
	s["pac"] = ""
	return s
}
