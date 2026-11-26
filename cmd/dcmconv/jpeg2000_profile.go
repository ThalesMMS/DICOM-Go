//go:build jpeg2000_openjpeg || codecfull

package main

import (
	"context"
	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func configureJPEG2000(opts options, target transfer.Syntax, decoders *pixeldata.MemoryRegistry, encoders *pixeldata.MemoryEncoderRegistry) error {
	m := opts.metadata
	if m.j2kBackend == "" {
		return nil
	}
	if err := jpeg2000.Register(decoders); err != nil {
		return err
	}
	resource := jpeg2000.OpenJPEGEncoderOptions{Executable: m.j2kExecutable}
	if target.UID == transfer.JPEG2000LosslessOnly.UID {
		resource.TileWidth, resource.TileHeight, resource.Resolutions = m.j2kWidth, m.j2kHeight, m.j2kResolutions
		return jpeg2000.RegisterOpenJPEGLosslessEncoder(context.Background(), encoders, resource)
	}
	return jpeg2000.RegisterOpenJPEGLossyEncoder(context.Background(), encoders, resource, jpeg2000.OpenJPEGLossyOptions{Rates: m.j2kRates, TileWidth: m.j2kWidth, TileHeight: m.j2kHeight, Resolutions: m.j2kResolutions, KeepRGB: true})
}
