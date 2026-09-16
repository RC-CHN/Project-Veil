//go:build linux

package linux

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestBundlePrivatePublicationAndConcurrentRefusal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pair")
	files := map[string][]byte{"client/key.pem": []byte("test-material"), "prepared.json": []byte("{}")}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errors <- (BundleWriter{}).WriteNew(dir, files) }()
	}
	wg.Wait()
	close(errors)
	successes := 0
	for err := range errors {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("publication must have exactly one winner", successes)
	}
	for _, path := range []string{"", "client", "client/key.pem", "prepared.json"} {
		st, err := os.Stat(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0600)
		if st.IsDir() {
			want = 0700
		}
		if st.Mode().Perm() != want {
			t.Fatal("non-private bundle path", path, st.Mode())
		}
	}
}

func TestBundleRejectsSymlinksAndTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(dir, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "link"), filepath.Join(dir, "link", "pair")} {
		if err := (BundleWriter{}).WriteNew(path, map[string][]byte{"file": []byte("x")}); err == nil {
			t.Fatal("symlink accepted", path)
		}
	}
	if err := (BundleWriter{}).WriteNew(filepath.Join(dir, "pair"), map[string][]byte{"../outside": []byte("x")}); err == nil {
		t.Fatal("parent traversal accepted")
	}
}
