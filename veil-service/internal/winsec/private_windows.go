// Package winsec defines the current user's private Windows objects.
package winsec

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Descriptor grants access only to this process's user and LocalSystem. For
// directories, children inherit these entries instead of the parent's ACL.
func Descriptor(children bool) (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	flags := ""
	if children {
		flags = "OICI"
	}
	sid := u.User.Sid.String()
	return "O:" + sid + "D:P(A;" + flags + ";FA;;;" + sid + ")(A;" + flags + ";FA;;;SY)", nil
}

// Check refuses directories/files with foreign owners or access grants. Do not
// silently repair an existing shared directory that may belong to another app.
func Check(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return check(sd)
}

// CheckHandle authenticates the connected pipe before a client sends secrets.
// Checking the handle avoids a name lookup race with another pipe instance.
func CheckHandle(h windows.Handle) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return check(sd)
}

func check(sd *windows.SECURITY_DESCRIPTOR) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(u.User.Sid) {
		return errors.New("private state must be owned by the current user")
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if acl == nil || acl.AceCount == 0 {
		return errors.New("private state requires an explicit access list")
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		// Reject nonstandard ACEs conservatively rather than misinterpreting SID
		// offsets in object/callback entries.
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("private state has an unsupported access entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(u.User.Sid) && !sid.IsWellKnown(windows.WinLocalSystemSid) {
			return errors.New("private state grants access to another user or group")
		}
	}
	return nil
}
