//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package dimse

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func acquireMWLReadLock(ctx context.Context, file *os.File) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return nil, err
		}
		if err = mwlLockPoll(ctx); err != nil {
			return nil, err
		}
	}
}
