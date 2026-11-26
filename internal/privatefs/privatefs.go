// Package privatefs applies native privacy permissions to tool output and
// durable protocol state. It never accepts symbolic links or special files.
package privatefs

import (
	"fmt"
	"os"
)

func Check(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return os.ErrPermission
	}
	private, err := isPrivate(path, info)
	if err != nil {
		return err
	}
	if !private {
		return fmt.Errorf("private filesystem permissions: %w", os.ErrPermission)
	}
	return nil
}

// Secure protects an existing file or directory before sensitive data is written.
func Secure(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return os.ErrPermission
	}
	return protect(path)
}

// CreateDirectory protects a newly created directory. Existing directories must
// already satisfy the policy; opening a spool must never repair an unsafe spool.
func CreateDirectory(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		return Secure(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.ErrPermission
	}
	return Check(path)
}
