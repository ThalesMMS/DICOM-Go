package nifti_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
)

func readBytes(t *testing.T, data []byte, options nifti.ReadOptions) (*nifti.Volume, error) {
	t.Helper()
	return nifti.Read(context.Background(), bytes.NewReader(data), options)
}

func mustRead(t *testing.T, data []byte, options nifti.ReadOptions) *nifti.Volume {
	t.Helper()
	volume, err := readBytes(t, data, options)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	return volume
}

func requireReadError(t *testing.T, err error, code nifti.ReadErrorCode, sentinel error) *nifti.ReadError {
	t.Helper()
	var readErr *nifti.ReadError
	if !errors.As(err, &readErr) {
		t.Fatalf("error = %v (%T), want *nifti.ReadError", err, err)
	}
	if readErr.Code != code || !errors.Is(err, sentinel) {
		t.Fatalf("error = %v (code %q), want code %q wrapping %v", err, readErr.Code, code, sentinel)
	}
	return readErr
}

func volumeValues(volume *nifti.Volume) []float64 {
	out := make([]float64, volume.Len())
	for index := range out {
		out[index] = volume.Value(index)
	}
	return out
}

func equalValues(got, want []float64) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if math.IsNaN(want[index]) {
			if !math.IsNaN(got[index]) {
				return false
			}
			continue
		}
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// TestReadRoundTripsExporterOrientations reads back every exporter
// orientation fixture and recovers the source DICOM slice geometry in LPS.
func TestReadRoundTripsExporterOrientations(t *testing.T) {
	for _, fixture := range patientOrientationFixtures() {
		for _, compression := range []nifti.Compression{nifti.CompressionNone, nifti.CompressionGZIP} {
			t.Run(fixture.name+"-"+compression.String(), func(t *testing.T) {
				options := nifti.DefaultOptions()
				options.Compression = compression
				var output bytes.Buffer
				report, err := nifti.WriteFiles(context.Background(), &output, orientationVolume(t, fixture), options)
				if err != nil {
					t.Fatalf("WriteFiles() error = %v", err)
				}
				volume := mustRead(t, output.Bytes(), nifti.ReadOptions{})
				if volume.Dimensions != report.Dimensions {
					t.Fatalf("dimensions = %v, want %v", volume.Dimensions, report.Dimensions)
				}
				geometry := volume.Geometry
				if geometry.Source != nifti.XformSourceSForm || geometry.Code != nifti.XformScannerAnat || !geometry.QFormSFormAgree {
					t.Fatalf("geometry source %q code %v agree %v, want agreeing scanner sform", geometry.Source, geometry.Code, geometry.QFormSFormAgree)
				}
				if !affineNear(geometry.IndexToRAS, report.IndexToRAS, 2e-6) {
					t.Fatalf("IndexToRAS = %v, want %v", geometry.IndexToRAS, report.IndexToRAS)
				}
				if len(volume.Warnings) != 0 {
					t.Fatalf("warnings = %v, want none", volume.Warnings)
				}
				row := render.Vec3{X: fixture.orientation[0], Y: fixture.orientation[1], Z: fixture.orientation[2]}
				column := render.Vec3{X: fixture.orientation[3], Y: fixture.orientation[4], Z: fixture.orientation[5]}
				step := render.Vec3{X: fixture.sliceStep[0], Y: fixture.sliceStep[1], Z: fixture.sliceStep[2]}
				slices := volume.SliceGeometries()
				if len(slices) != 2 {
					t.Fatalf("slice count = %d, want 2", len(slices))
				}
				for k, slice := range slices {
					if !vecNear(slice.Origin, step.Scale(float64(k)), 1e-5) ||
						!vecNear(slice.RowDir, row, 1e-6) || !vecNear(slice.ColDir, column, 1e-6) ||
						math.Abs(slice.ColSpacing-1) > 1e-6 || math.Abs(slice.RowSpacing-2) > 1e-6 ||
						slice.Rows != 1 || slice.Columns != 2 {
						t.Fatalf("slice %d geometry = %+v", k, slice)
					}
				}
				volumeGeometry := volume.VolumeGeometry(nifti.StrictGeometryTolerances())
				if !volumeGeometry.Regular || volumeGeometry.ReversedInput {
					t.Fatalf("VolumeGeometry regular %v reversed %v", volumeGeometry.Regular, volumeGeometry.ReversedInput)
				}
				if got := volumeValues(volume); !equalValues(got, []float64{1, 2, 3, 4}) {
					t.Fatalf("values = %v, want [1 2 3 4]", got)
				}

				qformOnly := mustRead(t, output.Bytes(), nifti.ReadOptions{Xform: nifti.XformQForm})
				if qformOnly.Geometry.Source != nifti.XformSourceQForm || !affineNear(qformOnly.Geometry.IndexToLPS, geometry.IndexToLPS, 2e-6) {
					t.Fatalf("qform geometry = %v, want %v", qformOnly.Geometry.IndexToLPS, geometry.IndexToLPS)
				}
			})
		}
	}
}

func TestReadRoundTripsExporterScaling(t *testing.T) {
	files := []*object.File{
		testSlice(t, imageSpec{position: [3]float64{0, 0, 0}, pixels: []byte{5, 6}, slope: float64Pointer(2), intercept: float64Pointer(-10)}),
		testSlice(t, imageSpec{position: [3]float64{0, 0, 2}, pixels: []byte{7, 8}, slope: float64Pointer(2), intercept: float64Pointer(-10)}),
	}
	for _, test := range []struct {
		name     string
		policy   nifti.ScalingPolicy
		datatype int16
		applied  bool
	}{
		{name: "preserve", policy: nifti.ScalingPreserveUniform, datatype: nifti.DatatypeUint8, applied: true},
		{name: "float32", policy: nifti.ScalingApplyFloat32, datatype: nifti.DatatypeFloat32},
		{name: "float64", policy: nifti.ScalingApplyFloat64, datatype: nifti.DatatypeFloat64},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := nifti.DefaultOptions()
			options.Scaling = test.policy
			var output bytes.Buffer
			if _, err := nifti.WriteFiles(context.Background(), &output, files, options); err != nil {
				t.Fatalf("WriteFiles() error = %v", err)
			}
			volume := mustRead(t, output.Bytes(), nifti.ReadOptions{})
			if volume.Header.Datatype != test.datatype || volume.Scaling.Applied != test.applied {
				t.Fatalf("datatype %d scaling %+v", volume.Header.Datatype, volume.Scaling)
			}
			if got := volumeValues(volume); !equalValues(got, []float64{0, 2, 4, 6}) {
				t.Fatalf("modality values = %v, want [0 2 4 6]", got)
			}
		})
	}
}

func TestReadDatatypesAndByteOrders(t *testing.T) {
	values := map[int16][]float64{
		nifti.DatatypeUint8:   {0, 7, 255, 128},
		nifti.DatatypeInt8:    {-128, -1, 0, 127},
		nifti.DatatypeInt16:   {-32768, -2, 3, 32767},
		nifti.DatatypeUint16:  {0, 1, 40000, 65535},
		nifti.DatatypeInt32:   {math.MinInt32, -5, 6, math.MaxInt32},
		nifti.DatatypeUint32:  {0, 1, 3_000_000_000, math.MaxUint32},
		nifti.DatatypeFloat32: {-1.5, 0.25, 1048576.5, -7},
		nifti.DatatypeFloat64: {-1.5, 1e-300, 3e300, math.Pi},
	}
	for datatype, want := range values {
		for _, version := range []int{1, 2} {
			for _, bigEndian := range []bool{false, true} {
				spec := defaultSynth()
				spec.version, spec.bigEndian, spec.datatype = version, bigEndian, datatype
				spec.dims = []int64{2, 2, 1}
				spec.qformCode = 1
				volume := mustRead(t, synthFile(spec, want), nifti.ReadOptions{})
				if volume.Header.Version != version || volume.Header.BigEndian != bigEndian {
					t.Fatalf("header version %d big-endian %v", volume.Header.Version, volume.Header.BigEndian)
				}
				if got := volumeValues(volume); !equalValues(got, want) {
					t.Fatalf("datatype %d v%d big-endian %v values = %v, want %v", datatype, version, bigEndian, got, want)
				}
			}
		}
	}
}

func TestReadRefusesUnsupportedDatatypesWithTypedErrors(t *testing.T) {
	// binary, complex64, RGB24, int64, uint64, float128, complex128,
	// complex256, RGBA32 and DT_UNKNOWN.
	for _, datatype := range []int16{1, 32, 128, 1024, 1280, 1536, 1792, 2048, 2304, 0} {
		spec := defaultSynth()
		spec.datatype = datatype
		spec.qformCode = 1
		_, err := readBytes(t, synthFile(spec, nil), nifti.ReadOptions{})
		readErr := requireReadError(t, err, nifti.ReadCodeDatatype, nifti.ErrUnsupportedDatatype)
		if readErr.Datatype != datatype {
			t.Fatalf("ReadError.Datatype = %d, want %d", readErr.Datatype, datatype)
		}
	}
}

func testRotation() ([3]float64, [3][3]float64) {
	return axisAngleQuaternion(render.Vec3{X: 1, Y: 2, Z: 3}, 0.4)
}

func expectedRAS(r [3][3]float64, spacing, offset [3]float64) render.GeometryAffine {
	rows := sformRows(r, spacing, offset)
	return render.GeometryAffine{
		rows[0][0], rows[0][1], rows[0][2], rows[0][3],
		rows[1][0], rows[1][1], rows[1][2], rows[1][3],
		rows[2][0], rows[2][1], rows[2][2], rows[2][3],
		0, 0, 0, 1,
	}
}

// TestReadXformPrecedence covers handcrafted third-party headers: qform only,
// sform only, both agreeing and both with different codes.
func TestReadXformPrecedence(t *testing.T) {
	quaternion, rotation := testRotation()
	_, other := axisAngleQuaternion(render.Vec3{X: 0, Y: 0, Z: 1}, 0.9)
	spacing := [3]float64{0.8, 0.9, 2.5}
	offset := [3]float64{-100.5, 20.25, 33}
	otherOffset := [3]float64{10, -20, 30}
	base := defaultSynth()
	base.pixdim = [8]float64{1, spacing[0], spacing[1], spacing[2], 1, 1, 1, 1}
	base.quatern, base.qoffset = quaternion, offset
	qformRAS := expectedRAS(rotation, spacing, offset)
	otherRAS := expectedRAS(other, spacing, otherOffset)
	sameSForm := sformRows(rotation, spacing, offset)
	otherSForm := sformRows(other, spacing, otherOffset)
	shearedSForm := sameSForm
	shearedSForm[0][1] += 0.5

	for _, test := range []struct {
		name      string
		qform     int
		sform     int
		srow      [3][4]float64
		badPixdim bool
		options   nifti.ReadOptions
		source    nifti.XformSource
		code      nifti.XformCode
		want      render.GeometryAffine
		agree     bool
		warning   string
		sentinel  error
		errorCode nifti.ReadErrorCode
	}{
		{name: "qform only", qform: 1, source: nifti.XformSourceQForm, code: 1, want: qformRAS},
		{name: "sform only aligned", sform: 2, srow: sameSForm, source: nifti.XformSourceSForm, code: 2, want: qformRAS, warning: nifti.WarningNonScannerCoordinates},
		{name: "both agree", qform: 1, sform: 1, srow: sameSForm, source: nifti.XformSourceSForm, code: 1, want: qformRAS, agree: true},
		{name: "agreeing template sform", qform: 1, sform: 4, srow: sameSForm, source: nifti.XformSourceSForm, code: 4, want: qformRAS, agree: true},
		{name: "scanner sform wins over different qform", qform: 1, sform: 1, srow: otherSForm, source: nifti.XformSourceSForm, code: 1, want: otherRAS},
		{name: "different MNI sform ignored", qform: 1, sform: 4, srow: otherSForm, source: nifti.XformSourceQForm, code: 1, want: qformRAS, warning: nifti.WarningSFormIgnored},
		{name: "different aligned sform ignored", qform: 1, sform: 2, srow: otherSForm, source: nifti.XformSourceQForm, code: 1, want: qformRAS, warning: nifti.WarningSFormIgnored},
		{name: "non-orthogonal sform falls back to qform", qform: 1, sform: 1, srow: shearedSForm, source: nifti.XformSourceQForm, code: 1, want: qformRAS, warning: nifti.WarningSFormNonOrthogonal},
		{name: "invalid qform falls back to sform", qform: 1, sform: 2, srow: otherSForm, badPixdim: true, source: nifti.XformSourceSForm, code: 2, want: otherRAS, warning: nifti.WarningQFormInvalid},
		{name: "forced qform", qform: 1, sform: 1, srow: otherSForm, options: nifti.ReadOptions{Xform: nifti.XformQForm}, source: nifti.XformSourceQForm, code: 1, want: qformRAS},
		{name: "forced sform", qform: 1, sform: 4, srow: otherSForm, options: nifti.ReadOptions{Xform: nifti.XformSForm}, source: nifti.XformSourceSForm, code: 4, want: otherRAS},
		{name: "forced missing qform", sform: 1, srow: sameSForm, options: nifti.ReadOptions{Xform: nifti.XformQForm}, errorCode: nifti.ReadCodeGeometry, sentinel: nifti.ErrNoSpatialTransform},
		{name: "forced missing sform", qform: 1, options: nifti.ReadOptions{Xform: nifti.XformSForm}, errorCode: nifti.ReadCodeGeometry, sentinel: nifti.ErrNoSpatialTransform},
		{name: "no transform", errorCode: nifti.ReadCodeGeometry, sentinel: nifti.ErrNoSpatialTransform},
		{name: "no transform allowed", options: nifti.ReadOptions{AllowMissingTransform: true}, source: nifti.XformSourcePixDim, want: render.GeometryAffine{0.8, 0, 0, 0, 0, 0.9, 0, 0, 0, 0, 2.5, 0, 0, 0, 0, 1}, warning: nifti.WarningNoSpatialTransform},
		{name: "invalid code", qform: 9, errorCode: nifti.ReadCodeHeader, sentinel: nifti.ErrMalformedHeader},
		{name: "invalid qform only", qform: 1, badPixdim: true, errorCode: nifti.ReadCodeGeometry, sentinel: nifti.ErrInvalidTransform},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := base
			spec.qformCode, spec.sformCode, spec.srow = test.qform, test.sform, test.srow
			if test.badPixdim {
				spec.pixdim[1] = 0
			}
			volume, err := readBytes(t, synthFile(spec, []float64{1, 2, 3, 4}), test.options)
			if test.sentinel != nil {
				requireReadError(t, err, test.errorCode, test.sentinel)
				return
			}
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			geometry := volume.Geometry
			if geometry.Source != test.source || geometry.Code != test.code || geometry.QFormSFormAgree != test.agree {
				t.Fatalf("source %q code %v agree %v, want %q %v %v", geometry.Source, geometry.Code, geometry.QFormSFormAgree, test.source, test.code, test.agree)
			}
			if !affineNear(geometry.IndexToRAS, test.want, 2e-5) {
				t.Fatalf("IndexToRAS = %v, want %v", geometry.IndexToRAS, test.want)
			}
			lps := test.want
			for column := 0; column < 4; column++ {
				lps[column], lps[4+column] = -lps[column], -lps[4+column]
			}
			if !affineNear(geometry.IndexToLPS, lps, 2e-5) {
				t.Fatalf("IndexToLPS = %v, want %v", geometry.IndexToLPS, lps)
			}
			if test.warning != "" && !containsString(volume.Warnings, test.warning) {
				t.Fatalf("warnings = %v, want %q", volume.Warnings, test.warning)
			}
		})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestReadQFacAndFlippedSFormAreLeftHanded(t *testing.T) {
	spec := defaultSynth()
	spec.dims = []int64{1, 1, 3}
	spec.qformCode = 1
	spec.pixdim = [8]float64{-1, 1, 1, 2, 1, 1, 1, 1}
	volume := mustRead(t, synthFile(spec, []float64{1, 2, 3}), nifti.ReadOptions{})
	if !volume.Geometry.LeftHanded || volume.Geometry.IndexToRAS[10] != -2 {
		t.Fatalf("qfac -1 geometry = %+v", volume.Geometry)
	}
	geometry := volume.VolumeGeometry(nifti.StrictGeometryTolerances())
	if !geometry.ReversedInput || !geometry.Regular {
		t.Fatalf("left-handed volume reversed %v regular %v", geometry.ReversedInput, geometry.Regular)
	}

	spec.qformCode, spec.sformCode = 0, 1
	spec.srow = [3][4]float64{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, -2, 5}}
	volume = mustRead(t, synthFile(spec, []float64{1, 2, 3}), nifti.ReadOptions{})
	if !volume.Geometry.LeftHanded || volume.Geometry.Spacing != [3]float64{1, 1, 2} {
		t.Fatalf("flipped sform geometry = %+v", volume.Geometry)
	}
}

func TestReadConvertsSpatialUnitsToMillimeters(t *testing.T) {
	for _, test := range []struct {
		units uint8
		scale float64
	}{
		{units: 0, scale: 1},
		{units: 1, scale: 1000},
		{units: 2 | 8, scale: 1},
		{units: 3, scale: 0.001},
	} {
		spec := defaultSynth()
		spec.xyztUnits = test.units
		spec.sformCode = 1
		spec.srow = [3][4]float64{{0.5, 0, 0, 1}, {0, 0.25, 0, 2}, {0, 0, 2, 3}}
		spec.pixdim = [8]float64{1, 0.5, 0.25, 2, 1, 1, 1, 1}
		volume := mustRead(t, synthFile(spec, []float64{1, 2, 3, 4}), nifti.ReadOptions{})
		want := render.GeometryAffine{0.5, 0, 0, 1, 0, 0.25, 0, 2, 0, 0, 2, 3, 0, 0, 0, 1}
		for index := 0; index < 12; index++ {
			want[index] *= test.scale
		}
		if !affineNear(volume.Geometry.IndexToRAS, want, 1e-9) || math.Abs(volume.Geometry.Spacing[2]-2*test.scale) > 1e-9 {
			t.Fatalf("units %d affine %v spacing %v, want %v", test.units, volume.Geometry.IndexToRAS, volume.Geometry.Spacing, want)
		}
	}
}

func TestReadFourDimensionalVolume(t *testing.T) {
	spec := defaultSynth()
	spec.dims = []int64{2, 1, 2, 3}
	spec.qformCode = 1
	spec.datatype = nifti.DatatypeInt16
	values := []float64{1, 2, 3, 4, 11, 12, 13, 14, 21, 22, 23, 24}
	volume := mustRead(t, synthFile(spec, values), nifti.ReadOptions{})
	if volume.Dimensions != [4]int{2, 1, 2, 3} {
		t.Fatalf("dimensions = %v", volume.Dimensions)
	}
	if got := volume.SliceValues(1, 2, nil); !equalValues(got, []float64{23, 24}) {
		t.Fatalf("SliceValues(1, 2) = %v, want [23 24]", got)
	}

	spec.dims = []int64{2, 1, 1, 1, 3}
	_, err := readBytes(t, synthFile(spec, make([]float64, 6)), nifti.ReadOptions{})
	requireReadError(t, err, nifti.ReadCodeDimensions, nifti.ErrUnsupportedDimensions)
}

func TestReadNIfTI2AndExtensionsAndOffsets(t *testing.T) {
	spec := defaultSynth()
	spec.version = 2
	spec.qformCode = 1
	spec.extension = append([]byte{32, 0, 0, 0, 6, 0, 0, 0}, make([]byte, 24)...)
	volume := mustRead(t, synthFile(spec, []float64{9, 8, 7, 6}), nifti.ReadOptions{})
	if !volume.Header.Extended || volume.Header.Version != 2 || !equalValues(volumeValues(volume), []float64{9, 8, 7, 6}) {
		t.Fatalf("NIfTI-2 extended read = header %+v values %v", volume.Header, volumeValues(volume))
	}

	legacy := defaultSynth()
	legacy.qformCode = 1
	legacy.voxOffset = 0
	volume = mustRead(t, synthFile(legacy, []float64{1, 2, 3, 4}), nifti.ReadOptions{})
	if !containsString(volume.Warnings, nifti.WarningVoxelOffsetAdjusted) || !equalValues(volumeValues(volume), []float64{1, 2, 3, 4}) {
		t.Fatalf("vox_offset 0 read warnings %v values %v", volume.Warnings, volumeValues(volume))
	}

	trailing := append(synthFile(legacy, []float64{1, 2, 3, 4}), 0xff)
	volume = mustRead(t, trailing, nifti.ReadOptions{})
	if !containsString(volume.Warnings, nifti.WarningTrailingData) {
		t.Fatalf("trailing data warnings = %v", volume.Warnings)
	}

	fractional := defaultSynth()
	fractional.qformCode = 1
	fractional.voxOffset = 352.5
	_, err := readBytes(t, synthFile(fractional, []float64{1, 2, 3, 4}), nifti.ReadOptions{})
	requireReadError(t, err, nifti.ReadCodeHeader, nifti.ErrMalformedHeader)
}

func TestReadGZIPAndPairedFiles(t *testing.T) {
	spec := defaultSynth()
	spec.qformCode = 1
	spec.datatype = nifti.DatatypeInt16
	spec.bigEndian = true
	values := []float64{-1, 2, -3, 4}
	directory := t.TempDir()

	single := filepath.Join(directory, "single.nii.gz")
	if err := os.WriteFile(single, gzipBytes(t, synthFile(spec, values)), 0o600); err != nil {
		t.Fatal(err)
	}
	volume, err := nifti.ReadFile(context.Background(), single, nifti.ReadOptions{})
	if err != nil || !equalValues(volumeValues(volume), values) {
		t.Fatalf("ReadFile(.nii.gz) = %v, %v", volume, err)
	}

	paired := spec
	paired.paired = true
	header := synthHeaderBytes(paired)
	voxels := synthVoxels(paired, values)
	for _, variant := range []struct {
		name          string
		header, image string
		gzipImage     bool
	}{
		{name: "plain", header: "pair.hdr", image: "pair.img"},
		{name: "gzip image", header: "zpair.hdr", image: "zpair.img.gz", gzipImage: true},
	} {
		t.Run(variant.name, func(t *testing.T) {
			image := voxels
			if variant.gzipImage {
				image = gzipBytes(t, voxels)
			}
			if err := os.WriteFile(filepath.Join(directory, variant.header), header, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, variant.image), image, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, entry := range []string{variant.header, variant.image} {
				volume, err := nifti.ReadFile(context.Background(), filepath.Join(directory, entry), nifti.ReadOptions{})
				if err != nil {
					t.Fatalf("ReadFile(%s) error = %v", entry, err)
				}
				if !volume.Header.Paired || !equalValues(volumeValues(volume), values) {
					t.Fatalf("ReadFile(%s) paired %v values %v", entry, volume.Header.Paired, volumeValues(volume))
				}
			}
		})
	}

	_, err = readBytes(t, append(header, voxels...), nifti.ReadOptions{})
	requireReadError(t, err, nifti.ReadCodeFormat, nifti.ErrPairedFile)

	missing := filepath.Join(directory, "patient-name-missing.hdr")
	if err := os.WriteFile(missing, header, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = nifti.ReadFile(context.Background(), missing, nifti.ReadOptions{})
	requireReadError(t, err, nifti.ReadCodeIO, nifti.ErrInvalidSource)
	if strings.Contains(err.Error(), "patient-name") || strings.Contains(err.Error(), directory) {
		t.Fatalf("error %q echoes the path", err)
	}
}

func TestReadRejectsMalformedAndOversizedInput(t *testing.T) {
	good := defaultSynth()
	good.qformCode = 1
	valid := synthFile(good, []float64{1, 2, 3, 4})

	analyze := append([]byte(nil), valid...)
	copy(analyze[344:348], "\x00\x00\x00\x00")
	corruptedV2 := defaultSynth()
	corruptedV2.version = 2
	corruptedV2.magic = "n+2\x00\n\x1a\n\x00"
	badDims := append([]byte(nil), valid...)
	badDims[40] = 9
	negativeDim := defaultSynth()
	negativeDim.dims = []int64{2, -1, 2}
	gzipCorrupt := gzipBytes(t, valid)
	gzipCorrupt[len(gzipCorrupt)-6] ^= 0xff
	hugeOffset := defaultSynth()
	hugeOffset.qformCode = 1
	hugeOffset.voxOffset = 1 << 30

	for _, test := range []struct {
		name     string
		data     []byte
		options  nifti.ReadOptions
		code     nifti.ReadErrorCode
		sentinel error
	}{
		{name: "empty", data: nil, code: nifti.ReadCodeTruncated, sentinel: nifti.ErrTruncatedData},
		{name: "not nifti", data: bytes.Repeat([]byte{7}, 400), code: nifti.ReadCodeFormat, sentinel: nifti.ErrNotNIfTI},
		{name: "analyze 7.5", data: analyze, code: nifti.ReadCodeFormat, sentinel: nifti.ErrNotNIfTI},
		{name: "nifti2 newline corruption", data: synthFile(corruptedV2, []float64{1, 2, 3, 4}), code: nifti.ReadCodeHeader, sentinel: nifti.ErrMalformedHeader},
		{name: "dim[0] out of range", data: badDims, code: nifti.ReadCodeDimensions, sentinel: nifti.ErrMalformedHeader},
		{name: "negative dimension", data: synthFile(negativeDim, nil), code: nifti.ReadCodeDimensions, sentinel: nifti.ErrMalformedHeader},
		{name: "truncated header", data: valid[:200], code: nifti.ReadCodeTruncated, sentinel: nifti.ErrTruncatedData},
		{name: "truncated voxels", data: valid[:len(valid)-1], code: nifti.ReadCodeTruncated, sentinel: nifti.ErrTruncatedData},
		{name: "gzip checksum", data: gzipCorrupt, code: nifti.ReadCodeIO, sentinel: nifti.ErrInvalidSource},
		{name: "voxel limit", data: valid, options: nifti.ReadOptions{Limits: nifti.ReadLimits{MaxVoxels: 3}}, code: nifti.ReadCodeLimit, sentinel: nifti.ErrLimitExceeded},
		{name: "payload limit", data: valid, options: nifti.ReadOptions{Limits: nifti.ReadLimits{MaxPayloadBytes: 3}}, code: nifti.ReadCodeLimit, sentinel: nifti.ErrLimitExceeded},
		{name: "header limit", data: synthFile(hugeOffset, nil), code: nifti.ReadCodeLimit, sentinel: nifti.ErrLimitExceeded},
		{name: "bad policy", data: valid, options: nifti.ReadOptions{Xform: 9}, code: nifti.ReadCodeGeometry, sentinel: nifti.ErrInvalidTransform},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := readBytes(t, test.data, test.options)
			requireReadError(t, err, test.code, test.sentinel)
		})
	}

	// A header that claims a 2 GiB payload but carries four bytes fails as
	// truncated after committing only the bytes that arrived.
	liar := defaultSynth()
	liar.qformCode = 1
	liar.dims = []int64{32767, 32767, 2}
	_, err := readBytes(t, synthFile(liar, []float64{1, 2, 3, 4}), nifti.ReadOptions{})
	requireReadError(t, err, nifti.ReadCodeTruncated, nifti.ErrTruncatedData)
}

func TestReadHonorsCancellation(t *testing.T) {
	spec := defaultSynth()
	spec.qformCode = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := nifti.Read(ctx, bytes.NewReader(synthFile(spec, []float64{1, 2, 3, 4})), nifti.ReadOptions{})
	requireReadError(t, err, nifti.ReadCodeCanceled, context.Canceled)
}
