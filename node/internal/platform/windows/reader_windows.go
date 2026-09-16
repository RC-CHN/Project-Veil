//go:build windows

package windows

import (
	"errors"
	win "golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"
)

type Reader struct{}

// The opened handle is checked and then read, avoiding a pathname re-open.
// ACLs outside the supported explicit user/SYSTEM/Administrators policy fail.
func (Reader) Read(path string, limit int64, private bool) ([]byte, error) {
	path, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	p, e := win.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	h, e := win.CreateFile(p, win.GENERIC_READ|win.READ_CONTROL, win.FILE_SHARE_READ, nil, win.OPEN_EXISTING, win.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	var info win.ByHandleFileInformation
	if e = win.GetFileInformationByHandle(h, &info); e != nil {
		return nil, e
	}
	if info.FileAttributes&(win.FILE_ATTRIBUTE_DIRECTORY|win.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || info.NumberOfLinks != 1 {
		return nil, errors.New("file type, reparse point or links")
	}
	sd, e := win.GetSecurityInfo(h, win.SE_FILE_OBJECT, win.OWNER_SECURITY_INFORMATION|win.DACL_SECURITY_INFORMATION)
	if e != nil {
		return nil, e
	}
	defer runtime.KeepAlive(sd)
	user, e := win.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return nil, e
	}
	allowed := func(s *win.SID) bool {
		return s != nil && (s.Equals(user.User.Sid) || s.IsWellKnown(win.WinLocalSystemSid) || s.IsWellKnown(win.WinBuiltinAdministratorsSid))
	}
	owner, _, e := sd.Owner()
	if e != nil || !allowed(owner) {
		return nil, errors.New("file owner")
	}
	acl, _, e := sd.DACL()
	if e != nil || acl == nil {
		return nil, errors.New("missing explicit file ACL")
	}
	for n := uint32(0); n < uint32(acl.AceCount); n++ {
		var ace *win.ACCESS_ALLOWED_ACE
		if e = win.GetAce(acl, n, &ace); e != nil {
			return nil, e
		}
		if ace.Header.AceFlags&win.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case win.ACCESS_DENIED_ACE_TYPE:
			continue
		case win.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*win.SID)(unsafe.Pointer(&ace.SidStart))
			if !allowed(sid) {
				// Public certificate/CA data may be readable, but not mutable by others.
				writeMask := win.ACCESS_MASK(win.GENERIC_ALL | win.GENERIC_WRITE | win.WRITE_DAC | win.WRITE_OWNER | win.DELETE | win.FILE_WRITE_DATA | win.FILE_APPEND_DATA | win.FILE_WRITE_ATTRIBUTES | win.FILE_WRITE_EA)
				if private || ace.Mask&writeMask != 0 {
					return nil, errors.New("file ACL grants access outside trusted identities")
				}
			}
		default:
			return nil, errors.New("unsupported file ACL entry")
		}
	}
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("file type or size")
	}
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(raw)) > limit {
		return nil, errors.New("file grew beyond limit")
	}
	return raw, e
}
