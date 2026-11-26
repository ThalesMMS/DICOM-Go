//go:build windows

package ul

const (
	descriptorExhaustionErrno = wsaEMFILE
	descriptorBuffersErrno    = wsaENOBUFS
)
