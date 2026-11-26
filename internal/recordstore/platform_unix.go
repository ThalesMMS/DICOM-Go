//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package recordstore

import (
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"golang.org/x/sys/unix"
	"os"
)

func lockFile(f *os.File) error      { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func syncDirectory(f *os.File) error { return f.Sync() }
func publish(parent, temp *os.File, name, key string, replace bool) (bool, error) {
	if !replace {
		i, err := temp.Stat()
		if err != nil {
			return false, err
		}
		published, err := nofollow.PublishClosedBetweenAt(parent, parent, name, key, i)
		if err == nil {
			err = nofollow.RemoveAt(parent, name)
		}
		return published, err
	}
	err := unix.Renameat(int(parent.Fd()), name, int(parent.Fd()), key)
	return err == nil, err
}
