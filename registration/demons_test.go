package registration

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

func TestDisplacementAtInterpolatesWithoutAllocatingAnImage(t *testing.T) {
	im, err := NewImage([3]int{3, 2, 2}, make([]float32, 12), render.Vec3{X: 10, Y: -20, Z: 30},
		render.Vec3{X: 2, Y: 3, Z: 4}, Matrix3{{0, -1, 0}, {1, 0, 0}, {0, 0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	field := newField(im)
	for z := range 2 {
		for y := range 2 {
			for x := range 3 {
				field.Vectors[(z*2+y)*3+x] = render.Vec3{X: float64(x), Y: 2 * float64(y), Z: 3 * float64(z)}
			}
		}
	}
	for _, tc := range []struct{ index, want render.Vec3 }{
		{render.Vec3{X: 0.5, Y: 0.5, Z: 0.5}, render.Vec3{X: 0.5, Y: 1, Z: 1.5}},
		{render.Vec3{X: -1, Y: 3, Z: 0.25}, render.Vec3{Y: 2, Z: 0.75}},
	} {
		p := im.Point(tc.index)
		if got := field.DisplacementAt(p); got.Sub(tc.want).Length() > 1e-12 {
			t.Fatalf("displacement at %v = %v, want %v", p, got, tc.want)
		}
		if allocations := testing.AllocsPerRun(20, func() { field.DisplacementAt(p) }); allocations != 0 {
			t.Errorf("displacement lookup allocated %g times", allocations)
		}
	}
}

// circleImage is a 128 × 128 image of unit spacing holding a disc of 250 on
// a background of 15, as itkDemonsRegistrationFilterTest fills its images.
func circleImage(t *testing.T, cx, cy, radius float64) *Image {
	t.Helper()
	const n = 128
	data := make([]float32, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			value := float32(15)
			if (float64(x)-cx)*(float64(x)-cx)+(float64(y)-cy)*(float64(y)-cy) <= radius*radius {
				value = 250
			}
			data[y*n+x] = value
		}
	}
	im, err := NewImage([3]int{n, n, 1}, data, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	return im
}

// differingPixels warps moving through the field with nearest-neighbour
// interpolation (the background outside) and counts the pixels that differ
// from fixed, as the ITK test does.
func differingPixels(fixed, moving *Image, field *DisplacementField) int {
	size := fixed.Size()
	count := 0
	for y := 0; y < size[1]; y++ {
		for x := 0; x < size[0]; x++ {
			p := fixed.Point(render.Vec3{X: float64(x), Y: float64(y)}).Add(field.At(x, y, 0))
			index := moving.ContinuousIndex(p)
			mx, my := int(math.Round(index.X)), int(math.Round(index.Y))
			value := float32(15)
			if mx >= 0 && my >= 0 && mx < size[0] && my < size[1] {
				value = moving.At(mx, my, 0)
			}
			if value != fixed.At(x, y, 0) {
				count++
			}
		}
	}
	return count
}

// TestDemonsRegistersITKsDeformedCircle is itkDemonsRegistrationFilterTest:
// a disc of radius 30 at (64, 64) registered onto one of radius 32 at
// (62, 64) with the field smoothed by σ = 1 and an intensity difference
// threshold of 0.001 (Gaussian maximum error 0.08, kernel width 10), stopped
// at 150 iterations; the moving image warped by
// the field must differ from the fixed one in at most 10 pixels. The
// diffeomorphic demons meet the same bound with a field whose Jacobian
// determinant stays positive.
func TestDemonsRegistersITKsDeformedCircle(t *testing.T) {
	fixed := circleImage(t, 62, 64, 32)
	moving := circleImage(t, 64, 64, 30)
	before := differingPixels(fixed, moving, newField(fixed))
	for name, opts := range map[string]DemonsOptions{
		"demons":                        {Iterations: 150, FieldSigma: 1, IntensityDifferenceThreshold: 0.001},
		"diffeomorphic":                 {Iterations: 150, FieldSigma: 1, IntensityDifferenceThreshold: 0.001, Diffeomorphic: true},
		"multiresolution diffeomorphic": {Shrinks: []int{4, 2, 1}, Iterations: 60, FieldSigma: 1, IntensityDifferenceThreshold: 0.001, Diffeomorphic: true},
	} {
		result, err := Demons(context.Background(), fixed, moving, opts)
		if err != nil {
			t.Fatal(err)
		}
		after := differingPixels(fixed, moving, result.Field)
		last := result.Levels[len(result.Levels)-1]
		t.Logf("%s: %d pixels differ before, %d after; mean squares %.2f, RMS change %.4f", name, before, after, last.MeanSquares, last.RMSChange)
		if after > 10 {
			t.Fatalf("%s: %d pixels differ", name, after)
		}
		// As a Deformable Spatial Registration grid, the field maps points
		// as it does, and the diffeomorphic field does not fold.
		grid, err := result.Field.Grid()
		if err != nil {
			t.Fatal(err)
		}
		identity := spatialreg.Matrix{Values: render.GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}}
		deformable := spatialreg.Registration{Pre: identity, Post: identity, Grid: grid}
		probe := fixed.Point(render.Vec3{X: 93, Y: 64})
		mapped, err := deformable.MapRegisteredToSource(probe)
		if err != nil || mapped.Sub(probe.Add(result.Field.At(93, 64, 0))).Length() > 1e-4 {
			t.Fatalf("%s: grid maps %v to %v: %v", name, probe, mapped, err)
		}
		if opts.Diffeomorphic {
			if folded := foldedSamples(result.Field); folded != 0 {
				t.Fatalf("%s: %d samples fold", name, folded)
			}
			change, err := deformable.VolumeChange()
			if err != nil || change.Folded != 0 {
				t.Fatalf("%s: volume change %v..%v, %d folded: %v", name, change.Min, change.Max, change.Folded, err)
			}
		}
	}
	if _, err := Demons(context.Background(), fixed, nil, DemonsOptions{}); !errors.Is(err, ErrDemons) {
		t.Fatalf("missing image: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := Demons(ctx, fixed, moving, DemonsOptions{Progress: func(DemonsProgress) {
		if calls++; calls == 3 {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) || calls != 3 {
		t.Fatalf("cancelled after %d iterations: %v", calls, err)
	}
}

// foldedSamples counts the 2D samples where det(I + ∇u) ≤ 0.
func foldedSamples(field *DisplacementField) int {
	folded := 0
	for y := 0; y < field.Size[1]; y++ {
		for x := 0; x < field.Size[0]; x++ {
			x0, x1 := max(0, x-1), min(field.Size[0]-1, x+1)
			y0, y1 := max(0, y-1), min(field.Size[1]-1, y+1)
			dx := field.At(x1, y, 0).Sub(field.At(x0, y, 0)).Scale(1 / float64(x1-x0))
			dy := field.At(x, y1, 0).Sub(field.At(x, y0, 0)).Scale(1 / float64(y1-y0))
			if (1+dx.X)*(1+dy.Y)-dy.X*dx.Y <= 0 {
				folded++
			}
		}
	}
	return folded
}
