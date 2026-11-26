//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package dimse

import (
	"context"
	"errors"
	"os"
)

func acquireMWLReadLock(context.Context, *os.File) (func(), error) {
	return nil, errors.New("MWL shared lockfile is unsupported on this platform")
}
