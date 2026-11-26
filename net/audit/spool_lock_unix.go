//go:build !windows

package audit

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockSpool(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func unlockSpool(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
