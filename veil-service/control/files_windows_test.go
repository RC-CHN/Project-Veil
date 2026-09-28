package control

import (
	"path/filepath"
	"testing"
	"veil-service/internal/winsec"

	"golang.org/x/sys/windows"
)

func checkConfigPrivate(t *testing.T, path string) {
	t.Helper()
	if err := winsec.Check(path); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryACLAndInheritedConfig(t *testing.T) {
	m, dir := manager(t)
	if err := PrivateDir(dir); err != nil {
		t.Fatalf("reopen private directory: %v", err)
	}
	perform(t, m, "save", profile(t, ""))
	checkConfigPrivate(t, filepath.Join(dir, "config.json"))
	// Replacements must preserve privacy and survive a new manager, not just
	// the initial CREATE_NEW path. Common lifecycle tests check both revisions.
	perform(t, m, "save", profile(t, "example.test:443"))
	checkConfigPrivate(t, filepath.Join(dir, "config.json"))
	for _, sddl := range []string{"D:P(A;OICI;FA;;;WD)", "D:NO_ACCESS_CONTROL"} {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
			t.Fatal(err)
		}
		if err := PrivateDir(dir); err == nil {
			t.Fatal("shared directory accepted: " + sddl)
		}
	}
}
