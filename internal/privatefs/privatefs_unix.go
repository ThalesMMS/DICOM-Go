//go:build !windows

package privatefs

import "os"

func Replace(from, to string) error { return os.Rename(from, to) }
func SyncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func protect(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0600)
	if info.IsDir() {
		mode = 0700
	}
	return os.Chmod(path, mode)
}

func isPrivate(_ string, info os.FileInfo) (bool, error) {
	return info.Mode().Perm()&0077 == 0, nil
}
