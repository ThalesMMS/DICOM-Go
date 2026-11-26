//go:build windows

package deid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"

	"github.com/ThalesMMS/dicom-go/internal/nofollow"
)

func createDeflatedSpool() (*os.File, func() error, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, err
	}
	name := "dicom-deid-inflated-" + hex.EncodeToString(nonce[:])
	directory, err := nofollow.OpenDirectory(os.TempDir())
	if err != nil {
		return nil, nil, err
	}
	file, err := nofollow.CreateAt(directory, name)
	if err != nil {
		return nil, nil, errors.Join(err, directory.Close())
	}
	return file, func() error {
		return errors.Join(file.Close(), nofollow.RemoveAt(directory, name), directory.Close())
	}, nil
}
