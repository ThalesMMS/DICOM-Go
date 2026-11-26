//go:build !unix && !windows

package sourceport

import "syscall"

func errnoKind(syscall.Errno) Kind { return NotLocalEndpoint }
