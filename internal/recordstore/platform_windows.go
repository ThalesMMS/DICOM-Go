//go:build windows

package recordstore

import (
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"golang.org/x/sys/windows"
	"os"
)

func lockFile(f *os.File) error {
	var overlap windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap)
}

// Windows file Sync and atomic rename do not provide Unix directory durability.
func syncDirectory(*os.File) error { return nil }
func publish(parent, temp *os.File, name, key string, replace bool) (bool, error) {
	err := nofollow.RenameAt(parent, temp, key, replace)
	return err == nil, err
}
