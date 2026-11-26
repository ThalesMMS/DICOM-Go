package render

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math"

	"github.com/ThalesMMS/dicom-go/object"
)

type MPRPlane string

const (
	MPRPlaneAxial    MPRPlane = "axial"
	MPRPlaneCoronal  MPRPlane = "coronal"
	MPRPlaneSagittal MPRPlane = "sagittal"
)

// GantryTiltRenderMode selects whether MPR views apply the patient-space
// correction for a stable gantry-tilted stack or expose the acquired source
// grid for explicit inspection. Corrected is the safe/default clinical view.
type GantryTiltRenderMode int

const (
	GantryTiltCorrected GantryTiltRenderMode = iota
	GantryTiltSourceGeometry
)

// MPRRenderOptions controls geometry and resource limits shared by single-plane
// and projection MPR rendering. The zero value applies gantry-tilt correction
// and finite default limits.
type MPRRenderOptions struct {
	GantryTiltMode GantryTiltRenderMode
	Limits         MPRLimits
	// Interpolation resamples tilt-corrected scalar planes (zero value:
	// trilinear). Cubic suits settled frames; interaction stays trilinear.
	Interpolation Interpolation
	// Slab controls sampling and endpoint weights in the scalar slab API.
	// Its zero value preserves one sample per source spacing and equal weights.
	// Legacy image-returning MPR APIs do not consume this field.
	Slab SlabSampling
}

func DefaultMPRRenderOptions() MPRRenderOptions {
	return MPRRenderOptions{GantryTiltMode: GantryTiltCorrected}
}

// MPRPlaneImageAxes returns the patient-space (LPS) directions of the image x
// axis (toward the right edge) and y axis (toward the bottom edge) of an
// orthogonal plane rendered by RenderMPRPlane, for a stack whose columns run
// along axisX and whose rows run along axisY (the Image Orientation (Patient)
// row and column vectors). The axial plane is the acquired plane. Coronal and
// sagittal planes cut through the stack with the last slice (farthest along
// axisX x axisY) at the top, so their y axis is the negated slice normal.
//
// The plane names follow the volume axes, not the anatomy: for a sagittal
// acquisition the "axial" plane is anatomically sagittal. Orientation labels
// should therefore be derived from these axes (for example with
// object.EdgeLabelsForImageAxes) rather than from the plane name. It reports
// false for an unknown plane or degenerate axes.
func MPRPlaneImageAxes(plane MPRPlane, axisX, axisY Vec3) (right, down Vec3, ok bool) {
	axisX, axisY = axisX.Normalize(), axisY.Normalize()
	normal := axisX.Cross(axisY).Normalize()
	if axisX.Length() == 0 || axisY.Length() == 0 || normal.Length() == 0 {
		return Vec3{}, Vec3{}, false
	}
	switch plane {
	case MPRPlaneAxial:
		return axisX, axisY, true
	case MPRPlaneCoronal:
		return axisX, normal.Scale(-1), true
	case MPRPlaneSagittal:
		return axisY, normal.Scale(-1), true
	default:
		return Vec3{}, Vec3{}, false
	}
}

func RenderMPRPlane(series *Stack, plane MPRPlane, index int, window WindowLevel) (image.Image, error) {
	return RenderMPRPlaneWithOptions(series, plane, index, window, DefaultMPRRenderOptions())
}

// RenderMPRPlaneWithOptions renders one orthogonal MPR plane while allowing an
// explicit source-grid inspection of stable gantry-tilted acquisitions.
func RenderMPRPlaneWithOptions(series *Stack, plane MPRPlane, index int, window WindowLevel, options MPRRenderOptions) (image.Image, error) {
	if series == nil || len(series.Frames) == 0 {
		return blankImage(512, 512), fmt.Errorf("render: no series selected")
	}
	first := firstRenderableSlice(series)
	if first == nil {
		return blankImage(512, 512), fmt.Errorf("render: series has no displayable images")
	}
	rows := int(first.Metadata.Rows)
	cols := int(first.Metadata.Columns)
	if rows <= 0 || cols <= 0 {
		return blankImage(512, 512), fmt.Errorf("render: invalid slice dimensions")
	}
	limits, err := normalizeMPRLimits(options.Limits)
	if err != nil {
		return blankImage(1, 1), err
	}
	window = normalizeWindow(window, series.DefaultWindow)
	mapper := prepareWindow(window)
	if plane == MPRPlaneCoronal || plane == MPRPlaneSagittal {
		correctTilt := options.GantryTiltMode != GantryTiltSourceGeometry
		if vol, sampler, err := stackVolumeSampler(series, correctTilt); err == nil {
			img, rendered, renderErr := renderCachedMPRPlane(vol, sampler, plane, index, mapper, correctTilt, limits)
			sampler.Close()
			if renderErr != nil {
				return blankImage(1, 1), renderErr
			}
			if rendered {
				return img, nil
			}
		} else if isNonCorrectableGeometry(err) {
			return blankImage(512, 512), err
		}
	}
	switch plane {
	case MPRPlaneAxial:
		if _, err := validateMPROutput(cols, rows, 1, 1, limits); err != nil {
			return blankImage(1, 1), err
		}
		if index < 0 {
			index = 0
		}
		if index >= len(series.Frames) {
			index = len(series.Frames) - 1
		}
		return RenderFrame(series.Frames[index], window)
	case MPRPlaneCoronal:
		if index < 0 {
			index = 0
		}
		if index >= rows {
			index = rows - 1
		}
		height := orthogonalMPRHeight(series, seriesColumnSpacing(series))
		if _, err := validateMPROutput(cols, height, 1, 1, limits); err != nil {
			return blankImage(1, 1), err
		}
		img := image.NewGray(image.Rect(0, 0, cols, height))
		for outZ := 0; outZ < height; outZ++ {
			slice := series.Frames[orthogonalDisplaySliceIndex(outZ, height, len(series.Frames))]
			for x := 0; x < cols; x++ {
				img.SetGray(x, outZ, color.Gray{Y: mprVoxelGray(slice, x, index, mapper)})
			}
		}
		return img, nil
	case MPRPlaneSagittal:
		if index < 0 {
			index = 0
		}
		if index >= cols {
			index = cols - 1
		}
		height := orthogonalMPRHeight(series, seriesRowSpacing(series))
		if _, err := validateMPROutput(rows, height, 1, 1, limits); err != nil {
			return blankImage(1, 1), err
		}
		img := image.NewGray(image.Rect(0, 0, rows, height))
		for outZ := 0; outZ < height; outZ++ {
			slice := series.Frames[orthogonalDisplaySliceIndex(outZ, height, len(series.Frames))]
			for y := 0; y < rows; y++ {
				img.SetGray(y, outZ, color.Gray{Y: mprVoxelGray(slice, index, y, mapper)})
			}
		}
		return img, nil
	default:
		return blankImage(512, 512), fmt.Errorf("render: unsupported MPR plane %q", plane)
	}
}

func stackVolumeSampler(series *Stack, regularize bool) (*Volume, *volumeSampler, error) {
	vol, err := series.Volume()
	if err != nil {
		return nil, nil, err
	}
	var sampler *volumeSampler
	var ok bool
	if regularize {
		sampler, ok = newVolumeSampler(vol)
	} else {
		sampler, ok = newDecodedVolumeSampler(vol)
	}
	if !ok {
		return vol, nil, fmt.Errorf("render: volume contains no decodable voxels")
	}
	return vol, sampler, nil
}

func renderCachedMPRPlane(vol *Volume, sampler *volumeSampler, plane MPRPlane, index int, mapper preparedVOI, correctGantryTilt bool, limits MPRLimits) (image.Image, bool, error) {
	if vol == nil || sampler == nil {
		return nil, false, nil
	}
	rows, cols, depth := sampler.rows, sampler.cols, sampler.depth
	if rows <= 0 || cols <= 0 || depth <= 0 {
		return nil, false, nil
	}
	if correctGantryTilt {
		if corrected, width, height, ok := vol.correctedOrthogonalPlane(plane, index); ok {
			img, err := resliceObliqueWithSamplerLimitsContext(context.Background(), vol, sampler, corrected, width, height, mapper, limits)
			return img, true, err
		}
	}
	switch plane {
	case MPRPlaneCoronal:
		if index < 0 {
			index = 0
		}
		if index >= rows {
			index = rows - 1
		}
		height := orthogonalVolumeHeight(depth, vol.SliceSpacing, vol.ColSpacing)
		if _, err := validateMPROutput(cols, height, 1, 1, limits); err != nil {
			return nil, true, err
		}
		img := image.NewGray(image.Rect(0, 0, cols, height))
		parallelRows(height, func(outZ int) {
			z := orthogonalDisplaySliceIndex(outZ, height, depth)
			offset := img.PixOffset(0, outZ)
			for x := 0; x < cols; x++ {
				if value, ok := sampler.valueAt(x, index, z); ok {
					img.Pix[offset+x] = sampler.displayGrayMapped(value, mapper)
				}
			}
		})
		return img, true, nil
	case MPRPlaneSagittal:
		if index < 0 {
			index = 0
		}
		if index >= cols {
			index = cols - 1
		}
		height := orthogonalVolumeHeight(depth, vol.SliceSpacing, vol.RowSpacing)
		if _, err := validateMPROutput(rows, height, 1, 1, limits); err != nil {
			return nil, true, err
		}
		img := image.NewGray(image.Rect(0, 0, rows, height))
		parallelRows(height, func(outZ int) {
			z := orthogonalDisplaySliceIndex(outZ, height, depth)
			offset := img.PixOffset(0, outZ)
			for y := 0; y < rows; y++ {
				if value, ok := sampler.valueAt(index, y, z); ok {
					img.Pix[offset+y] = sampler.displayGrayMapped(value, mapper)
				}
			}
		})
		return img, true, nil
	default:
		return nil, false, nil
	}
}

func firstRenderableSlice(series *Stack) *Frame {
	if series == nil {
		return nil
	}
	for _, slice := range series.Frames {
		if slice != nil && slice.DecodeErr == nil && slice.Metadata.Rows > 0 && slice.Metadata.Columns > 0 {
			return slice
		}
	}
	return nil
}

func mprVoxelGray(slice *Frame, x, y int, mapper preparedVOI) uint8 {
	value, ok := PixelValueAt(slice, x, y)
	if !ok {
		return 0
	}
	return displayGrayMapped(value.Rescaled, mapper, slice.Metadata.PhotometricInterpretation)
}

func orthogonalMPRHeight(series *Stack, inPlaneAxisSpacing float64) int {
	if series == nil || len(series.Frames) == 0 {
		return 0
	}
	// Resample the through-plane axis by the actual inter-slice SPACING, not
	// SliceThickness — the slab thickness can differ from the slice-to-slice
	// distance (overlapping or gapped acquisitions), which would squash or stretch
	// the coronal/sagittal reformat. This matches BuildVolume.SliceSpacing.
	sliceSpacing := seriesSliceSpacing(series)
	if sliceSpacing <= 0 || inPlaneAxisSpacing <= 0 || math.IsNaN(sliceSpacing) || math.IsNaN(inPlaneAxisSpacing) || math.IsInf(sliceSpacing, 0) || math.IsInf(inPlaneAxisSpacing, 0) {
		return len(series.Frames)
	}
	height := int(math.Round(float64(len(series.Frames)) * sliceSpacing / inPlaneAxisSpacing))
	if height < 1 {
		return 1
	}
	return height
}

func orthogonalVolumeHeight(depth int, sliceSpacing, inPlaneAxisSpacing float64) int {
	if depth <= 0 {
		return 0
	}
	if sliceSpacing <= 0 || inPlaneAxisSpacing <= 0 || math.IsNaN(sliceSpacing) || math.IsNaN(inPlaneAxisSpacing) || math.IsInf(sliceSpacing, 0) || math.IsInf(inPlaneAxisSpacing, 0) {
		return depth
	}
	height := int(math.Round(float64(depth) * sliceSpacing / inPlaneAxisSpacing))
	if height < 1 {
		return 1
	}
	return height
}

// seriesSliceSpacing is the mean through-plane spacing between slices in mm,
// derived from their spatial positions — NOT SliceThickness, which is the slab
// thickness and may differ from the inter-slice distance. Falls back to
// seriesFallbackSliceSpacing (Spacing Between Slices, then SliceThickness,
// then 1 mm) when positions are unavailable. This mirrors
// BuildVolume.SliceSpacing so the orthogonal reformats and the volume-based
// reslice agree.
func seriesSliceSpacing(series *Stack) float64 {
	if series == nil {
		return 1
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	count := 0
	for _, sl := range series.Frames {
		if sl == nil {
			continue
		}
		v := spatialSortValue(sl)
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
		count++
	}
	if count > 1 && hi > lo {
		if sp := (hi - lo) / float64(count-1); sp > 0 && !math.IsNaN(sp) && !math.IsInf(sp, 0) {
			return sp
		}
	}
	return seriesFallbackSliceSpacing(series).Value
}

// framePixelSpacing reads a [row, column] Pixel Spacing: a single value is
// isotropic (object.SpacingPair). Both must be finite and positive.
func framePixelSpacing(values []float64) (row, column float64, ok bool) {
	switch {
	case len(values) >= 2:
		row, column = values[0], values[1]
	case len(values) == 1:
		row, column = values[0], values[0]
	default:
		return 0, 0, false
	}
	if !finitePositiveSpacing(row) || !finitePositiveSpacing(column) {
		return 0, 0, false
	}
	return row, column, true
}

// seriesPixelSpacing is the series Pixel Spacing, or that of its first frame
// with one; 0, 0 when none is usable.
func seriesPixelSpacing(series *Stack) (row, column float64) {
	if series == nil {
		return 0, 0
	}
	if row, column, ok := framePixelSpacing(series.PixelSpacing); ok {
		return row, column
	}
	for _, frame := range series.Frames {
		if frame != nil {
			if row, column, ok := framePixelSpacing(frame.PixelSpacing); ok {
				return row, column
			}
		}
	}
	return 0, 0
}

func seriesRowSpacing(series *Stack) float64 {
	row, _ := seriesPixelSpacing(series)
	return row
}

func seriesColumnSpacing(series *Stack) float64 {
	_, column := seriesPixelSpacing(series)
	return column
}

// seriesFallbackSliceSpacing is the slice spacing when slice positions
// cannot measure it (one slice, or no spread): object.ResolveSliceSpacing
// over the series attributes, else over the first frame with a usable one.
// Spacing Between Slices therefore wins over Slice Thickness (ITK
// 9467d94115), and 1 mm is the reported default.
func seriesFallbackSliceSpacing(series *Stack) object.SliceSpacing {
	if series == nil {
		return object.ResolveSliceSpacing(0, 0)
	}
	if spacing := object.ResolveSliceSpacing(series.SpacingBetweenSlices, series.SliceThickness); spacing.Source != object.SliceSpacingDefault {
		return spacing
	}
	for _, frame := range series.Frames {
		if frame == nil {
			continue
		}
		if spacing := object.ResolveSliceSpacing(frame.SpacingBetweenSlices, frame.SliceThickness); spacing.Source != object.SliceSpacingDefault {
			return spacing
		}
	}
	return object.ResolveSliceSpacing(0, 0)
}

func finitePositiveSpacing(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func resampledSliceIndex(outZ, outputHeight, sliceCount int) int {
	if sliceCount <= 1 || outputHeight <= 1 {
		return 0
	}
	index := int(math.Floor((float64(outZ) + 0.5) * float64(sliceCount) / float64(outputHeight)))
	if index < 0 {
		return 0
	}
	if index >= sliceCount {
		return sliceCount - 1
	}
	return index
}

func orthogonalDisplaySliceIndex(outZ, outputHeight, sliceCount int) int {
	return resampledSliceIndex(outputHeight-1-outZ, outputHeight, sliceCount)
}
