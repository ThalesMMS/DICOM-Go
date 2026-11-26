package nifti

import (
	"context"
	"io"
)

// Info describes a validated single-file header without allocating voxel
// payloads. Inspect is not evidence that the payload is complete or its gzip
// checksum valid; Read must still succeed before the source is displayed.
type Info struct {
	Header       Header
	Geometry     Geometry
	Dimensions   [4]int
	Scaling      Scaling
	PayloadBytes uint64
	Warnings     []string
}

func (v *Volume) Info() Info {
	geometry := v.Geometry
	geometry.Warnings = append([]string(nil), geometry.Warnings...)
	return Info{Header: v.Header, Geometry: geometry, Dimensions: v.Dimensions,
		Scaling: v.Scaling, PayloadBytes: uint64(v.expected), Warnings: append([]string(nil), v.Warnings...)}
}

// Inspect applies Read's format, datatype, dimension, transform and resource
// admission to .nii/.nii.gz headers. Reader buffering may read a bounded prefix
// beyond the header. Extensions and voxel payloads are not materialized.
func Inspect(ctx context.Context, r io.Reader, options ReadOptions) (Info, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	options, err := normalizedReadOptions(options)
	if err != nil {
		return Info{}, err
	}
	stream, closeStream, err := openMaybeGZIP(r)
	if err != nil {
		return Info{}, err
	}
	defer closeStream()
	header, _, err := readHeaderBytes(stream)
	if err != nil {
		return Info{}, err
	}
	if header.Paired {
		return Info{}, readError(ReadCodeFormat, "magic", ErrPairedFile, nil)
	}
	volume, _, err := prepareVolume(header, options, false)
	if err != nil {
		return Info{}, err
	}
	if err = ctx.Err(); err != nil {
		return Info{}, err
	}
	return volume.Info(), nil
}
