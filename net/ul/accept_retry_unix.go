//go:build unix

package ul

import "syscall"

// acceptResourceErrno reports whether an accept failed because the process or
// the system ran out of descriptors, buffers or memory.
func acceptResourceErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM:
		return true
	}
	return false
}
