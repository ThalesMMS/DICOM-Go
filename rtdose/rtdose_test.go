package rtdose

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/rtstruct"
)

func TestReadScalesSamplesAndPreservesDoseSemantics(t *testing.T) {
	file := testDoseFile(t, testDoseOptions{
		rows: 2, columns: 2, frames: 2,
		spacing: []float64{2, 3}, position: []float64{10, 20, 30},
		orientation: []float64{1, 0, 0, 0, 1, 0},
		offsets:     []float64{0, 4}, scaling: 0.01,
		values: []uint16{0, 100, 200, 300, 400, 500, 600, 700},
		units:  DoseUnitsGray, doseType: DoseTypePhysical, summation: "PLAN",
	})
	dose, err := Read(file)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if dose.Rows != 2 || dose.Columns != 2 || dose.Frames != 2 {
		t.Fatalf("grid = %dx%dx%d, want 2x2x2", dose.Columns, dose.Rows, dose.Frames)
	}
	if dose.DisplayUnit() != "Gy (physical)" || dose.SummationType != "PLAN" {
		t.Fatalf("semantics = %q/%q", dose.DisplayUnit(), dose.SummationType)
	}
	if got, ok := dose.ValueAt(1, 1, 1); !ok || got != 7 {
		t.Fatalf("ValueAt(1,1,1) = %v, %v, want 7, true", got, ok)
	}
	// Patient x follows columns with 3 mm spacing; y follows rows with 2 mm.
	// The point is halfway in all three axes, including between z=30 and z=34.
	got, ok := dose.SamplePatient(testVec(11.5, 21, 32))
	if !ok || math.Abs(got-3.5) > 1e-9 {
		t.Fatalf("SamplePatient() = %v, %v, want 3.5, true", got, ok)
	}
	if _, ok := dose.SamplePatient(testVec(100, 20, 30)); ok {
		t.Fatal("SamplePatient accepted a point outside the dose grid")
	}
	// Voxel boxes (#1115), as the DVH: the outer half voxel and half slice
	// repeat the edge values; beyond them nothing is sampled.
	for _, test := range []struct {
		x, y, z float64
		want    float64
		ok      bool
	}{
		{14.4, 20, 30, 1, true},  // column 1.467
		{14.6, 20, 30, 0, false}, // column 1.533
		{8.6, 19.1, 30, 0, true}, // column -0.467, row -0.45
		{14.4, 20, 35.9, 5, true},
		{14.4, 20, 36.1, 0, false},
		{14.4, 20, 28.1, 1, true},
		{14.4, 20, 27.9, 0, false},
	} {
		got, ok := dose.SamplePatient(testVec(test.x, test.y, test.z))
		if ok != test.ok || (ok && math.Abs(got-test.want) > 1e-9) {
			t.Errorf("SamplePatient(%v, %v, %v) = %v, %v, want %v, %v", test.x, test.y, test.z, got, ok, test.want, test.ok)
		}
	}
}

func TestSignedDoseErrorGridAndDVH(t *testing.T) {
	for _, tt := range []struct {
		name     string
		doseType string
		repr     uint16
		stored   []uint16
		want     [2]float64
	}{
		{"signed error", "ERROR ", 1, []uint16{0xfffe, 2}, [2]float64{-0.25, 0.25}},
		{"all-negative error", DoseTypeError, 1, []uint16{0xfffc, 0xfffe}, [2]float64{-0.5, -0.25}},
		{"signed physical", DoseTypePhysical, 1, []uint16{0xfffe, 2}, [2]float64{}},
		{"unsigned error", DoseTypeError, 0, []uint16{0xfffe, 2}, [2]float64{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file := testDoseFile(t, testDoseOptions{
				rows: 1, columns: 2, frames: 1, spacing: []float64{1, 1},
				position: []float64{0, 0, 0}, orientation: []float64{1, 0, 0, 0, 1, 0},
				scaling: 0.125, values: tt.stored, units: DoseUnitsGray, doseType: tt.doseType, summation: "PLAN",
			})
			file.Dataset.Put(derivedio.US(derivedio.TagPixelRepresentation, tt.repr))
			dose, err := Read(file)
			if tt.want == [2]float64{} {
				if dose != nil || !errors.Is(err, ErrInvalidObject) {
					t.Fatalf("Read = (%+v, %v), want invalid pixel representation", dose, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if dose.Values[0] != tt.want[0] || dose.Values[1] != tt.want[1] ||
				dose.Maximum != tt.want[1] || dose.DisplayUnit() != "Gy (error)" {
				t.Fatalf("Read = %v, max = %g, unit = %q", dose.Values, dose.Maximum, dose.DisplayUnit())
			}
			midpoint := (tt.want[0] + tt.want[1]) / 2
			if got, ok := dose.SamplePatient(testVec(0.5, 0, 0)); !ok || got != midpoint {
				t.Fatalf("midpoint = (%g, %t), want %g", got, ok, midpoint)
			}
			set := &rtstruct.StructureSet{
				FrameOfReferenceUID: dose.FrameOfReferenceUID,
				ROIs: []rtstruct.ROI{{Number: 7, Name: "Test ROI", Contours: []rtstruct.Contour{
					testContour(rtstruct.ContourClosedPlanar, [][3]float64{
						{-0.5, -0.5, 0}, {1.5, -0.5, 0}, {1.5, 0.5, 0}, {-0.5, 0.5, 0},
					}),
				}}},
			}
			dvh, err := ComputeDVH(context.Background(), dose, set, 7, DVHOptions{Bins: 3})
			if err != nil {
				t.Fatal(err)
			}
			if dvh.VoxelCount != 2 || dvh.Minimum != tt.want[0] || dvh.Maximum != tt.want[1] || dvh.Mean != midpoint ||
				dvh.D95 != tt.want[0] || dvh.D50 != tt.want[1] || dvh.DoseUnit != "Gy (error)" ||
				dvh.Bins[0].Dose != tt.want[0] || dvh.Bins[0].VolumePercent != 100 ||
				dvh.Bins[1].VolumePercent != 50 {
				t.Fatalf("signed DVH = %+v, want both dose errors represented", dvh)
			}
		})
	}
}

func TestReadSupportsAbsoluteAxialOffsetsAndDescendingFrames(t *testing.T) {
	file := testDoseFile(t, testDoseOptions{
		rows: 1, columns: 1, frames: 2,
		spacing: []float64{1, 1}, position: []float64{0, 0, 10},
		orientation: []float64{1, 0, 0, 0, 1, 0},
		offsets:     []float64{10, 12}, scaling: 1,
		values: []uint16{10, 12},
		units:  DoseUnitsGray, doseType: DoseTypePhysical, summation: "FRACTION",
	})
	dose, err := Read(file)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got, _ := dose.ValueAt(0, 0, 0); got != 10 {
		t.Fatalf("first sorted frame = %v, want 10", got)
	}
	if got, _ := dose.ValueAt(0, 0, 1); got != 12 {
		t.Fatalf("second sorted frame = %v, want 12", got)
	}
	if dose.Geometry.Positions[0] != 10 || dose.Geometry.Positions[1] != 12 {
		t.Fatalf("positions = %v, want [10 12]", dose.Geometry.Positions)
	}

	descending, err := Read(testDoseFile(t, testDoseOptions{
		rows: 1, columns: 1, frames: 2,
		spacing:     []float64{1, 1},
		position:    []float64{0, 0, 12},
		orientation: []float64{1, 0, 0, 0, 1, 0},
		offsets:     []float64{0, -2},
		scaling:     1,
		values:      []uint16{12, 10},
		units:       DoseUnitsGray,
		doseType:    DoseTypePhysical,
		summation:   "FRACTION",
	}))
	if err != nil {
		t.Fatalf("descending relative Read() error = %v", err)
	}
	if got, _ := descending.ValueAt(0, 0, 0); got != 10 {
		t.Fatalf("descending first sorted frame = %v, want 10", got)
	}
}

func TestReadFailsClosedOnAmbiguousGeometryAndUnsafeUnits(t *testing.T) {
	options := testDoseOptions{
		rows: 1, columns: 1, frames: 2,
		spacing: []float64{1, 1}, position: []float64{0, 0, 10},
		orientation: []float64{0, 1, 0, 0, 0, 1},
		offsets:     []float64{10, 11}, scaling: 1, values: []uint16{1, 2},
		units: DoseUnitsGray, doseType: DoseTypePhysical, summation: "PLAN",
	}
	if _, err := Read(testDoseFile(t, options)); !errors.Is(err, ErrGeometryMismatch) {
		t.Fatalf("ambiguous absolute offsets error = %v, want ErrGeometryMismatch", err)
	}
	options.orientation = []float64{1, 0, 0, 0, 1, 0}
	options.offsets = []float64{0, 1}
	options.units = "CGY"
	if _, err := Read(testDoseFile(t, options)); !errors.Is(err, ErrUnsafeSemantics) {
		t.Fatalf("unsafe units error = %v, want ErrUnsafeSemantics", err)
	}
}

func TestReadNeverLabelsRelativeOrEffectiveDoseAsPhysical(t *testing.T) {
	options := testDoseOptions{
		rows: 1, columns: 1, frames: 1,
		spacing: []float64{1, 1}, position: []float64{0, 0, 0},
		orientation: []float64{1, 0, 0, 0, 1, 0},
		offsets:     []float64{0}, scaling: 1, values: []uint16{100},
		units: DoseUnitsRelative, doseType: DoseTypePhysical, summation: "PLAN",
	}
	relative, err := Read(testDoseFile(t, options))
	if err != nil {
		t.Fatal(err)
	}
	if relative.DisplayUnit() != "relative" {
		t.Fatalf("relative DisplayUnit = %q", relative.DisplayUnit())
	}
	options.units, options.doseType = DoseUnitsGray, DoseTypeEffective
	effective, err := Read(testDoseFile(t, options))
	if err != nil {
		t.Fatal(err)
	}
	if effective.DisplayUnit() != "Gy (effective/biological)" {
		t.Fatalf("effective DisplayUnit = %q", effective.DisplayUnit())
	}
}

func TestReadEmbeddedDVHAndReferences(t *testing.T) {
	options := testDoseOptions{
		rows: 1, columns: 1, frames: 1,
		spacing: []float64{1, 1}, position: []float64{0, 0, 0},
		orientation: []float64{1, 0, 0, 0, 1, 0},
		offsets:     []float64{0}, scaling: 1, values: []uint16{5},
		units: DoseUnitsGray, doseType: DoseTypePhysical, summation: "PLAN",
		extra: []core.Element{
			derivedio.Seq(tagReferencedRTPlanSequence, derivedio.DataSet(
				derivedio.UI(derivedio.TagRefSOPClassUID, "1.2.plan.class"),
				derivedio.UI(derivedio.TagRefSOPInstanceUID, "1.2.plan"),
			)),
			derivedio.Seq(tagDVHSequence, derivedio.DataSet(
				derivedio.CS(core.NewTag(0x3004, 0x0001), "CUMULATIVE"),
				derivedio.DS(tagDVHDoseScaling, 0.01),
				derivedio.CS(tagDVHVolumeUnits, "CM3"),
				derivedio.IS(tagDVHNumberOfBins, 2),
				derivedio.DS(tagDVHData, 1, 10, 1, 4),
				derivedio.DS(tagDVHMinimumDose, 1),
				derivedio.DS(tagDVHMaximumDose, 2),
				derivedio.DS(tagDVHMeanDose, 1.5),
				derivedio.Seq(tagDVHReferencedROISequence, derivedio.DataSet(
					derivedio.IS(tagReferencedROINumber, 7),
					derivedio.CS(tagDVHROIContributionType, "INCLUDED"),
				)),
			)),
		},
	}
	dose, err := Read(testDoseFile(t, options))
	if err != nil {
		t.Fatal(err)
	}
	if len(dose.References) != 1 || dose.References[0].SOPInstanceUID != "1.2.plan" {
		t.Fatalf("references = %#v", dose.References)
	}
	if len(dose.EmbeddedDVHs) != 1 || dose.EmbeddedDVHs[0].NumberOfBins != 2 ||
		len(dose.EmbeddedDVHs[0].ReferencedROIs) != 1 || dose.EmbeddedDVHs[0].ReferencedROIs[0] != 7 {
		t.Fatalf("embedded DVHs = %#v", dose.EmbeddedDVHs)
	}
}

func TestComputeDVHHandlesAnisotropyHolesPartialOverlapAndBounds(t *testing.T) {
	values := make([]uint16, 5*5*2)
	for index := range values {
		values[index] = uint16(index + 1)
	}
	dose, err := Read(testDoseFile(t, testDoseOptions{
		rows: 5, columns: 5, frames: 2,
		spacing: []float64{2, 3}, position: []float64{0, 0, 0},
		orientation: []float64{1, 0, 0, 0, 1, 0},
		offsets:     []float64{0, 4}, scaling: 0.1, values: values,
		units: DoseUnitsGray, doseType: DoseTypePhysical, summation: "PLAN",
	}))
	if err != nil {
		t.Fatal(err)
	}
	set := &rtstruct.StructureSet{
		FrameOfReferenceUID: "1.2.frame",
		ROIs: []rtstruct.ROI{{
			Number: 7, Name: "PTV",
			Contours: []rtstruct.Contour{
				testContour(rtstruct.ContourClosedPlanarXOR, [][3]float64{
					{-2, -2, 0}, {14, -2, 0}, {14, 10, 0}, {-2, 10, 0},
				}),
				testContour(rtstruct.ContourClosedPlanarXOR, [][3]float64{
					{4.5, 3, 0}, {7.5, 3, 0}, {7.5, 5, 0}, {4.5, 5, 0},
				}),
			},
		}},
	}
	got, err := ComputeDVH(context.Background(), dose, set, 7, DVHOptions{Bins: 32})
	if err != nil {
		t.Fatalf("ComputeDVH() error = %v", err)
	}
	if got.VoxelCount <= 0 || got.VoxelCount >= 25 || got.VolumeCC <= 0 {
		t.Fatalf("DVH voxel/volume = %d/%v, expected clipped outer contour with XOR hole", got.VoxelCount, got.VolumeCC)
	}
	if !got.PartialOverlap {
		t.Fatal("DVH did not report partial grid overlap")
	}
	if got.Minimum <= 0 || got.Maximum < got.Minimum || got.Mean < got.Minimum || got.Mean > got.Maximum ||
		got.D95 > got.D50 || got.D50 > got.D2 {
		t.Fatalf("invalid DVH metrics: %+v", got)
	}
	again, err := ComputeDVH(context.Background(), dose, set, 7, DVHOptions{Bins: 32})
	if err != nil || !equalDVH(got, again) {
		t.Fatalf("DVH is not deterministic: err=%v\nfirst=%+v\nsecond=%+v", err, got, again)
	}
	if _, err := ComputeDVH(context.Background(), dose, set, 7, DVHOptions{MaxVoxels: 10}); !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("memory-bound error = %v, want ErrMemoryLimit", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ComputeDVH(ctx, dose, set, 7, DVHOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v, want context.Canceled", err)
	}
	set.FrameOfReferenceUID = "other"
	if _, err := ComputeDVH(context.Background(), dose, set, 7, DVHOptions{}); !errors.Is(err, ErrGeometryMismatch) {
		t.Fatalf("frame mismatch error = %v, want ErrGeometryMismatch", err)
	}
}

type testDoseOptions struct {
	rows, columns, frames                   int
	spacing, position, orientation, offsets []float64
	scaling                                 float64
	values                                  []uint16
	units, doseType, summation              string
	extra                                   []core.Element
}

func testDoseFile(t *testing.T, opts testDoseOptions) *object.File {
	t.Helper()
	pixels := make([]byte, len(opts.values)*2)
	for index, value := range opts.values {
		binary.LittleEndian.PutUint16(pixels[index*2:], value)
	}
	elements := []core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, RTDoseStorage),
		derivedio.UI(derivedio.TagSOPInstanceUID, "1.2.dose"),
		derivedio.UI(derivedio.TagStudyInstanceUID, "1.2.study"),
		derivedio.UI(derivedio.TagSeriesInstanceUID, "1.2.series"),
		derivedio.UI(derivedio.TagFrameOfReferenceUID, "1.2.frame"),
		derivedio.CS(derivedio.TagModality, "RTDOSE"),
		derivedio.CS(tagDoseUnits, opts.units),
		derivedio.CS(tagDoseType, opts.doseType),
		derivedio.CS(tagDoseSummationType, opts.summation),
		derivedio.DS(tagDoseGridScaling, opts.scaling),
		derivedio.DS(tagImagePositionPatient, opts.position...),
		derivedio.DS(tagImageOrientationPatient, opts.orientation...),
		derivedio.DS(tagPixelSpacing, opts.spacing...),
		derivedio.DS(tagSliceThickness, 1),
		derivedio.DS(tagGridFrameOffsetVector, opts.offsets...),
		derivedio.US(derivedio.TagRows, uint16(opts.rows)),
		derivedio.US(derivedio.TagColumns, uint16(opts.columns)),
		derivedio.US(derivedio.TagSamplesPerPixel, 1),
		derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME2"),
		derivedio.IS(derivedio.TagNumberOfFrames, opts.frames),
		derivedio.US(derivedio.TagBitsAllocated, 16),
		derivedio.US(derivedio.TagBitsStored, 16),
		derivedio.US(derivedio.TagHighBit, 15),
		derivedio.US(derivedio.TagPixelRepresentation, 0),
		derivedio.Raw(derivedio.TagPixelData, core.VROW, pixels),
	}
	elements = append(elements, opts.extra...)
	file, err := derivedio.File(RTDoseStorage, "1.2.dose", derivedio.Object(elements...))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func testContour(kind string, points [][3]float64) rtstruct.Contour {
	out := rtstruct.Contour{GeometricType: kind}
	for _, point := range points {
		out.Points = append(out.Points, rtstruct.Point3D{X: point[0], Y: point[1], Z: point[2]})
	}
	return out
}

func testVec(x, y, z float64) render.Vec3 {
	return render.Vec3{X: x, Y: y, Z: z}
}

func equalDVH(a, b DVHResult) bool {
	if a.ROINumber != b.ROINumber || a.VoxelCount != b.VoxelCount || a.VolumeCC != b.VolumeCC ||
		a.Minimum != b.Minimum || a.Maximum != b.Maximum || a.Mean != b.Mean ||
		a.D95 != b.D95 || a.D50 != b.D50 || a.D2 != b.D2 || len(a.Bins) != len(b.Bins) {
		return false
	}
	for index := range a.Bins {
		if a.Bins[index] != b.Bins[index] {
			return false
		}
	}
	return true
}
