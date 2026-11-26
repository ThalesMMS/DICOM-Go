package nifti_test

// Scenarios ported from ITK (Apache-2.0) NIfTI reader tests. ITK's inputs
// (LPSLabels*.nii.gz, NonOrthoSform.nii.gz, SmallVoxels*.nii.gz) are external
// data; the headers here are synthesized from the same descriptions, and only
// the assertions are ported. No ITK code or data is copied.

import (
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

type itkDirectionFixture struct {
	both, noQForm, noSForm, nonOrthoSForm []byte
	rotation                              [3]render.Vec3
}

// itkDirectionFixtures mirrors the four inputs of ITK's
// itkNiftiReadWriteDirectionTest: a file with both transforms, the same file
// without qform, without sform, and a sheared sform without qform.
func itkDirectionFixtures(spacing [3]float64, shear float64) itkDirectionFixture {
	quaternion, r := axisAngleQuaternion(render.Vec3{X: 0.3, Y: -0.5, Z: 0.8}, 2.1)
	offset := [3]float64{-91.25, 126.5, -72.75}
	base := defaultSynth()
	base.dims = []int64{2, 2, 2}
	base.pixdim = [8]float64{1, spacing[0], spacing[1], spacing[2], 1, 1, 1, 1}
	base.quatern, base.qoffset = quaternion, offset
	base.srow = sformRows(r, spacing, offset)
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8}

	both := base
	both.qformCode, both.sformCode = 1, 1
	noQForm := base
	noQForm.sformCode = 1
	noSForm := base
	noSForm.qformCode = 1
	nonOrtho := noQForm
	// Shear the second and third columns towards the first ones.
	for row := 0; row < 3; row++ {
		nonOrtho.srow[row][1] += shear * r[row][0] * spacing[1]
		nonOrtho.srow[row][2] += shear * r[row][1] * spacing[2]
	}
	var rotation [3]render.Vec3
	for column := 0; column < 3; column++ {
		rotation[column] = render.Vec3{X: r[0][column], Y: r[1][column], Z: r[2][column]}
	}
	return itkDirectionFixture{
		both:          synthFile(both, values),
		noQForm:       synthFile(noQForm, values),
		noSForm:       synthFile(noSForm, values),
		nonOrthoSForm: synthFile(nonOrtho, values),
		rotation:      rotation,
	}
}

// TestReadITKPrefersSFormOverLossyQForm ports ITK 34231b5702 ("Prefer to use
// sform over qform when both are set") and the first half of
// itkNiftiReadWriteDirectionTest: with both transforms the orthonormal
// scanner sform is used, so the direction equals the sform-only file exactly
// and differs from the lossy quaternion-only file.
func TestReadITKPrefersSFormOverLossyQForm(t *testing.T) {
	for _, test := range []struct {
		name    string
		spacing [3]float64
	}{
		{name: "LPSLabels", spacing: [3]float64{0.7, 0.7, 1.2}},
		// ITK 6278c4171f: SmallVoxels*.nii.gz, a determinant far below 1e-5.
		{name: "SmallVoxels", spacing: [3]float64{0.005, 0.005, 0.01}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := itkDirectionFixtures(test.spacing, 0.1)
			both := mustRead(t, fixture.both, nifti.ReadOptions{})
			noQForm := mustRead(t, fixture.noQForm, nifti.ReadOptions{})
			noSForm := mustRead(t, fixture.noSForm, nifti.ReadOptions{})
			for name, volume := range map[string]*nifti.Volume{"both": both, "noqform": noQForm, "nosform": noSForm} {
				if !orthonormal(directionColumns(volume.Geometry.IndexToLPS), 1e-4) {
					t.Fatalf("%s rotation is not orthonormal", name)
				}
				if volume.Geometry.SFormCorrected {
					t.Fatalf("%s SFormCorrected = true, want false", name)
				}
				for axis := 0; axis < 3; axis++ {
					if !nearlyRelative(volume.Geometry.Spacing[axis], test.spacing[axis], 1e-6) {
						t.Fatalf("%s spacing = %v, want %v", name, volume.Geometry.Spacing, test.spacing)
					}
				}
			}
			if both.Geometry.Source != nifti.XformSourceSForm || !both.Geometry.QFormSFormAgree {
				t.Fatalf("both: source %q agree %v, want agreeing sform", both.Geometry.Source, both.Geometry.QFormSFormAgree)
			}
			if both.Geometry.IndexToLPS != noQForm.Geometry.IndexToLPS {
				t.Fatalf("both-forms direction %v differs from sform-only %v", both.Geometry.IndexToLPS, noQForm.Geometry.IndexToLPS)
			}
			if noSForm.Geometry.Source != nifti.XformSourceQForm {
				t.Fatalf("qform-only source = %q", noSForm.Geometry.Source)
			}
			if both.Geometry.IndexToLPS == noSForm.Geometry.IndexToLPS {
				t.Fatal("sform and lossy qform directions are bitwise equal; the fixture no longer exercises precedence")
			}
			if angle := rotationAngleDegrees(directionColumns(both.Geometry.IndexToLPS), directionColumns(noSForm.Geometry.IndexToLPS)); angle > 1e-3 {
				t.Fatalf("sform/qform direction angle = %v degrees", angle)
			}
		})
	}
}

// TestReadITKNonOrthogonalSFormPermissive ports ITK 5cdf6ee6c9 ("Added
// permissive mode to NIFTI reader") and 71449a713c ("Non-ortho sform
// tolerance now user specified"): a sheared sform without qform is refused by
// default, accepted in permissive mode with the correction flagged, yields an
// orthonormal rotation, and stays within 8 degrees (4 degrees for the small
// voxel fixture) of the unsheared direction.
func TestReadITKNonOrthogonalSFormPermissive(t *testing.T) {
	for _, test := range []struct {
		name      string
		spacing   [3]float64
		shear     float64
		threshold float64
	}{
		{name: "NonOrthoSform", spacing: [3]float64{0.7, 0.7, 1.2}, shear: 0.12, threshold: 8},
		{name: "SmallVoxelsNonOrthoSform", spacing: [3]float64{0.005, 0.005, 0.01}, shear: 0.05, threshold: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := itkDirectionFixtures(test.spacing, test.shear)
			_, err := readBytes(t, fixture.nonOrthoSForm, nifti.ReadOptions{})
			requireReadError(t, err, nifti.ReadCodeGeometry, nifti.ErrNonOrthogonalSForm)

			volume := mustRead(t, fixture.nonOrthoSForm, nifti.ReadOptions{SFormPermissive: true})
			geometry := volume.Geometry
			if !geometry.SFormCorrected || geometry.Source != nifti.XformSourceSFormOrthogonalized ||
				!containsString(volume.Warnings, nifti.WarningSFormOrthogonalized) {
				t.Fatalf("permissive geometry = %+v warnings %v", geometry, volume.Warnings)
			}
			directions := directionColumns(geometry.IndexToLPS)
			if !orthonormal(directions, 1e-4) {
				t.Fatalf("corrected rotation is not orthonormal: %v", directions)
			}
			truth := fixture.rotation
			for column := range truth {
				truth[column].X, truth[column].Y = -truth[column].X, -truth[column].Y
			}
			if angle := rotationAngleDegrees(directions, truth); angle > test.threshold {
				t.Fatalf("corrected direction is %v degrees from the unsheared one, threshold %v", angle, test.threshold)
			}
			if !(geometry.SFormDeviationDegrees > 0) || geometry.SFormDeviationDegrees > nifti.DefaultPermissiveMaxDeviationDegrees {
				t.Fatalf("SFormDeviationDegrees = %v", geometry.SFormDeviationDegrees)
			}

			_, err = readBytes(t, fixture.nonOrthoSForm, nifti.ReadOptions{SFormPermissive: true, PermissiveMaxDeviationDegrees: 0.01})
			requireReadError(t, err, nifti.ReadCodeGeometry, nifti.ErrNonOrthogonalSForm)
		})
	}
}

// TestReadITKSmallVoxelSFormIsInvertible ports ITK 6278c4171f ("Fix matrix
// check rejecting NIFTI sform for small voxels"): invertibility is decided
// scale-free, so a 1 micrometer sform-only grid keeps its directions, while a
// genuinely singular sform is still refused.
func TestReadITKSmallVoxelSFormIsInvertible(t *testing.T) {
	_, r := axisAngleQuaternion(render.Vec3{X: 1, Y: 1, Z: 0}, 0.3)
	spacing := [3]float64{0.001, 0.001, 0.002}
	spec := defaultSynth()
	spec.sformCode = 1
	spec.srow = sformRows(r, spacing, [3]float64{1, 2, 3})
	spec.pixdim = [8]float64{1, spacing[0], spacing[1], spacing[2], 1, 1, 1, 1}
	volume := mustRead(t, synthFile(spec, []float64{1, 2, 3, 4}), nifti.ReadOptions{})
	if volume.Geometry.Source != nifti.XformSourceSForm {
		t.Fatalf("small voxel source = %q", volume.Geometry.Source)
	}
	for axis := 0; axis < 3; axis++ {
		if !nearlyRelative(volume.Geometry.Spacing[axis], spacing[axis], 1e-6) {
			t.Fatalf("spacing = %v, want %v", volume.Geometry.Spacing, spacing)
		}
	}

	singular := spec
	singular.srow = [3][4]float64{{0.001, 0.001, 0, 0}, {0, 0, 0, 0}, {0, 0, 0.002, 0}}
	_, err := readBytes(t, synthFile(singular, []float64{1, 2, 3, 4}), nifti.ReadOptions{SFormPermissive: true})
	requireReadError(t, err, nifti.ReadCodeGeometry, nifti.ErrInvalidTransform)
}

// TestReadITKNonFiniteVoxelsSurvive ports ITK 0d8c703921 ("Preserve
// non-finite pixel values when reading NIfTI images",
// NonFiniteFloatPixelsSurviveRoundTrip / NonFiniteDoublePixelsSurviveRoundTrip):
// NaN and infinities are read as stored, and scaling keeps them non-finite.
func TestReadITKNonFiniteVoxelsSurvive(t *testing.T) {
	for _, datatype := range []int16{nifti.DatatypeFloat32, nifti.DatatypeFloat64} {
		for _, scaled := range []bool{false, true} {
			spec := defaultSynth()
			spec.dims = []int64{4, 1, 1}
			spec.datatype = datatype
			spec.qformCode = 1
			if scaled {
				spec.slope, spec.inter = 2, 1
			}
			volume := mustRead(t, synthFile(spec, []float64{1.5, math.NaN(), math.Inf(1), math.Inf(-1)}), nifti.ReadOptions{})
			want := []float64{1.5, math.NaN(), math.Inf(1), math.Inf(-1)}
			if scaled {
				want[0] = 4
			}
			if got := volumeValues(volume); !equalValues(got, want) {
				t.Fatalf("datatype %d scaled %v values = %v, want %v", datatype, scaled, got, want)
			}
			if !math.IsNaN(volume.Stored(1)) {
				t.Fatalf("Stored(1) = %v, want NaN", volume.Stored(1))
			}
		}
	}
}

// TestReadITKSclSlopeKeepsFullPrecision ports ITK a6fb755498
// (SclSlopeInterceptRoundTripsAtFullPrecision): a float32 slope of 1/3 and
// intercept 5.333333 are recovered exactly and applied to every voxel.
func TestReadITKSclSlopeKeepsFullPrecision(t *testing.T) {
	slope, intercept := float32(1.0/3.0), float32(5.333333)
	spec := defaultSynth()
	spec.dims = []int64{10, 10, 10}
	spec.qformCode = 1
	spec.slope, spec.inter = float64(slope), float64(intercept)
	values := make([]float64, 1000)
	for index := range values {
		values[index] = 231
	}
	volume := mustRead(t, synthFile(spec, values), nifti.ReadOptions{})
	if volume.Scaling.Slope != float64(slope) || volume.Scaling.Intercept != float64(intercept) || !volume.Scaling.Applied {
		t.Fatalf("scaling = %+v, want slope %v intercept %v", volume.Scaling, slope, intercept)
	}
	want := 231*float64(slope) + float64(intercept)
	for index := 0; index < volume.Len(); index++ {
		got := volume.Value(index)
		if got != want || float32(got) != float32(231)*slope+intercept {
			t.Fatalf("voxel %d = %v, want %v", index, got, want)
		}
	}

	// nifti_clib semantics: a zero or non-finite slope disables scaling and
	// a non-finite intercept reads as zero.
	for _, test := range []struct{ slope, inter float64 }{{0, 7}, {math.NaN(), 7}, {math.Inf(1), 7}} {
		disabled := spec
		disabled.dims = []int64{1, 1, 1}
		disabled.slope, disabled.inter = test.slope, test.inter
		volume := mustRead(t, synthFile(disabled, []float64{9}), nifti.ReadOptions{})
		if volume.Scaling.Applied || volume.Value(0) != 9 {
			t.Fatalf("slope %v: scaling %+v value %v", test.slope, volume.Scaling, volume.Value(0))
		}
	}
	nanIntercept := spec
	nanIntercept.dims = []int64{1, 1, 1}
	nanIntercept.slope, nanIntercept.inter = 2, math.NaN()
	volume = mustRead(t, synthFile(nanIntercept, []float64{9}), nifti.ReadOptions{})
	if volume.Value(0) != 18 {
		t.Fatalf("NaN intercept value = %v, want 18", volume.Value(0))
	}
}

func nearlyRelative(got, want, tolerance float64) bool {
	return math.Abs(got-want) <= tolerance*math.Max(math.Abs(got), math.Abs(want))
}
