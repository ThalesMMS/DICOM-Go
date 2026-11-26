//go:build !unix && !windows

package ul

import "syscall"

func acceptResourceErrno(syscall.Errno) bool { return false }
