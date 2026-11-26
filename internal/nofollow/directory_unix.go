//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package nofollow

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

// OpenDirectoryAt opens one child without following links. When requested,
// creation is exclusive and private; a concurrent existing directory is opened.
func OpenDirectoryAt(parent *os.File, name string, create bool) (*os.File, bool, error) {
	if parent == nil || !ValidName(name) {
		return nil, false, os.ErrInvalid
	}
	created := false
	if create {
		err := unix.Mkdirat(int(parent.Fd()), name, 0o700)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, false, err
		}
		created = err == nil
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, created, err
	}
	f, err := fileFromFD(fd, name)
	return f, created, err
}

// PublishClosedBetweenAt publishes a complete closed temporary into an absent
// entry of another pinned directory on the same filesystem. It never copies.
func PublishClosedBetweenAt(sourceParent, destinationParent *os.File, source, destination string, expected os.FileInfo) (bool, error) {
	if sourceParent == nil || destinationParent == nil || !ValidName(source) || !ValidName(destination) || expected == nil {
		return false, os.ErrInvalid
	}
	f, err := OpenAt(sourceParent, source)
	if err != nil {
		return false, err
	}
	info, statErr := f.Stat()
	closeErr := f.Close()
	if statErr != nil || closeErr != nil {
		return false, errors.Join(statErr, closeErr)
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, expected) {
		return false, os.ErrInvalid
	}
	err = unix.Linkat(int(sourceParent.Fd()), source, int(destinationParent.Fd()), destination, 0)
	return err == nil, err
}
