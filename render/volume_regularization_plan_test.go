package render

import (
	"math"
	"testing"
)

func regularizationPlanGeometry() VolumeGeometry {
	slices := make([]SliceGeometry, 4)
	for i, z := range []float64{0, 1, 3, 4} {
		slices[i] = SliceGeometry{Origin: Vec3{X: .25 * z, Z: z}, RowDir: Vec3{X: 1}, ColDir: Vec3{Y: 1}, Normal: Vec3{Z: 1}, RowSpacing: 2, ColSpacing: .5, Rows: 5, Columns: 6}
	}
	return BuildVolumeGeometry(slices, DefaultGeometryTolerances())
}

func TestVolumeRegularizationPlanMetadataMissingSlicesTiltAndOwnership(t *testing.T) {
	geometry := regularizationPlanGeometry()
	plan, err := PlanVolumeRegularization(geometry)
	if err != nil {
		t.Fatal(err, geometry)
	}
	if plan.SourceDimensions != [3]uint32{6, 5, 4} || plan.Target.Dimensions != [3]int{8, 5, 5} {
		t.Fatal("planned bounds", plan)
	}
	for z, want := range []float64{0, 1, 1.5, 2, 3} {
		point := plan.SliceOrigins[z]
		if math.Abs(point.X+.5*float64(z)) > 1e-12 || math.Abs(point.Y) > 1e-12 || math.Abs(point.Z-want) > 1e-12 {
			t.Fatal("metadata index mapping", z, point)
		}
	}
	if plan.ColumnStep != (Vec3{X: 1}) || plan.RowStep != (Vec3{Y: 1}) {
		t.Fatal("metadata steps", plan)
	}
	frozen, err := plan.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	plan.SliceOrigins[1].Z = 999
	if frozen.SliceOrigins[1].Z != 1 {
		t.Fatal("plan aliases authoring table")
	}
	d, err := frozen.Descriptor(2, 1)
	if err != nil || d.ParentGeneration != 1 || d.VolumeGeneration != 2 || d.Derivation != VolumeDerivationRegularized || d.SampleDomain != VolumeSampleDomainModality || d.ByteLength != 8*5*5*4 {
		t.Fatal("derived metadata", d, err)
	}
	for _, mutate := range []func(*VolumeRegularizationPlan){func(p *VolumeRegularizationPlan) { p.SliceOrigins = p.SliceOrigins[:1] }, func(p *VolumeRegularizationPlan) { p.SourceDimensions[0] = 0 }, func(p *VolumeRegularizationPlan) { p.RowStep = Vec3{} }, func(p *VolumeRegularizationPlan) { p.SliceOrigins[0].X = math.NaN() }} {
		bad, _ := frozen.Freeze()
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("malformed plan accepted")
		}
	}
	geometry.Slices[1].Origin = geometry.Slices[0].Origin
	if _, err := PlanVolumeRegularization(geometry); err == nil {
		t.Fatal("duplicate acquisition positions accepted")
	}
}
