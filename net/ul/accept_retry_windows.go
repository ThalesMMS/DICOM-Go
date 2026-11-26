//go:build windows

package ul

import "syscall"

// Winsock reports these as WSA error numbers, which the syscall package does
// not map to its portable EMFILE and ENOBUFS values.
const (
	wsaEMFILE  syscall.Errno = 10024
	wsaENOBUFS syscall.Errno = 10055
)

// acceptResourceErrno reports whether an accept failed because the process or
// the system ran out of sockets or buffers.
func acceptResourceErrno(errno syscall.Errno) bool {
	return errno == wsaEMFILE || errno == wsaENOBUFS
}
