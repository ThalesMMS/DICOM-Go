//go:build unix

package ul

import "syscall"

const (
	descriptorExhaustionErrno = syscall.EMFILE
	descriptorBuffersErrno    = syscall.ENOBUFS
)
