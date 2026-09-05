package host

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func TestHostKeyConcurrentCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "host")
	const count = 16
	keys := make([][]byte, count)
	errs := make([]error, count)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			<-gate
			keys[i], errs[i] = hostKey(path)
		})
	}
	close(gate)
	wg.Wait()
	for i := range count {
		if errs[i] != nil {
			t.Fatalf("creator %d: %v", i, errs[i])
		}
		if !bytes.Equal(keys[0], keys[i]) {
			t.Fatal("concurrent starts returned different host keys")
		}
	}
	if _, err := gossh.ParsePrivateKey(keys[0]); err != nil {
		t.Fatal("published incomplete key:", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions: info=%v error=%v", info, err)
	}
	assertNoTemporaryKeys(t, filepath.Dir(path))
}

func assertNoTemporaryKeys(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".host-key-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary keys retained: %v, error=%v", files, err)
	}
}

func TestHostKeyFailurePreservesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host")
	original := []byte("invalid existing key")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.HostKey = path
	if h, err := New(cfg); err == nil {
		h.Hub.Close()
		t.Fatal("invalid existing key accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatal("failed startup replaced existing key")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, badPath := range []string{link, dir, filepath.Join(path, "child")} {
		if _, err := hostKey(badPath); err == nil {
			t.Fatalf("invalid key path accepted: %s", badPath)
		}
	}
	assertNoTemporaryKeys(t, dir)
}
