//go:build !jpeg2000_openjpeg && !codecfull

package main

import (
	"errors"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func configureJPEG2000(opts options, _ transfer.Syntax, _ *pixeldata.MemoryRegistry, _ *pixeldata.MemoryEncoderRegistry) error {
	if opts.metadata.j2kBackend != "" {
		return errors.New("OpenJPEG profile unavailable: build this source checkout with -modfile=dcmconv_openjpeg.mod -tags jpeg2000_openjpeg and provide qualified OpenJPEG 2.5.4")
	}
	return nil
}
