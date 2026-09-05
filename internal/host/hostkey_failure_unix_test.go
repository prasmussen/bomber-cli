//go:build darwin || linux

package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestHostKeyWriteFailure(t *testing.T) {
	const envKey = "BOMBER_TEST_KEY_WRITE_FAILURE"
	if path := os.Getenv(envKey); path != "" {
		// Force a real partial disk write in an isolated process. Do not change
		// resource limits or signal handling in the main test process.
		signal.Ignore(syscall.SIGXFSZ)
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1, Max: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := hostKey(path); !errors.Is(err, syscall.EFBIG) {
			t.Fatalf("expected file-size write failure, got %v", err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("failed write published a key: %v", err)
		}
		assertNoTemporaryKeys(t, filepath.Dir(path))
		return
	}
	path := filepath.Join(t.TempDir(), "host")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestHostKeyWriteFailure$")
	cmd.Env = append(os.Environ(), envKey+"="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write failure subprocess: %v\n%s", err, out)
	}
	if _, err := hostKey(path); err != nil {
		t.Fatal("could not retry after failed write:", err)
	}
	assertNoTemporaryKeys(t, filepath.Dir(path))
}
