package jobs

import (
	"errors"
	"testing"
)

func TestAcquireContainerLock_SecondCallerIsRefusedUntilRelease(t *testing.T) {
	dir := t.TempDir()

	release, err := AcquireContainerLock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := AcquireContainerLock(dir); !errors.Is(err, ErrUpgradeLocked) {
		t.Fatalf("second lock while held: got %v, want ErrUpgradeLocked", err)
	}
	release()

	again, err := AcquireContainerLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}
