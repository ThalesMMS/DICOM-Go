package registration

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// sampledImage samples field at the points of a grid.
func sampledImage(t *testing.T, grid Grid, field func(render.Vec3) float64) *Image {
	t.Helper()
	data := make([]float32, grid.Size[0]*grid.Size[1]*grid.Size[2])
	for z := 0; z < grid.Size[2]; z++ {
		for y := 0; y < grid.Size[1]; y++ {
			for x := 0; x < grid.Size[0]; x++ {
				p := grid.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
				data[(z*grid.Size[1]+y)*grid.Size[0]+x] = float32(field(p))
			}
		}
	}
	im, err := NewImage(grid.Size, data, grid.Origin, grid.Spacing, grid.Direction)
	if err != nil {
		t.Fatal(err)
	}
	return im
}

// obliqueDirection rotates the index axes by angle about z.
func obliqueDirection(angle float64) Matrix3 {
	c, s := math.Cos(angle), math.Sin(angle)
	return Matrix3{{c, -s, 0}, {s, c, 0}, {0, 0, 1}}
}

func rigidTruth(t *testing.T, degrees [3]float64, translation render.Vec3, centre render.Vec3) *Euler3D {
	t.Helper()
	truth := NewEuler3D()
	if err := truth.SetCenter(centre); err != nil {
		t.Fatal(err)
	}
	r := math.Pi / 180
	if err := truth.SetParameters([]float64{degrees[0] * r, degrees[1] * r, degrees[2] * r, translation.X, translation.Y, translation.Z}); err != nil {
		t.Fatal(err)
	}
	return truth
}

// TestResampleMatchesAnalyticValuesThroughARigidTransform resamples a moving
// image holding a linear function of position (which trilinear
// interpolation reproduces exactly) onto an oblique, anisotropic fixed grid
// through a rotation and translation: every sample inside the moving field
// equals the function at the mapped point, every other sample is the
// padding.
func TestResampleMatchesAnalyticValuesThroughARigidTransform(t *testing.T) {
	linear := func(p render.Vec3) float64 { return 3*p.X - 2*p.Y + 0.5*p.Z - 40 }
	moving := sampledImage(t, Grid{
		Size: [3]int{40, 36, 30}, Origin: render.Vec3{X: -10, Y: -5, Z: -20},
		Spacing: render.Vec3{X: 1.5, Y: 1.5, Z: 2}, Direction: Identity3(),
	}, linear)
	fixed := Grid{
		Size: [3]int{32, 30, 24}, Origin: render.Vec3{X: 2, Y: -8, Z: -12},
		Spacing: render.Vec3{X: 1.7, Y: 1.4, Z: 2.5}, Direction: obliqueDirection(0.3),
	}
	truth := rigidTruth(t, [3]float64{4, -3, 10}, render.Vec3{X: 6, Y: -4, Z: 3}, render.Vec3{X: 20, Y: 20, Z: 10})
	mapping, err := TransformMapping(truth)
	if err != nil {
		t.Fatal(err)
	}
	var progressed int
	out, err := Resample(context.Background(), moving, fixed, mapping, ResampleOptions{Padding: -1024, Progress: func(done, total int) {
		if total != fixed.Size[2] || done < 1 || done > total {
			t.Errorf("progress %d of %d", done, total)
		}
		progressed++
	}})
	if err != nil {
		t.Fatal(err)
	}
	if progressed != fixed.Size[2] || !out.Image.Grid().SameAs(fixed, 1e-12) {
		t.Fatalf("%d progress calls, grid %+v", progressed, out.Image.Grid())
	}
	var inside, outside int
	for z := 0; z < fixed.Size[2]; z++ {
		for y := 0; y < fixed.Size[1]; y++ {
			for x := 0; x < fixed.Size[0]; x++ {
				i := (z*fixed.Size[1]+y)*fixed.Size[0] + x
				q := truth.TransformPoint(fixed.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}))
				index := moving.ContinuousIndex(q)
				want, in := float64(-1024), moving.inside(index)
				if in {
					// Within half a sample outside, the outer samples extend.
					clamped := moving.Point(render.Vec3{
						X: math.Min(math.Max(index.X, 0), 39), Y: math.Min(math.Max(index.Y, 0), 35), Z: math.Min(math.Max(index.Z, 0), 29),
					})
					want = linear(clamped)
					inside++
				} else {
					outside++
				}
				if out.Inside[i] != in || math.Abs(float64(out.Image.Data()[i])-want) > 2e-3 {
					t.Fatalf("sample (%d, %d, %d): %v (inside %v), want %v (inside %v)", x, y, z, out.Image.Data()[i], out.Inside[i], want, in)
				}
			}
		}
	}
	if inside == 0 || outside == 0 || out.Outside != outside {
		t.Fatalf("%d inside, %d outside, reported %d outside", inside, outside, out.Outside)
	}
}

// blob is a smooth object for registration-style tests.
func blob(p render.Vec3) float64 {
	g := func(x, y, z, width, height float64) float64 {
		dx, dy, dz := p.X-x, p.Y-y, p.Z-z
		return height * math.Exp(-(dx*dx+dy*dy+dz*dz)/width)
	}
	return -1000 + g(30, 32, 24, 120, 1100) + g(44, 26, 30, 60, 400)
}

// TestResampledThenSubtractedIdenticalVolumesAreNearlyZero builds a fixed
// volume and a moving volume showing the same object after a known rigid
// motion (moving(T(p)) = fixed(p)); resampling the moving volume through T
// onto the fixed grid and subtracting the fixed volume leaves a difference
// near zero, in the volumes' units (HU here), while subtracting without the
// registration does not.
func TestResampledThenSubtractedIdenticalVolumesAreNearlyZero(t *testing.T) {
	truth := rigidTruth(t, [3]float64{2, -3, 5}, render.Vec3{X: 4.5, Y: -3.25, Z: 2}, render.Vec3{X: 36, Y: 36, Z: 28})
	inverse, err := truth.Inverse()
	if err != nil {
		t.Fatal(err)
	}
	grid := Grid{Size: [3]int{40, 40, 32}, Spacing: render.Vec3{X: 1.8, Y: 1.8, Z: 1.8}, Direction: Identity3()}
	fixed := sampledImage(t, grid, blob)
	moving := sampledImage(t, Grid{Size: [3]int{44, 44, 34}, Origin: render.Vec3{X: -3, Y: -2, Z: -2}, Spacing: render.Vec3{X: 1.7, Y: 1.7, Z: 1.9}, Direction: Identity3()},
		func(q render.Vec3) float64 { return blob(inverse.TransformPoint(q)) })

	mapping, _ := TransformMapping(truth)
	aligned, err := Resample(context.Background(), moving, grid, mapping, ResampleOptions{Padding: -1000})
	if err != nil {
		t.Fatal(err)
	}
	difference, err := Subtract(aligned.Image, fixed, aligned.Inside, 0)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := TransformMapping(NewEuler3D())
	unaligned, err := Resample(context.Background(), moving, grid, identity, ResampleOptions{Padding: -1000})
	if err != nil {
		t.Fatal(err)
	}
	naive, err := Subtract(unaligned.Image, fixed, unaligned.Inside, 0)
	if err != nil {
		t.Fatal(err)
	}
	maxAbs := func(im *Image) float64 {
		m := 0.0
		for _, v := range im.Data() {
			m = math.Max(m, math.Abs(float64(v)))
		}
		return m
	}
	// Linear interpolation of the blob over 1.7–1.9 mm samples: a few
	// percent of its 1100 HU contrast at most.
	if got := maxAbs(difference); got > 30 {
		t.Fatalf("aligned difference up to %.1f HU", got)
	}
	if got := maxAbs(naive); got < 300 {
		t.Fatalf("unaligned difference only %.1f HU", got)
	}
	// Identical aligned volumes subtract to exactly zero, padded outside.
	self, err := Subtract(fixed, fixed, nil, 0)
	if err != nil || maxAbs(self) != 0 {
		t.Fatalf("self difference %v, %v", maxAbs(self), err)
	}
	valid := make([]bool, len(fixed.Data()))
	valid[0] = true
	padded, _ := Subtract(fixed, fixed, valid, -7)
	if padded.Data()[0] != 0 || padded.Data()[1] != -7 {
		t.Fatalf("padding %v %v", padded.Data()[0], padded.Data()[1])
	}
	other := sampledImage(t, Grid{Size: grid.Size, Origin: render.Vec3{X: 1}, Spacing: grid.Spacing, Direction: Identity3()}, blob)
	if _, err := Subtract(fixed, other, nil, 0); !errors.Is(err, ErrResample) {
		t.Fatalf("different grids: %v", err)
	}
}

// TestResampleLabelsKeepTheirValues resamples a label volume with nearest
// and Gaussian label interpolation: no new values appear, an integer
// translation is exact, and both agree with the labels away from edges.
func TestResampleLabelsKeepTheirValues(t *testing.T) {
	grid := Grid{Size: [3]int{24, 24, 20}, Spacing: render.Vec3{X: 2, Y: 2, Z: 2}, Direction: Identity3()}
	labelAt := func(p render.Vec3) float64 {
		switch d := p.Sub(render.Vec3{X: 24, Y: 24, Z: 20}).Length(); {
		case d < 8:
			return 2
		case d < 14:
			return 5
		}
		return 0
	}
	labels := sampledImage(t, grid, labelAt)
	shift := NewEuler3D()
	_ = shift.SetTranslation(render.Vec3{X: 4, Y: -2, Z: 2})
	mapping, _ := TransformMapping(shift)
	nearest, err := Resample(context.Background(), labels, grid, mapping, ResampleOptions{Interpolator: InterpolateNearest, Padding: 0})
	if err != nil {
		t.Fatal(err)
	}
	for z := 0; z < 20; z++ {
		for y := 0; y < 24; y++ {
			for x := 0; x < 24; x++ {
				p := grid.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
				want := 0.0
				if x+2 < 24 && y-1 >= 0 && z+1 < 20 {
					want = labelAt(shift.TransformPoint(p))
				}
				if got := float64(nearest.Image.Data()[(z*24+y)*24+x]); got != want {
					t.Fatalf("nearest (%d, %d, %d) = %v, want %v", x, y, z, got, want)
				}
			}
		}
	}
	rotation := rigidTruth(t, [3]float64{0, 0, 17}, render.Vec3{X: 0.7}, render.Vec3{X: 24, Y: 24, Z: 20})
	rotated, _ := TransformMapping(rotation)
	for _, interpolator := range []Interpolator{InterpolateNearest, InterpolateLabelGaussian} {
		out, err := Resample(context.Background(), labels, grid, rotated, ResampleOptions{Interpolator: interpolator, Padding: 0})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range out.Image.Data() {
			if v != 0 && v != 2 && v != 5 {
				t.Fatalf("interpolator %d made label %v", interpolator, v)
			}
		}
		// The centre of the inner sphere stays label 2.
		centre := (10*24+12)*24 + 12
		if out.Image.Data()[centre] != 2 {
			t.Fatalf("interpolator %d: centre %v", interpolator, out.Image.Data()[centre])
		}
	}
	if _, err := Resample(context.Background(), labels, grid, rotated, ResampleOptions{Interpolator: InterpolateLabelGaussian, LabelSigma: render.Vec3{X: -1, Y: 1, Z: 1}}); !errors.Is(err, ErrResample) {
		t.Fatalf("negative sigma: %v", err)
	}
}

// TestResampleThroughADisplacementFieldMatchesTheTranslation: a constant
// field is the translation it holds; the field is undefined beyond its
// grid, and composing it with a transform applies both.
func TestResampleThroughADisplacementFieldMatchesTheTranslation(t *testing.T) {
	linear := func(p render.Vec3) float64 { return p.X + 2*p.Y - p.Z }
	grid := Grid{Size: [3]int{16, 16, 12}, Spacing: render.Vec3{X: 2, Y: 2, Z: 2}, Direction: Identity3()}
	moving := sampledImage(t, Grid{Size: [3]int{24, 24, 20}, Origin: render.Vec3{X: -8, Y: -8, Z: -8}, Spacing: render.Vec3{X: 2, Y: 2, Z: 2}, Direction: Identity3()}, linear)
	displacement := render.Vec3{X: 1.5, Y: -2.5, Z: 0.75}
	field := &DisplacementField{Size: [3]int{8, 8, 6}, Spacing: render.Vec3{X: 4, Y: 4, Z: 4}, Direction: Identity3(), Vectors: make([]render.Vec3, 8*8*6)}
	for i := range field.Vectors {
		field.Vectors[i] = displacement
	}
	fieldMapping, err := FieldMapping(field)
	if err != nil {
		t.Fatal(err)
	}
	shift := NewEuler3D()
	_ = shift.SetTranslation(render.Vec3{X: -1, Y: 1, Z: 2})
	shiftMapping, _ := TransformMapping(shift)
	composed := ComposeMappings(fieldMapping, shiftMapping)
	out, err := Resample(context.Background(), moving, grid, composed, ResampleOptions{Padding: -1})
	if err != nil {
		t.Fatal(err)
	}
	for z := 0; z < 12; z++ {
		for y := 0; y < 16; y++ {
			for x := 0; x < 16; x++ {
				i := (z*16+y)*16 + x
				p := grid.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
				// The field covers 0..28 mm plus half a sample (2 mm).
				covered := p.X < 30 && p.Y < 30 && p.Z < 22
				if out.Inside[i] != covered {
					t.Fatalf("(%d, %d, %d) inside %v, want %v", x, y, z, out.Inside[i], covered)
				}
				if covered {
					want := linear(p.Add(displacement).Add(render.Vec3{X: -1, Y: 1, Z: 2}))
					if math.Abs(float64(out.Image.Data()[i])-want) > 1e-3 {
						t.Fatalf("(%d, %d, %d) = %v, want %v", x, y, z, out.Image.Data()[i], want)
					}
				}
			}
		}
	}
}

// TestResampleReportsSourceSlicesAndCancels: an integer translation along z
// reads one moving slice per output slice, and a cancelled context stops
// the resampling.
func TestResampleReportsSourceSlicesAndCancels(t *testing.T) {
	grid := Grid{Size: [3]int{8, 8, 10}, Spacing: render.Vec3{X: 1, Y: 1, Z: 3}, Direction: Identity3()}
	moving := sampledImage(t, grid, func(p render.Vec3) float64 { return p.Z })
	shift := NewEuler3D()
	_ = shift.SetTranslation(render.Vec3{Z: 6})
	mapping, _ := TransformMapping(shift)
	out, err := Resample(context.Background(), moving, grid, mapping, ResampleOptions{Padding: -5})
	if err != nil {
		t.Fatal(err)
	}
	for z, r := range out.SourceSlices {
		if z+2 < 10 {
			if r.Empty() || r.First != z+2 || r.Last != z+2 {
				t.Fatalf("slice %d sources %+v", z, r)
			}
		} else if !r.Empty() {
			t.Fatalf("slice %d outside the moving image reads %+v", z, r)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Resample(ctx, moving, grid, mapping, ResampleOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := Resample(context.Background(), moving, Grid{Size: [3]int{0, 1, 1}, Spacing: render.Vec3{X: 1, Y: 1, Z: 1}, Direction: Identity3()}, mapping, ResampleOptions{}); !errors.Is(err, ErrResample) {
		t.Fatalf("empty grid: %v", err)
	}
	if _, err := Resample(context.Background(), moving, grid, nil, ResampleOptions{}); !errors.Is(err, ErrResample) {
		t.Fatalf("nil mapping: %v", err)
	}
}
