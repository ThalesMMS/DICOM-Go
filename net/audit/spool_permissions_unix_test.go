//go:build !windows

package audit

import "os"

func makeSpoolDirectoryPublic(path string) error { return os.Chmod(path, 0755) }
