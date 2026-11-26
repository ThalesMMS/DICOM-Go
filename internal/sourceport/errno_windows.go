//go:build windows

package sourceport

import "syscall"

// Winsock reports these as WSA error numbers, which the syscall package does
// not map to its portable EADDRINUSE, EADDRNOTAVAIL and ENOBUFS values.
const (
	wsaEADDRINUSE    syscall.Errno = 10048
	wsaEADDRNOTAVAIL syscall.Errno = 10049
	wsaENOBUFS       syscall.Errno = 10055
)

func errnoKind(errno syscall.Errno) Kind {
	switch errno {
	case wsaEADDRINUSE:
		return AddressInUse
	case wsaEADDRNOTAVAIL:
		return AddressNotAvailable
	case wsaENOBUFS:
		return NoBuffers
	}
	return NotLocalEndpoint
}
