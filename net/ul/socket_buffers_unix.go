//go:build unix

package ul

import "syscall"

func setSocketBuffers(fd uintptr, b SocketBuffers) error {
	if b.Send > 0 {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, b.Send); err != nil {
			return err
		}
	}
	if b.Receive > 0 {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, b.Receive); err != nil {
			return err
		}
	}
	return nil
}

func getSocketBuffers(fd uintptr) (SocketBuffers, error) {
	send, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	if err != nil {
		return SocketBuffers{}, err
	}
	receive, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	if err != nil {
		return SocketBuffers{}, err
	}
	return SocketBuffers{Send: send, Receive: receive}, nil
}
