package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrUpgradeLocked means another process (the daemon running an update, or a
// CLI restore) is changing the PayRam container right now.
var ErrUpgradeLocked = errors.New("another update or restore is changing PayRam right now")

// AcquireContainerLock takes the host-wide lock that every operation which
// stops, renames, replaces or restores the PayRam container must hold. It
// never blocks: a second caller gets ErrUpgradeLocked. The lock is released
// by the returned function, and by the kernel if the process dies.
func AcquireContainerLock(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}
	path := filepath.Join(stateDir, "container.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrUpgradeLocked
		}
		return nil, fmt.Errorf("failed to lock %s: %w", path, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
