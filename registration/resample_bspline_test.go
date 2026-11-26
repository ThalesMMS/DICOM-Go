package registration

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func smoothPatientField(p render.Vec3) float64 {
	return 120*math.Sin(0.3*p.X+0.1) + 70*math.Cos(0.25*p.Y)*math.Sin(0.2*p.Z+0.4) + 2*p.Z
}

// TestResampleCubicBSplineReproducesTheMovingSamples: on the moving image's
// own grid the B-spline returns its samples, as trilinear does.
func TestResampleCubicBSplineReproducesTheMovingSamples(t *testing.T) {
	grid := Grid{
		Size: [3]int{21, 18, 13}, Origin: render.Vec3{X: -4, Y: 3, Z: -9},
		Spacing: render.Vec3{X: 1.2, Y: 0.9, Z: 2.5}, Direction: obliqueDirection(0.2),
	}
	moving := sampledImage(t, grid, func(p render.Vec3) float64 {
		return smoothPatientField(p) + math.Mod(math.Abs(p.X*7+p.Y*3), 5)
	})
	identity := func(p render.Vec3) (render.Vec3, bool) { return p, true }
	out, err := Resample(context.Background(), moving, grid, identity, ResampleOptions{Interpolator: InterpolateCubicBSpline})
	if err != nil {
		t.Fatal(err)
	}
	if out.Outside != 0 {
		t.Fatalf("%d samples outside the moving grid", out.Outside)
	}
	for i, want := range moving.Data() {
		if got := out.Image.Data()[i]; math.Abs(float64(got-want)) > 1e-3*(1+math.Abs(float64(want))) {
			t.Fatalf("sample %d = %v, want %v", i, got, want)
		}
	}
	for z, sources := range out.SourceSlices {
		if sources.First != max(z-1, 0) || sources.Last != min(z+1, grid.Size[2]-1) {
			t.Fatalf("slice %d sources %+v", z, sources)
		}
	}
}

// TestResampleCubicBSplineBeatsTrilinear resamples a smooth field onto a
// grid shifted between the moving samples (and through a small rotation):
// against the analytic field the B-spline error is a fraction of
// trilinear's.
func TestResampleCubicBSplineBeatsTrilinear(t *testing.T) {
	moving := sampledImage(t, Grid{
		Size: [3]int{40, 36, 28}, Origin: render.Vec3{X: -10, Y: -8, Z: -14},
		Spacing: render.Vec3{X: 1.5, Y: 1.4, Z: 2}, Direction: Identity3(),
	}, smoothPatientField)
	fixed := Grid{
		Size: [3]int{30, 26, 18}, Origin: render.Vec3{X: -2.3, Y: -0.9, Z: -6.7},
		Spacing: render.Vec3{X: 1.3, Y: 1.1, Z: 1.7}, Direction: obliqueDirection(0.1),
	}
	truth := rigidTruth(t, [3]float64{1, -2, 3}, render.Vec3{X: 1.1, Y: -0.7, Z: 0.4}, render.Vec3{X: 10, Y: 10, Z: 0})
	mapping, err := TransformMapping(truth)
	if err != nil {
		t.Fatal(err)
	}
	rms := func(interpolator Interpolator) float64 {
		out, err := Resample(context.Background(), moving, fixed, mapping, ResampleOptions{Interpolator: interpolator, Padding: -1})
		if err != nil {
			t.Fatal(err)
		}
		var squared float64
		count := 0
		for z := 0; z < fixed.Size[2]; z++ {
			for y := 0; y < fixed.Size[1]; y++ {
				for x := 0; x < fixed.Size[0]; x++ {
					i := (z*fixed.Size[1]+y)*fixed.Size[0] + x
					if !out.Inside[i] {
						continue
					}
					q := truth.TransformPoint(fixed.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}))
					index := moving.ContinuousIndex(q)
					// Interior points only: both kernels clamp at the border.
					if index.X < 2 || index.Y < 2 || index.Z < 2 || index.X > 37 || index.Y > 33 || index.Z > 25 {
						continue
					}
					squared += math.Pow(float64(out.Image.Data()[i])-smoothPatientField(q), 2)
					count++
				}
			}
		}
		if count < 1000 {
			t.Fatalf("only %d interior samples", count)
		}
		return math.Sqrt(squared / float64(count))
	}
	linear, cubic := rms(InterpolateLinear), rms(InterpolateCubicBSpline)
	if !(cubic < linear/4) {
		t.Fatalf("RMS error: B-spline %.4f, trilinear %.4f; want the B-spline below a quarter", cubic, linear)
	}
	t.Logf("RMS error: B-spline %.4f, trilinear %.4f", cubic, linear)
}

func TestResampleCubicBSplineRejectsNonFiniteMovingSamples(t *testing.T) {
	grid := Grid{Size: [3]int{6, 6, 6}, Spacing: render.Vec3{X: 1, Y: 1, Z: 1}, Direction: Identity3()}
	moving := sampledImage(t, grid, func(p render.Vec3) float64 {
		if p.X == 3 && p.Y == 3 && p.Z == 3 {
			return math.NaN()
		}
		return 1
	})
	identity := func(p render.Vec3) (render.Vec3, bool) { return p, true }
	if _, err := Resample(context.Background(), moving, grid, identity, ResampleOptions{Interpolator: InterpolateCubicBSpline}); !errors.Is(err, ErrResample) {
		t.Fatalf("NaN moving sample: err = %v", err)
	}
	// A mapping that never reaches the moving image needs no coefficients.
	away := func(p render.Vec3) (render.Vec3, bool) { return p.Add(render.Vec3{X: 1000}), true }
	out, err := Resample(context.Background(), moving, grid, away, ResampleOptions{Interpolator: InterpolateCubicBSpline, Padding: 7})
	if err != nil || out.Outside != 6*6*6 || out.Image.Data()[0] != 7 {
		t.Fatalf("outside mapping: err %v, outside %d", err, out.Outside)
	}
}
