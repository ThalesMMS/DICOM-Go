package render_test

import (
	"bytes"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
)

func sweepClose(t *testing.T, got, want render.Vec3) {
	t.Helper()
	if got.Sub(want).Length() > 1e-9 {
		t.Fatalf("geometry %v want %v", got, want)
	}
}

func TestPlaneSweepParallelPhysicalDirectionEndpointsAndImmutability(t *testing.T) {
	o := render.PlaneSweepOptions{Kind: render.PlaneSweepParallel, Base: render.Plane{Origin: render.Vec3{X: 10, Y: 20, Z: 30}, U: render.Vec3{X: 4, Y: 4}, V: render.Vec3{Z: 6}}, Start: 6, End: -4, Step: 3, SliceThicknessMM: 2.5}
	p, e := render.NewPlaneSweep(o)
	if e != nil {
		t.Fatal(e)
	}
	positions := []float64{6, 3, 0, -3, -4}
	if p.Count() != len(positions) {
		t.Fatal("count", p.Count())
	}
	n := render.Vec3{X: math.Sqrt(.5), Y: -math.Sqrt(.5)}
	for i, want := range positions {
		position, ok := p.Position(i)
		if !ok || position != want {
			t.Fatal("position", position, want)
		}
		plane, ok := p.Plane(i)
		if !ok {
			t.Fatal("plane", i)
		}
		sweepClose(t, plane.Origin, o.Base.Origin.Add(n.Scale(want)))
		sweepClose(t, plane.U, o.Base.U)
		sweepClose(t, plane.V, o.Base.V)
	}
	copy := p.Options()
	copy.Base.Origin = render.Vec3{}
	copy.End = 999
	last, _ := p.Position(p.Count() - 1)
	if last != -4 {
		t.Fatal("options alias generator")
	}
	if _, ok := p.Plane(p.Count()); ok {
		t.Fatal("out of bounds plane")
	}
	if _, ok := (render.PlaneSweep{}).Plane(0); ok {
		t.Fatal("zero plan accepted")
	}
}

func TestPlaneSweepRotationAxisPivotAndDICOMGeometryRoundTrip(t *testing.T) {
	o := render.PlaneSweepOptions{Kind: render.PlaneSweepRotational, Base: render.Plane{Origin: render.Vec3{X: 10, Y: 20, Z: 30}, U: render.Vec3{X: 6}, V: render.Vec3{Y: 4}}, Axis: render.Vec3{Z: 7}, Pivot: render.Vec3{X: 13, Y: 22, Z: 30}, Start: 0, End: 90, Step: 45, SliceThicknessMM: 1.25}
	p, e := render.NewPlaneSweep(o)
	if e != nil {
		t.Fatal(e)
	}
	last, _ := p.Plane(2)
	sweepClose(t, last.Origin, render.Vec3{X: 15, Y: 19, Z: 30})
	sweepClose(t, last.U, render.Vec3{Y: 6})
	sweepClose(t, last.V, render.Vec3{X: -4})
	for i := 0; i < p.Count(); i++ {
		plane, _ := p.Plane(i)
		u, v := plane.U.Normalize(), plane.V.Normalize()
		spacing := plane.PixelSpacingMM(4, 3)
		files, e := imagebuild.Build(imagebuild.Geometry{Rows: 3, Columns: 4, PixelSpacing: [2]float64{spacing.Y, spacing.X}, RowDirection: [3]float64{u.X, u.Y, u.Z}, ColumnDirection: [3]float64{v.X, v.Y, v.Z}, SliceThickness: o.SliceThicknessMM, FrameOfReferenceUID: "2.25.1517.4"}, []imagebuild.Slice{{Values: make([]float64, 12), Position: [3]float64{plane.Origin.X, plane.Origin.Y, plane.Origin.Z}}}, imagebuild.Options{IOD: imagebuild.CT})
		if e != nil {
			t.Fatal(e)
		}
		var wire bytes.Buffer
		if e = object.WriteFile(&wire, files[0]); e != nil {
			t.Fatal(e)
		}
		parsed, e := object.ReadFile(bytes.NewReader(wire.Bytes()))
		if e != nil {
			t.Fatal(e)
		}
		position, _ := parsed.GetFloats(core.NewTag(0x0020, 0x0032))
		orientation, _ := parsed.GetFloats(core.NewTag(0x0020, 0x0037))
		pixels, _ := parsed.GetFloats(core.NewTag(0x0028, 0x0030))
		thickness, _ := parsed.GetFloats(core.NewTag(0x0018, 0x0050))
		if len(position) != 3 || len(orientation) != 6 || len(pixels) != 2 || len(thickness) != 1 {
			t.Fatal("missing Image Plane geometry")
		}
		want := []float64{plane.Origin.X, plane.Origin.Y, plane.Origin.Z, u.X, u.Y, u.Z, v.X, v.Y, v.Z, 2, 2, 1.25}
		got := append(append(append(position, orientation...), pixels...), thickness...)
		for k := range want {
			if math.Abs(got[k]-want[k]) > 1e-8 {
				t.Fatalf("plane %d field %d: %g want %g", i, k, got[k], want[k])
			}
		}
		parsed.Close()
		files[0].Close()
	}
}

func TestPlaneSweepCrossPanelsClipRealObliqueExtremes(t *testing.T) {
	reference := render.Plane{U: render.Vec3{X: 10}, V: render.Vec3{Y: 10}}
	target := render.Plane{Origin: render.Vec3{X: 2}, U: render.Vec3{X: 4, Y: 4}, V: render.Vec3{Z: 5}}
	line, visible, coplanar := render.PlaneCrossSection(reference, target)
	if !visible || coplanar {
		t.Fatal("oblique line missing")
	}
	for _, point := range line {
		if math.Abs(point[0]-point[1]-.2) > 1e-9 {
			t.Fatal("projected wrong physical plane", line)
		}
	}
	_, visible, coplanar = render.PlaneCrossSection(reference, reference)
	if visible || !coplanar {
		t.Fatal("coplanar policy")
	}
	target = reference
	target.Origin.Z = 1
	_, visible, coplanar = render.PlaneCrossSection(reference, target)
	if visible || coplanar {
		t.Fatal("parallel disjoint policy")
	}
	target = render.Plane{Origin: render.Vec3{X: 11}, U: render.Vec3{Y: 10}, V: render.Vec3{Z: 10}}
	_, visible, _ = render.PlaneCrossSection(reference, target)
	if visible {
		t.Fatal("out of panel line")
	}
}

func TestPlaneSweepRejectsInvalidAndExcessiveBeforeAllocation(t *testing.T) {
	base := render.PlaneSweepOptions{Kind: render.PlaneSweepParallel, Base: render.Plane{U: render.Vec3{X: 10}, V: render.Vec3{Y: 10}}, Start: 0, End: 10, Step: 1, SliceThicknessMM: 1}
	for _, mutate := range []func(*render.PlaneSweepOptions){
		func(o *render.PlaneSweepOptions) { o.Step = 0 }, func(o *render.PlaneSweepOptions) { o.Step = math.SmallestNonzeroFloat64 }, func(o *render.PlaneSweepOptions) { o.End = math.NaN() }, func(o *render.PlaneSweepOptions) { o.Base.V = o.Base.U }, func(o *render.PlaneSweepOptions) { o.SliceThicknessMM = math.Inf(1) }, func(o *render.PlaneSweepOptions) { o.MaxPlanes = 10 }, func(o *render.PlaneSweepOptions) { o.Kind = render.PlaneSweepRotational; o.Axis = render.Vec3{} },
	} {
		o := base
		mutate(&o)
		p, e := render.NewPlaneSweep(o)
		if e == nil || p.Count() != 0 {
			t.Fatal("invalid plan admitted", o, p, e)
		}
	}
}
