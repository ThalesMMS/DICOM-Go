//go:build !windows

package object

import "os"

func openSourceFile(path string) (*os.File, error) { return os.Open(path) }
