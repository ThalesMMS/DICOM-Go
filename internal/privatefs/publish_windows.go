//go:build windows

package privatefs

import "golang.org/x/sys/windows"

func Replace(from, to string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Windows publishes with WRITE_THROUGH after syncing the file. FlushFileBuffers
// does not support the read-only directory handles used by os.Open.
func SyncDirectory(string) error { return nil }
