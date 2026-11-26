//go:build !unix && !windows

package ul

import "syscall"

const (
	descriptorExhaustionErrno syscall.Errno = 0
	descriptorBuffersErrno    syscall.Errno = 0
)
