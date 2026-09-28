package systemproxy

import (
	"os"
	"reflect"
	"testing"
)

// Opt in only on an isolated Windows user/session: this exercises real WinINet
// settings and restores the complete original snapshot even on assertion failure.
func TestWinINetSettings(t *testing.T) {
	if os.Getenv("VEIL_TEST_SYSTEM_PROXY") != "1" {
		t.Skip("set VEIL_TEST_SYSTEM_PROXY=1 in an isolated Windows session")
	}
	b := wininet{}
	original, err := b.Read()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Write(original); err != nil {
			t.Errorf("restore original proxy: %v", err)
		}
	})
	for _, want := range []Snapshot{
		b.Manual(clone(original), "127.0.0.1:1080"),
		b.Direct(clone(original)),
		original,
	} {
		if err := b.Write(want); err != nil {
			t.Fatal(err)
		}
		got, err := b.Read()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("WinINet settings did not round-trip")
		}
	}
}
