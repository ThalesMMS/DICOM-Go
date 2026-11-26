package nifti_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
)

// Single-slice export (#1119). Synthetic frames only; no patient data.

var (
	tagSliceThickness       = core.NewTag(0x0018, 0x0050)
	tagSpacingBetweenSlices = core.NewTag(0x0018, 0x0088)
	tagImagerPixelSpacing   = core.NewTag(0x0018, 0x1164)
)

// withElements adds elements to a synthetic file (optional DS values are
// skipped when NaN) and removes tags.
func withElements(file *object.File, remove []core.Tag, elements ...core.Element) *object.File {
	for _, tag := range remove {
		file.Dataset.Remove(tag)
	}
	for _, element := range elements {
		file.Dataset.Put(element)
	}
	return file
}

func spacingElements(spacingBetweenSlices, thickness float64) []core.Element {
	var elements []core.Element
	if !math.IsNaN(spacingBetweenSlices) {
		elements = append(elements, derivedio.DS(tagSpacingBetweenSlices, spacingBetweenSlices))
	}
	if !math.IsNaN(thickness) {
		elements = append(elements, derivedio.DS(tagSliceThickness, thickness))
	}
	return elements
}

// projectionFrame is a DX-style frame: no Image Position/Orientation
// (Patient), no Pixel Spacing, optionally Imager Pixel Spacing, and a SOP
// Class whose IOD family PixelSpacingAt knows.
func projectionFrame(t *testing.T, sopClass, modality string, imagerSpacing []float64, extra ...core.Element) *object.File {
	t.Helper()
	file := testSlice(t, imageSpec{pixels: []byte{7, 9}})
	elements := append([]core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, sopClass), derivedio.CS(core.NewTag(0x0008, 0x0060), modality),
	}, extra...)
	if imagerSpacing != nil {
		elements = append(elements, derivedio.DS(tagImagerPixelSpacing, imagerSpacing...))
	}
	return withElements(file, []core.Tag{tagImagePositionPatient, tagImageOrientationPatient, tagPixelSpacing, derivedio.TagFrameOfReferenceUID}, elements...)
}

func TestWriteSingleSliceIsRejectedByDefault(t *testing.T) {
	var output bytes.Buffer
	_, err := nifti.WriteFiles(context.Background(), &output, []*object.File{testSlice(t, imageSpec{pixels: []byte{1, 2}})}, nifti.DefaultOptions())
	requireExportError(t, err, nifti.CodeSingleSlice, nifti.ErrInvalidGeometry)
	if output.Len() != 0 {
		t.Fatalf("destination length = %d, want 0", output.Len())
	}
}

// TestWriteSingleSlicePatientSpacingPrecedence: the through-plane pixdim is
// Spacing Between Slices, then Slice Thickness, then 1 mm (ITK 9467d94115,
// 296bed66ff), reported with its source, and the reader round-trips it.
func TestWriteSingleSlicePatientSpacingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name               string
		spacing, thickness float64
		orientation        [6]float64
		wantZ              float64
		wantSource         object.SliceSpacingSource
		wantWarnings       []string
		wantAffine         [16]float64
	}{
		{
			name: "spacing between slices wins", spacing: 3, thickness: 5, wantZ: 3,
			wantSource: object.SliceSpacingFromSpacingBetweenSlices,
			wantAffine: [16]float64{-1, 0, 0, -10, 0, -2, 0, -20, 0, 0, 3, 30, 0, 0, 0, 1},
		},
		{
			name: "negative spacing between slices uses its magnitude", spacing: -2.5, thickness: 5, wantZ: 2.5,
			wantSource: object.SliceSpacingFromSpacingBetweenSlices,
			wantAffine: [16]float64{-1, 0, 0, -10, 0, -2, 0, -20, 0, 0, 2.5, 30, 0, 0, 0, 1},
		},
		{
			name: "slice thickness when spacing is absent", spacing: math.NaN(), thickness: 5, wantZ: 5,
			wantSource: object.SliceSpacingFromSliceThickness, wantWarnings: []string{nifti.WarningSliceSpacingFromThickness},
			wantAffine: [16]float64{-1, 0, 0, -10, 0, -2, 0, -20, 0, 0, 5, 30, 0, 0, 0, 1},
		},
		{
			name: "one millimetre when neither is present", spacing: math.NaN(), thickness: math.NaN(), wantZ: 1,
			wantSource: object.SliceSpacingDefault, wantWarnings: []string{nifti.WarningSliceSpacingDefault},
			wantAffine: [16]float64{-1, 0, 0, -10, 0, -2, 0, -20, 0, 0, 1, 30, 0, 0, 0, 1},
		},
		{
			name: "coronal normal", spacing: 4, thickness: math.NaN(), orientation: [6]float64{1, 0, 0, 0, 0, -1}, wantZ: 4,
			wantSource: object.SliceSpacingFromSpacingBetweenSlices,
			wantAffine: [16]float64{-1, 0, 0, -10, 0, 0, -4, -20, 0, -2, 0, 30, 0, 0, 0, 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := withElements(testSlice(t, imageSpec{
				position: [3]float64{10, 20, 30}, orientation: tc.orientation, pixels: []byte{4, 8},
			}), nil, spacingElements(tc.spacing, tc.thickness)...)
			options := nifti.DefaultOptions()
			options.SingleSlice = nifti.SingleSlicePatient
			var output bytes.Buffer
			report, err := nifti.WriteFiles(context.Background(), &output, []*object.File{file}, options)
			if err != nil {
				t.Fatalf("WriteFiles: %v", err)
			}
			data := output.Bytes()
			requireNIfTIHeader(t, data, [4]int{2, 1, 1, 1}, nifti.DatatypeUint8, 8)
			if got := headerFloat32(data, 88); math.Abs(got-tc.wantZ) > 1e-6 {
				t.Fatalf("pixdim[3] = %v, want %v", got, tc.wantZ)
			}
			if !report.SingleSlice || report.SliceSpacingSource != tc.wantSource || report.NoSpatialTransform ||
				!reflect.DeepEqual(report.Warnings, tc.wantWarnings) {
				t.Fatalf("report single %v source %q transformless %v warnings %v, want source %q warnings %v",
					report.SingleSlice, report.SliceSpacingSource, report.NoSpatialTransform, report.Warnings, tc.wantSource, tc.wantWarnings)
			}
			if report.Sidecar.SliceSpacingSource != string(tc.wantSource) {
				t.Fatalf("sidecar slice spacing source = %q, want %q", report.Sidecar.SliceSpacingSource, tc.wantSource)
			}
			requireAffine(t, report.IndexToRAS[:], tc.wantAffine[:], 1e-6)
			if got := [2]int16{headerInt16(data, 252), headerInt16(data, 254)}; got != [2]int16{1, 1} {
				t.Fatalf("qform/sform codes = %v, want scanner (1, 1)", got)
			}
			if voxels := data[352:]; !bytes.Equal(voxels, []byte{4, 8}) {
				t.Fatalf("voxels = %v, want [4 8]", voxels)
			}

			volume, err := nifti.Read(context.Background(), bytes.NewReader(data), nifti.ReadOptions{})
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if volume.Dimensions != [4]int{2, 1, 1, 1} || math.Abs(volume.Geometry.Spacing[2]-tc.wantZ) > 1e-6 {
				t.Fatalf("read dimensions %v spacing %v, want z %v", volume.Dimensions, volume.Geometry.Spacing, tc.wantZ)
			}
			requireAffine(t, volume.Geometry.IndexToRAS[:], tc.wantAffine[:], 1e-5)
			if volume.Value(0) != 4 || volume.Value(1) != 8 {
				t.Fatalf("read voxels = %v, %v; want 4, 8", volume.Value(0), volume.Value(1))
			}
			slice := volume.SliceGeometries()[0]
			if math.Abs(slice.Origin.X-10) > 1e-4 || math.Abs(slice.Origin.Y-20) > 1e-4 || math.Abs(slice.Origin.Z-30) > 1e-4 {
				t.Fatalf("read origin = %+v, want LPS (10, 20, 30)", slice.Origin)
			}
		})
	}
}

// TestWriteSingleSliceImageGrid: a projection frame without patient
// geometry exports only with SingleSliceImageGrid, as a NIfTI method-1 grid
// with no qform/sform, and reads back only with AllowMissingTransform.
func TestWriteSingleSliceImageGrid(t *testing.T) {
	const (
		dxStorage = "1.2.840.10008.5.1.4.1.1.1.1"
		usStorage = "1.2.840.10008.5.1.4.1.1.6.1"
	)
	for _, tc := range []struct {
		name         string
		file         func(*testing.T) *object.File
		wantPixdim   [3]float64
		wantSemantic object.PixelSpacingSemantic
		wantSource   object.SliceSpacingSource
		wantWarnings []string
	}{
		{
			name: "DX with imager pixel spacing and slice thickness",
			file: func(t *testing.T) *object.File {
				return projectionFrame(t, dxStorage, "DX", []float64{0.2, 0.1}, derivedio.DS(tagSliceThickness, 2))
			},
			wantPixdim: [3]float64{0.1, 0.2, 2}, wantSemantic: object.SpacingDetector,
			wantSource:   object.SliceSpacingFromSliceThickness,
			wantWarnings: []string{nifti.WarningSliceSpacingFromThickness, nifti.WarningNoPatientGeometry},
		},
		{
			name: "DX spacing between slices",
			file: func(t *testing.T) *object.File {
				return projectionFrame(t, dxStorage, "DX", []float64{0.15}, spacingElements(3, 5)...)
			},
			wantPixdim: [3]float64{0.15, 0.15, 3}, wantSemantic: object.SpacingDetector,
			wantSource:   object.SliceSpacingFromSpacingBetweenSlices,
			wantWarnings: []string{nifti.WarningNoPatientGeometry},
		},
		{
			name:       "US without any spacing",
			file:       func(t *testing.T) *object.File { return projectionFrame(t, usStorage, "US", nil) },
			wantPixdim: [3]float64{1, 1, 1},
			wantSource: object.SliceSpacingDefault,
			wantWarnings: []string{
				nifti.WarningSliceSpacingDefault, nifti.WarningNoPatientGeometry, nifti.WarningPixelSpacingDefault,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := nifti.DefaultOptions()
			options.SingleSlice = nifti.SingleSlicePatient
			var rejected bytes.Buffer
			_, err := nifti.WriteFiles(context.Background(), &rejected, []*object.File{tc.file(t)}, options)
			if err == nil || rejected.Len() != 0 {
				t.Fatalf("SingleSlicePatient accepted a frame without patient geometry (err %v, %d bytes)", err, rejected.Len())
			}

			options.SingleSlice = nifti.SingleSliceImageGrid
			var output bytes.Buffer
			report, err := nifti.WriteFiles(context.Background(), &output, []*object.File{tc.file(t)}, options)
			if err != nil {
				t.Fatalf("WriteFiles: %v", err)
			}
			data := output.Bytes()
			requireNIfTIHeader(t, data, [4]int{2, 1, 1, 1}, nifti.DatatypeUint8, 8)
			for axis, want := range tc.wantPixdim {
				if got := headerFloat32(data, 80+4*axis); math.Abs(got-want) > 1e-6 {
					t.Fatalf("pixdim[%d] = %v, want %v", axis+1, got, want)
				}
			}
			if got := [2]int16{headerInt16(data, 252), headerInt16(data, 254)}; got != [2]int16{0, 0} {
				t.Fatalf("qform/sform codes = %v, want (0, 0)", got)
			}
			if !report.NoSpatialTransform || report.PixelSpacingSemantic != tc.wantSemantic ||
				report.SliceSpacingSource != tc.wantSource || !reflect.DeepEqual(report.Warnings, tc.wantWarnings) {
				t.Fatalf("report transformless %v semantic %q source %q warnings %v", report.NoSpatialTransform,
					report.PixelSpacingSemantic, report.SliceSpacingSource, report.Warnings)
			}
			if report.Sidecar.PixelSpacingSemantic != string(tc.wantSemantic) || !reflect.DeepEqual(report.Sidecar.Warnings, tc.wantWarnings) {
				t.Fatalf("sidecar = %+v", report.Sidecar)
			}
			wantAffine := [16]float64{tc.wantPixdim[0], 0, 0, 0, 0, tc.wantPixdim[1], 0, 0, 0, 0, tc.wantPixdim[2], 0, 0, 0, 0, 1}
			requireAffine(t, report.IndexToRAS[:], wantAffine[:], 1e-6)

			if _, err := nifti.Read(context.Background(), bytes.NewReader(data), nifti.ReadOptions{}); !errors.Is(err, nifti.ErrNoSpatialTransform) {
				t.Fatalf("Read without AllowMissingTransform error = %v, want ErrNoSpatialTransform", err)
			}
			volume, err := nifti.Read(context.Background(), bytes.NewReader(data), nifti.ReadOptions{AllowMissingTransform: true})
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if volume.Geometry.Source != nifti.XformSourcePixDim || volume.Dimensions != [4]int{2, 1, 1, 1} {
				t.Fatalf("read source %q dimensions %v", volume.Geometry.Source, volume.Dimensions)
			}
			requireAffine(t, volume.Geometry.IndexToRAS[:], wantAffine[:], 1e-6)
			if volume.Value(0) != 7 || volume.Value(1) != 9 {
				t.Fatalf("read voxels = %v, %v; want 7, 9", volume.Value(0), volume.Value(1))
			}
		})
	}
}

func TestWriteSingleSliceKeepsVolumeRules(t *testing.T) {
	options := nifti.DefaultOptions()
	options.SingleSlice = nifti.SingleSliceImageGrid

	t.Run("two instances stay a volume", func(t *testing.T) {
		var output bytes.Buffer
		report, err := nifti.WriteFiles(context.Background(), &output, testUint8Volume(t, "MONOCHROME2"), options)
		if err != nil {
			t.Fatalf("WriteFiles: %v", err)
		}
		if report.SingleSlice || report.SliceSpacingSource != object.SliceSpacingFromPositions || report.Dimensions != [4]int{2, 1, 2, 1} {
			t.Fatalf("report single %v source %q dimensions %v", report.SingleSlice, report.SliceSpacingSource, report.Dimensions)
		}
	})
	t.Run("an image grid needs a single source", func(t *testing.T) {
		files := []*object.File{
			projectionFrame(t, "1.2.840.10008.5.1.4.1.1.1.1", "DX", []float64{0.1}),
			projectionFrame(t, "1.2.840.10008.5.1.4.1.1.1.1", "DX", []float64{0.1}),
		}
		var output bytes.Buffer
		_, err := nifti.WriteFiles(context.Background(), &output, files, options)
		requireExportError(t, err, nifti.CodeIdentity, nifti.ErrMixedIdentity)
	})
	t.Run("invalid patient geometry is not demoted to a grid", func(t *testing.T) {
		file := testSlice(t, imageSpec{orientation: [6]float64{1, 0, 0, 1, 0, 0}, pixels: []byte{1, 2}})
		var output bytes.Buffer
		_, err := nifti.WriteFiles(context.Background(), &output, []*object.File{file}, options)
		requireExportError(t, err, nifti.CodeGeometry, nifti.ErrInvalidGeometry)
	})
	for _, tc := range []struct {
		name    string
		missing core.Tag
	}{
		{name: "position without orientation", missing: tagImageOrientationPatient},
		{name: "orientation without position", missing: tagImagePositionPatient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := withElements(testSlice(t, imageSpec{pixels: []byte{1, 2}}), []core.Tag{tc.missing})
			var output bytes.Buffer
			_, err := nifti.WriteFiles(context.Background(), &output, []*object.File{file}, options)
			requireExportError(t, err, nifti.CodeGeometry, nifti.ErrInvalidGeometry)
		})
	}
	t.Run("patient geometry needs a frame of reference", func(t *testing.T) {
		file := withElements(testSlice(t, imageSpec{pixels: []byte{1, 2}}), []core.Tag{derivedio.TagFrameOfReferenceUID})
		var output bytes.Buffer
		_, err := nifti.WriteFiles(context.Background(), &output, []*object.File{file}, options)
		requireExportError(t, err, nifti.CodeIdentity, nifti.ErrMixedIdentity)
	})
	t.Run("unknown policy", func(t *testing.T) {
		bad := nifti.DefaultOptions()
		bad.SingleSlice = nifti.SingleSliceImageGrid + 1
		_, err := nifti.PlanVolume(context.Background(), nifti.NewFilesSource([]*object.File{testSlice(t, imageSpec{pixels: []byte{1, 2}})}), bad)
		requireExportError(t, err, nifti.CodeGeometry, nifti.ErrInvalidGeometry)
	})
}
