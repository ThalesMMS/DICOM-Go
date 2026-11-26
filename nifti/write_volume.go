package nifti

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/seg"
)

// WriteVolume writes v as a single-file NIfTI-1 volume (.nii, or .nii.gz
// with CompressionGZIP): its voxels as stored, its datatype, its
// scl_slope/scl_inter when Scaling is applied, and its geometry as both
// qform and sform with the scanner-anatomical code. For a temporal volume,
// it retains the time origin and positive step in the input units at NIfTI-1
// precision, using seconds for an unspecified unit. A volume read without a
// spatial transform (XformSourcePixDim) is written without one. Volumes
// from NewVolume, such as labelmaps built from a DICOM Segmentation, use it
// to reach disk.
func WriteVolume(w io.Writer, v *Volume, compression Compression) (err error) {
	if v == nil || v.bytesPerVoxel == 0 {
		return ErrInvalidSource
	}
	spacing := [4]float64{v.Geometry.Spacing[0], v.Geometry.Spacing[1], v.Geometry.Spacing[2], 1}
	var timeOffset float64
	var temporalUnit uint8
	if v.Dimensions[3] > 1 {
		timeOffset, temporalUnit = v.Header.TOffset, v.Header.TemporalUnit()
		if v.Header.PixDim[4] > 0 {
			spacing[3] = v.Header.PixDim[4]
		}
	}
	slope, intercept := 1.0, 0.0
	if v.Scaling.Applied {
		slope, intercept = v.Scaling.Slope, v.Scaling.Intercept
	}
	affine := v.Geometry.IndexToRAS
	noTransform := v.Geometry.Source == XformSourcePixDim
	if noTransform {
		affine = render.GeometryAffine{spacing[0], 0, 0, 0, 0, spacing[1], 0, 0, 0, 0, spacing[2], 0, 0, 0, 0, 1}
	}
	header, err := buildHeader(headerSpec{
		Dimensions: v.Dimensions, Datatype: v.Header.Datatype, BitPix: int16(8 * v.bytesPerVoxel), Spacing: spacing,
		VoxelOffset: 352, Slope: slope, Intercept: intercept, AffineRAS: affine, NoSpatialTransform: noTransform,
		TimeOffset: timeOffset, TemporalUnit: temporalUnit,
	})
	if err != nil {
		return err
	}
	output := w
	if compression == CompressionGZIP {
		zipper := gzip.NewWriter(w)
		defer func() { err = errors.Join(err, zipper.Close()) }()
		output = zipper
	}
	if err := writeExact(output, header); err != nil {
		return err
	}
	if err := writeExact(output, []byte{0, 0, 0, 0}); err != nil {
		return err
	}
	return v.WritePayload(output)
}

// SEGReferences returns the image planes of the files of one series, one
// per frame, for SEGOptions.References: each plane's geometry, read strictly
// from Image Position and Orientation (Patient) and Pixel Spacing as the
// exporter reads them, and its Series, SOP Class and SOP Instance UIDs, with
// the frame number of a multi-frame image. Files without Pixel Data or
// patient geometry fail with ErrInvalidGeometry.
func SEGReferences(files []*object.File) ([]SEGReferenceImage, error) {
	var references []SEGReferenceImage
	for index, file := range files {
		if file == nil || file.Dataset == nil {
			return nil, fmt.Errorf("%w: file %d", ErrInvalidSource, index+1)
		}
		metadata, err := pixeldata.ExtractMetadata(file.Dataset)
		if err != nil {
			return nil, fmt.Errorf("%w: file %d: %w", ErrInvalidGeometry, index+1, err)
		}
		image := seg.ReferencedImage{}
		image.SeriesInstanceUID, _ = file.GetUID(tagSeriesInstanceUID)
		image.SOPClassUID, _ = file.GetUID(tagSOPClassUID)
		image.SOPInstanceUID, _ = file.GetUID(core.NewTag(0x0008, 0x0018))
		frames := max(metadata.NumberOfFrames, 1)
		for frame := 0; frame < frames; frame++ {
			geometry, err := strictSliceGeometry(file.FrameGeometryAt(frame), int(metadata.Rows), int(metadata.Columns))
			if err != nil {
				return nil, fmt.Errorf("file %d frame %d: %w", index+1, frame+1, err)
			}
			reference := SEGReferenceImage{Geometry: geometry, Image: image}
			if frames > 1 {
				reference.Image.Frames = []int{frame + 1}
			}
			references = append(references, reference)
		}
	}
	return references, nil
}
