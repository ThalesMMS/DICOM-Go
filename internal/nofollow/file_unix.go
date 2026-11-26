//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package nofollow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func OpenFile(path string) (*os.File, error) {
	return openAbsolute(path, false)
}

func OpenDirectory(path string) (*os.File, error) {
	return openAbsolute(path, true)
}

// maxSystemSymlinks bounds the root-owned system symlinks (such as macOS
// /var and /tmp) one traversal may follow.
const maxSystemSymlinks = 8

func openAbsolute(path string, directory bool) (*os.File, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return openResolved(absPath, absPath, directory, 0)
}

// openResolved walks absPath from "/" one component at a time without
// following symbolic links. The one exception is a directory component that
// is a system symlink (see systemSymlinkTarget): the walk restarts at its
// target, so paths under macOS /var and /tmp work while user-controlled
// symlinks anywhere on the path are still refused.
func openResolved(absPath, name string, directory bool, hops int) (*os.File, error) {
	relative := strings.TrimPrefix(filepath.Clean(absPath), string(filepath.Separator))
	if relative == "" || relative == "." {
		if !directory {
			return nil, errors.New("dicom nofollow: invalid transcode source")
		}
		fd, openErr := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return nil, openErr
		}
		return fileFromFD(fd, name)
	}

	currentFD, err := openTraversalRoot()
	if err != nil {
		return nil, err
	}
	defer func() {
		if currentFD >= 0 {
			_ = unix.Close(currentFD)
		}
	}()

	parts := strings.Split(relative, string(filepath.Separator))
	traversed := string(filepath.Separator)
	for index, part := range parts {
		last := index == len(parts)-1
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if !last || directory {
			flags |= unix.O_DIRECTORY
		} else {
			flags |= unix.O_NONBLOCK
		}
		nextFD, openErr := unix.Openat(currentFD, part, flags, 0)
		if openErr != nil {
			if (!last || directory) && hops < maxSystemSymlinks {
				if target, ok := systemSymlinkTarget(currentFD, part); ok {
					if !filepath.IsAbs(target) {
						target = filepath.Join(traversed, target)
					}
					rest := filepath.Join(append([]string{target}, parts[index+1:]...)...)
					return openResolved(rest, name, directory, hops+1)
				}
			}
			return nil, openErr
		}
		if closeErr := unix.Close(currentFD); closeErr != nil {
			_ = unix.Close(nextFD)
			return nil, closeErr
		}
		currentFD = nextFD
		traversed = filepath.Join(traversed, part)
	}
	file, err := fileFromFD(currentFD, name)
	if err != nil {
		return nil, err
	}
	currentFD = -1
	return file, nil
}

// systemSymlinkTarget returns the target of entry name in directory dirFD
// when it is a symbolic link that only the superuser can create or replace:
// the link is owned by root and sits in a root-owned directory that is not
// group- or world-writable. Such links are part of the operating system
// layout (macOS /var -> private/var, /tmp -> private/tmp), not something an
// unprivileged process can plant.
func systemSymlinkTarget(dirFD int, name string) (string, bool) {
	var link unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &link, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", false
	}
	if uint32(link.Mode)&unix.S_IFMT != unix.S_IFLNK || link.Uid != 0 {
		return "", false
	}
	var parent unix.Stat_t
	if err := unix.Fstat(dirFD, &parent); err != nil {
		return "", false
	}
	if parent.Uid != 0 || uint32(parent.Mode)&0o022 != 0 {
		return "", false
	}
	buffer := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(dirFD, name, buffer)
	if err != nil || n <= 0 || n >= len(buffer) {
		return "", false
	}
	return string(buffer[:n]), true
}

func fileFromFD(fd int, name string) (*os.File, error) {
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("dicom nofollow: create file from descriptor")
	}
	return file, nil
}

func CreateAt(parent *os.File, name string) (*os.File, error) {
	if parent == nil || !ValidName(name) {
		return nil, errors.New("dicom nofollow: invalid temporary entry")
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return fileFromFD(fd, name)
}

func OpenAt(parent *os.File, name string) (*os.File, error) {
	if parent == nil || !ValidName(name) {
		return nil, errors.New("dicom nofollow: invalid directory entry")
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return fileFromFD(fd, name)
}

func RemoveAt(parent *os.File, name string) error {
	if parent == nil || !ValidName(name) {
		return errors.New("dicom nofollow: invalid directory entry")
	}
	err := unix.Unlinkat(int(parent.Fd()), name, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func ValidName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}
