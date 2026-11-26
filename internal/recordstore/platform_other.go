//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package recordstore

import "os"

func lockFile(*os.File) error                                        { return ErrUnavailable }
func syncDirectory(*os.File) error                                   { return ErrUnavailable }
func publish(*os.File, *os.File, string, string, bool) (bool, error) { return false, ErrUnavailable }
