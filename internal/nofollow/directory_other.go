//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package nofollow

import "os"

func OpenDirectoryAt(*os.File, string, bool) (*os.File, bool, error) {
	return nil, false, errUnsupported
}
func PublishClosedBetweenAt(*os.File, *os.File, string, string, os.FileInfo) (bool, error) {
	return false, errUnsupported
}
