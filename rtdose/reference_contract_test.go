package rtdose

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/render"
)

// DICOM PS3.3 C.8.8.3.2 defines relative dose planes along the row/column
// cross product and the axial-only absolute coordinate form.
func TestReferenceRTDoseNonuniformPlanes(t *testing.T) {
	oblique := []float64{0.6, 0, 0.8, 0, 1, 0}
	axial := []float64{1, 0, 0, 0, 1, 0}
	for _, tc := range []struct {
		name        string
		orientation []float64
		offsets     []float64
		stored      []uint16
		origins     []render.Vec3
	}{
		{
			name: "oblique ascending relative", orientation: oblique,
			offsets: []float64{0, 5, 15}, stored: []uint16{12, 32, 72},
			origins: []render.Vec3{testVec(11, 12, 13), testVec(7, 12, 16), testVec(-1, 12, 22)},
		},
		{
			name: "oblique descending relative", orientation: oblique,
			offsets: []float64{0, -10, -15}, stored: []uint16{72, 32, 12},
			origins: []render.Vec3{testVec(23, 12, 4), testVec(19, 12, 7), testVec(11, 12, 13)},
		},
		{
			name: "axial descending absolute", orientation: axial,
			offsets: []float64{13, 3, -2}, stored: []uint16{72, 32, 12},
			origins: []render.Vec3{testVec(11, 12, -2), testVec(11, 12, 3), testVec(11, 12, 13)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dose, err := Read(testDoseFile(t, testDoseOptions{
				rows: 1, columns: 1, frames: 3, spacing: []float64{2, 3},
				position: []float64{11, 12, 13}, orientation: tc.orientation,
				offsets: tc.offsets, scaling: 0.25, values: tc.stored,
				units: DoseUnitsGray, doseType: DoseTypePhysical, summation: "PLAN",
			}))
			if err != nil {
				t.Fatal(err)
			}
			if len(dose.Geometry.Slices) != len(tc.origins) {
				t.Fatalf("slice count = %d, want %d", len(dose.Geometry.Slices), len(tc.origins))
			}
			for i, origin := range tc.origins {
				if got := dose.Geometry.Slices[i].Origin; got.Sub(origin).Length() > 1e-9 {
					t.Errorf("slice %d origin = %v, want %v", i, got, origin)
				}
			}
			if !slices.Equal(dose.Values, []float64{3, 8, 18}) || dose.Maximum != 18 {
				t.Fatalf("scaled samples = %v, maximum = %g", dose.Values, dose.Maximum)
			}
			for _, query := range []struct {
				lower, upper int
				fraction     float64
				want         float64
			}{{0, 1, 0.2, 4}, {1, 2, 0.7, 15}} {
				origin := tc.origins[query.lower]
				point := origin.Add(tc.origins[query.upper].Sub(origin).Scale(query.fraction))
				if got, ok := dose.SamplePatient(point); !ok || math.Abs(got-query.want) > 1e-9 {
					t.Errorf("SamplePatient(%v) = %g, %v; want %g, true", point, got, ok, query.want)
				}
			}
		})
	}
}

func TestReferenceRTDoseInvalidOffsets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offsets []float64
	}{
		{"missing frames", nil}, {"too few offsets", []float64{0, 1}},
		{"too many offsets", []float64{0, 1, 2, 3}},
		{"repeated planes", []float64{0, 2, 2}},
		{"changes direction", []float64{0, 2, 1}},
		{"absolute origin mismatch", []float64{1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dose, err := Read(testDoseFile(t, testDoseOptions{
				rows: 1, columns: 1, frames: 3, spacing: []float64{2, 3},
				position: []float64{5, 6, 7}, orientation: []float64{1, 0, 0, 0, 1, 0},
				offsets: tc.offsets, scaling: 1, values: []uint16{3, 4, 5},
				units: DoseUnitsGray, doseType: DoseTypePhysical, summation: "PLAN",
			}))
			if dose != nil || !errors.Is(err, ErrGeometryMismatch) {
				t.Fatalf("Read() = %v, %v; want nil and ErrGeometryMismatch", dose, err)
			}
		})
	}
}

func TestReferenceRTDoseKeepsCompleteDVHSiblings(t *testing.T) {
	dvh := func(kind string, roi, bins int, scaling float64, data ...float64) core.DataSet {
		return derivedio.DataSet(
			derivedio.CS(core.NewTag(0x3004, 0x0001), kind),
			derivedio.DS(tagDVHDoseScaling, scaling),
			derivedio.CS(tagDVHVolumeUnits, "CM3"),
			derivedio.IS(tagDVHNumberOfBins, bins),
			derivedio.DS(tagDVHData, data...),
			derivedio.Seq(tagDVHReferencedROISequence, derivedio.DataSet(
				derivedio.IS(tagReferencedROINumber, roi),
				derivedio.CS(tagDVHROIContributionType, "INCLUDED"),
			)),
		)
	}
	file := testDoseFile(t, testDoseOptions{
		rows: 1, columns: 1, frames: 1, spacing: []float64{1, 1},
		position: []float64{0, 0, 0}, orientation: []float64{1, 0, 0, 0, 1, 0},
		scaling: 1, values: []uint16{11}, units: DoseUnitsGray, doseType: DoseTypePhysical, summation: "PLAN",
		extra: []core.Element{derivedio.Seq(tagDVHSequence,
			dvh("CUMULATIVE", 17, 1, 0.5, 2, 9),
			dvh("CUMULATIVE", 18, 2, 0.5, 2, 9, 3),
			dvh("DIFFERENTIAL", 19, 1, 0, 2, 9),
			dvh("DIFFERENTIAL", 20, 2, 0.25, 2, 5, 3, 2),
		)},
	})
	dose, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(dose.EmbeddedDVHs) != 2 {
		t.Fatalf("embedded DVHs = %+v, want two complete items", dose.EmbeddedDVHs)
	}
	for i, want := range []EmbeddedDVH{
		{Type: "CUMULATIVE", DoseUnits: "GY", DoseScaling: 0.5, VolumeUnits: "CM3", NumberOfBins: 1, Data: []float64{2, 9}, ReferencedROIs: []int{17}, ContributionType: "INCLUDED"},
		{Type: "DIFFERENTIAL", DoseUnits: "GY", DoseScaling: 0.25, VolumeUnits: "CM3", NumberOfBins: 2, Data: []float64{2, 5, 3, 2}, ReferencedROIs: []int{20}, ContributionType: "INCLUDED"},
	} {
		got := dose.EmbeddedDVHs[i]
		if got.Type != want.Type || got.DoseUnits != want.DoseUnits || got.DoseScaling != want.DoseScaling ||
			got.VolumeUnits != want.VolumeUnits || got.NumberOfBins != want.NumberOfBins || got.ContributionType != want.ContributionType ||
			!slices.Equal(got.Data, want.Data) || !slices.Equal(got.ReferencedROIs, want.ReferencedROIs) {
			t.Errorf("embedded DVH %d = %+v, want %+v", i, got, want)
		}
	}
}
