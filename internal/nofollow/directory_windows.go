//go:build windows

package nofollow

import (
	"errors"
	"os"
)

func OpenDirectoryAt(parent *os.File, name string, create bool) (*os.File, bool, error) {
	if parent == nil || !ValidName(name) {
		return nil, false, os.ErrInvalid
	}
	if create {
		f, err := openWindowsRelative(parent, name, true, true, false)
		if err == nil {
			return f, true, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, false, err
		}
	}
	f, err := openWindowsRelative(parent, name, true, false, false)
	return f, false, err
}

func PublishClosedBetweenAt(sourceParent, destinationParent *os.File, source, destination string, expected os.FileInfo) (bool, error) {
	if sourceParent == nil || destinationParent == nil || !ValidName(source) || !ValidName(destination) || expected == nil {
		return false, os.ErrInvalid
	}
	f, err := openWindowsRelative(sourceParent, source, false, false, true)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, expected) {
		return false, os.ErrInvalid
	}
	if err := RenameAt(destinationParent, f, destination, false); err != nil {
		return false, err
	}
	return true, f.Close()
}
