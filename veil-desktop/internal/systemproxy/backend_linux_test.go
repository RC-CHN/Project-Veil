package systemproxy

import (
	"reflect"
	"testing"
)

func TestGNOMETransactionalSettings(t *testing.T) {
	// Use GLib's in-process memory backend: this test cannot alter the user's dconf database.
	t.Setenv("GSETTINGS_BACKEND", "memory")
	backend := gnome{}
	original, err := backend.Read()
	if err != nil {
		t.Skip(err)
	}
	manual := backend.Manual(clone(original), "127.0.0.1:18321")
	if err = backend.Write(manual); err != nil {
		t.Fatal(err)
	}
	got, err := backend.Read()
	if err != nil || !reflect.DeepEqual(got, manual) {
		t.Fatalf("manual readback: %#v %v", got, err)
	}
	incomplete := clone(original)
	delete(incomplete, "socks/port")
	if err = backend.Write(incomplete); err == nil {
		t.Fatal("incomplete snapshot accepted")
	}
	got, _ = backend.Read()
	if !reflect.DeepEqual(got, manual) {
		t.Fatal("staged partial update escaped before apply")
	}
	if err = backend.Write(original); err != nil {
		t.Fatal(err)
	}
	got, _ = backend.Read()
	if !reflect.DeepEqual(got, original) {
		t.Fatal("original settings not restored")
	}
}
