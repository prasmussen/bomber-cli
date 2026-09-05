package host

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	gossh "golang.org/x/crypto/ssh"
)

func hostKey(path string) ([]byte, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("host key must be a regular file")
		}
		if err = os.Chmod(path, 0600); err != nil {
			return nil, err
		}
		return os.ReadFile(path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	data, err := generateHostKey()
	if err != nil {
		return nil, err
	}
	return publishHostKey(path, data)
}

func generateHostKey() ([]byte, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := gossh.MarshalPrivateKey(private, "")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

func publishHostKey(path string, data []byte) ([]byte, error) {
	// Publish only a fully written key. Linking the temporary file is atomic and
	// never replaces a key published by another starting server.
	f, err := os.CreateTemp(filepath.Dir(path), ".host-key-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.Remove(f.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("remove temporary host key", "path", f.Name(), "error", err)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := os.Link(f.Name(), path); err != nil {
		if os.IsExist(err) {
			return hostKey(path)
		}
		return nil, err
	}
	return data, nil
}
