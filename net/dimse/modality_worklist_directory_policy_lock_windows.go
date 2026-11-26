//go:build windows

package dimse

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func acquireMWLReadLock(ctx context.Context, file *os.File) (func(), error) {
	var overlapped windows.Overlapped
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 0xffffffff, 0xffffffff, &overlapped)
		if err == nil {
			return func() { _ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 0xffffffff, 0xffffffff, &overlapped) }, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, err
		}
		if err = mwlLockPoll(ctx); err != nil {
			return nil, err
		}
	}
}
