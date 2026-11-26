package registration

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func TestResampleAffineIdentityMatchesStagedSamples(t *testing.T) {
	ctx := context.Background()
	identity := NewAffine().GeometryAffine()
	identityMapping := func(p render.Vec3) (render.Vec3, bool) { return p, true }
	tests := []struct {
		name      string
		origin    render.Vec3
		direction Matrix3
		exact     bool
	}{
		{"permuted", render.Vec3{X: 16, Y: -24, Z: 9}, Matrix3{{0, -1, 0}, {1, 0, 0}, {0, 0, 1}}, true},
		{"rounded origin", render.Vec3{X: 1 << 54}, Identity3(), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := []float32{
				math.Float32frombits(0x80000000), 7, -12, math.Float32frombits(0x00000001),
				math.Float32frombits(0x7fc12345), math.Float32frombits(0x7f812345),
				float32(math.Inf(1)), float32(math.Inf(-1)),
				23, 17, 81, 51, 19, 2, 31, 47,
			}
			moving, err := NewImage([3]int{4, 2, 2}, data, test.origin, render.Vec3{X: .5, Y: 1, Z: 2}, test.direction)
			must(t, err)
			if got := moving.exactIdentityGrid(ctx); got != test.exact {
				t.Fatalf("exact round trip = %v, want %v", got, test.exact)
			}
			for _, mode := range []Interpolator{InterpolateLinear, InterpolateNearest} {
				opts := ResampleOptions{Interpolator: mode, Padding: -1024}
				want, err := Resample(ctx, moving, moving.Grid(), identityMapping, opts)
				must(t, err)
				var progress []int
				opts.Progress = func(done, total int) {
					if total != 2 {
						t.Errorf("mode %v: progress total = %d, want 2", mode, total)
					}
					progress = append(progress, done)
				}
				got, err := ResampleAffine(ctx, moving, moving.Grid(), identity, opts)
				must(t, err)
				if got.Image.Grid() != want.Image.Grid() ||
					got.Outside != want.Outside || !slices.Equal(got.Inside, want.Inside) ||
					!slices.Equal(got.SourceSlices, want.SourceSlices) || !slices.Equal(progress, []int{1, 2}) {
					t.Fatalf("mode %v: grid, inside mask, padding count, provenance, or progress differs", mode)
				}
				for i, sample := range got.Image.Data() {
					if bits, expected := math.Float32bits(sample), math.Float32bits(want.Image.Data()[i]); bits != expected {
						t.Errorf("mode %v sample %d = %#08x, want %#08x", mode, i, bits, expected)
					}
				}
				got.Image.Data()[1] = -2000
				if data[1] != 7 {
					t.Errorf("mode %v: output aliases the moving data", mode)
				}
			}
		})
	}
}

// The affine path operates in LPS across distinct fixed and moving grids.
func TestResampleAffinePreservesReferenceGridAndSliceProvenance(t *testing.T) {
	moving, err := NewImage([3]int{3, 3, 2}, []float32{
		0, 1, 2, 10, 11, 12, 20, 21, 22,
		100, 101, 102, 110, 111, 112, 120, 121, 122,
	}, render.Vec3{X: 10, Y: -4, Z: 20}, render.Vec3{X: 2, Y: 4, Z: 8},
		Matrix3{{0, -1, 0}, {1, 0, 0}, {0, 0, 1}})
	must(t, err)
	fixed := Grid{
		Size:      [3]int{5, 2, 4},
		Origin:    render.Vec3{X: 1, Y: 2, Z: 3},
		Spacing:   render.Vec3{X: 1, Y: 2, Z: 4},
		Direction: Matrix3{{0, 1, 0}, {0, 0, 1}, {1, 0, 0}},
	}
	// The transform sends fixed index (x,y,z) to moving continuous index
	// (x-0.5, y+z/4, 3z/4-0.5). The first x center maps to the included
	// lower half-pixel boundary, and the fourth to the excluded upper one.
	affine := NewAffine()
	must(t, affine.SetMatrix(Matrix3{{-2, -.25, 0}, {0, 0, 2}, {0, 1.5, 0}}))
	must(t, affine.SetOffset(render.Vec3{X: 12.5, Y: -11, Z: 13}))

	tests := []struct {
		name   string
		method Interpolator
		values [6][3]float32
		slices [3]SliceRange
	}{
		{
			name:   "linear",
			method: InterpolateLinear,
			values: [6][3]float32{{0, .5, 1.5}, {10, 10.5, 11.5}, {27.5, 28, 29}, {37.5, 38, 39}, {105, 105.5, 106.5}, {115, 115.5, 116.5}},
			slices: [3]SliceRange{{0, 0}, {0, 1}, {1, 1}},
		},
		{
			name:   "nearest",
			method: InterpolateNearest,
			values: [6][3]float32{{0, 1, 2}, {10, 11, 12}, {0, 1, 2}, {10, 11, 12}, {110, 111, 112}, {120, 121, 122}},
			slices: [3]SliceRange{{0, 0}, {0, 0}, {1, 1}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const padding = float32(-1024)
			out, err := ResampleAffine(context.Background(), moving, fixed, affine.GeometryAffine(), ResampleOptions{Interpolator: test.method, Padding: padding})
			must(t, err)
			if out.Image.Grid() != fixed {
				t.Fatalf("output grid %+v, want reference grid %+v", out.Image.Grid(), fixed)
			}
			if out.Outside != 22 || len(out.Inside) != 40 || len(out.Image.Data()) != 40 || len(out.SourceSlices) != 4 {
				t.Fatalf("unexpected output shape or outside count: inside length %d, data length %d, source slices %d, outside %d",
					len(out.Inside), len(out.Image.Data()), len(out.SourceSlices), out.Outside)
			}
			for z, sources := range out.SourceSlices {
				if z < len(test.slices) && sources != test.slices[z] {
					t.Fatalf("slice %d uses %v, want %v", z, sources, test.slices[z])
				}
				if z == len(test.slices) && !sources.Empty() {
					t.Fatalf("all-padding slice %d has moving sources %v", z, sources)
				}
				for y := 0; y < fixed.Size[1]; y++ {
					for x := 0; x < fixed.Size[0]; x++ {
						index := (z*fixed.Size[1]+y)*fixed.Size[0] + x
						inside := z < 3 && x < 3
						want := padding
						if inside {
							want = test.values[z*2+y][x]
						}
						if out.Inside[index] != inside || out.Image.Data()[index] != want {
							t.Fatalf("sample (%d,%d,%d) = %g/%t, want %g/%t", x, y, z, out.Image.Data()[index], out.Inside[index], want, inside)
						}
					}
				}
			}
		})
	}
}

func TestResampleAffineRejectsBadGeometryAndCancelsWithoutPublishing(t *testing.T) {
	geom := render.Vec3{X: 1, Y: 1, Z: 1}
	moving, err := NewImage([3]int{2, 2, 2}, make([]float32, 8), render.Vec3{}, geom, Identity3())
	must(t, err)
	affine := NewAffine().GeometryAffine()
	malformed := affine
	malformed[3] = math.Inf(1)
	if out, err := ResampleAffine(context.Background(), moving, moving.Grid(), malformed, ResampleOptions{}); out != nil || !errors.Is(err, ErrResample) {
		t.Fatalf("nonfinite affine produced %v, %v; want nil, ErrResample", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := ResampleAffine(ctx, moving, moving.Grid(), affine, ResampleOptions{
		Progress: func(done, total int) { cancel() },
	})
	if out != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled affine produced %v, %v; want nil, context.Canceled", out, err)
	}
}

func TestResampleAffineKeepsStagedNearestHalfVoxelTie(t *testing.T) {
	moving, err := NewImage([3]int{4, 1, 1}, []float32{10, 20, 30, 40},
		render.Vec3{X: 10.1}, render.Vec3{X: .7, Y: 1, Z: 1}, Identity3())
	must(t, err)
	affine := NewAffine()
	must(t, affine.SetOffset(render.Vec3{X: .35}))
	mapping, err := TransformMapping(affine)
	must(t, err)
	options := ResampleOptions{Interpolator: InterpolateNearest}
	generic, err := Resample(context.Background(), moving, moving.Grid(), mapping, options)
	must(t, err)
	typed, err := ResampleAffine(context.Background(), moving, moving.Grid(), affine.GeometryAffine(), options)
	must(t, err)
	// At index 2, precomposing all three matrices would yield 2.4999999999999996
	// and read label 30. The staged arithmetic arrives at 2.5.
	if got := typed.Image.At(2, 0, 0); got != 40 || got != generic.Image.At(2, 0, 0) {
		t.Fatalf("typed/staged labels = %g/%g, want 40/40", got, generic.Image.At(2, 0, 0))
	}
}
