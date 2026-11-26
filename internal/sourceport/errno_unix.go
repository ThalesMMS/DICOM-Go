//go:build unix

package sourceport

import "syscall"

func errnoKind(errno syscall.Errno) Kind {
	switch errno {
	case syscall.EADDRINUSE:
		return AddressInUse
	case syscall.EADDRNOTAVAIL:
		return AddressNotAvailable
	case syscall.ENOBUFS:
		return NoBuffers
	}
	return NotLocalEndpoint
}
