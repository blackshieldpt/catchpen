// Package fsutil holds the cross-process lock and durable atomic write used across curral.
package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes an exclusive flock on path (created if missing), blocking until it
// is available. The returned func releases it.
func Lock(path string) (func(), error) { return lock(path, syscall.LOCK_EX) }

// RLock takes a shared flock on path.
func RLock(path string) (func(), error) { return lock(path, syscall.LOCK_SH) }

// TryLock takes an exclusive flock without waiting; ok is false if it is held.
func TryLock(path string) (unlock func(), ok bool, err error) {
	u, err := lock(path, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, false, nil
	}
	return u, err == nil, err
}

func lock(path string, how int) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil // closing the only fd releases the flock
}

// WriteFileAtomic writes data to path via a unique temp file, fsync and rename,
// so readers never see a partial file and a crash leaves either version.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
