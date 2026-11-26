//go:build !unix && !windows

package ul

func setSocketBuffers(uintptr, SocketBuffers) error {
	return ErrSocketBuffersUnsupported
}

func getSocketBuffers(uintptr) (SocketBuffers, error) {
	return SocketBuffers{}, ErrSocketBuffersUnsupported
}
