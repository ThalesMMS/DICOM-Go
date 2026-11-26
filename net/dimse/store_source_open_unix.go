//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package dimse

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/nofollow"
)

func openStorePathNoFollow(path string) (*os.File, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrStoreInvalidSource
	}
	physicalParent, err := filepath.EvalSymlinks(filepath.Dir(absPath))
	if err != nil {
		return nil, err
	}
	absPath = filepath.Join(physicalParent, filepath.Base(absPath))
	return openStoreAbsolutePathNoFollow(absPath)
}

// openStoreAbsolutePathNoFollow opens absPath through nofollow, which refuses
// every symbolic link except root-owned system ones such as macOS /var.
func openStoreAbsolutePathNoFollow(absPath string) (*os.File, error) {
	relative := strings.TrimPrefix(filepath.Clean(absPath), string(filepath.Separator))
	if relative == "" || relative == "." {
		return nil, ErrStoreInvalidSource
	}
	return nofollow.OpenFile(absPath)
}

func openStoreRootedPathNoFollow(root, relative string) (*os.File, error) {
	if !filepath.IsAbs(root) {
		return nil, ErrStoreInvalidSource
	}
	return openStoreAbsolutePathNoFollow(filepath.Join(root, relative))
}
