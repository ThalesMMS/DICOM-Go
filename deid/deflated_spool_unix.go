//go:build !windows

package deid

import (
	"errors"
	"os"
)

func createDeflatedSpool() (*os.File, func() error, error) {
	file, err := os.CreateTemp("", "dicom-deid-inflated-*")
	if err != nil {
		return nil, nil, err
	}
	return file, func() error { return errors.Join(file.Close(), os.Remove(file.Name())) }, nil
}
