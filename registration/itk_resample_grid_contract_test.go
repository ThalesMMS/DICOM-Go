package registration

import (
	"context"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// itkResampleImageTest2 and itkResampleImageTest3 cover the use of a reference
// image grid. This fixture assigns different directions to the two grids and
// uses a fixed-to-moving affine to exercise their physical-space relationship.
func TestITKResamplePreservesReferenceGridThroughAffine(t *testing.T) {
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
	mapping, err := TransformMapping(affine)
	must(t, err)
	// TransformMapping takes ownership of a snapshot of the mapping.
	must(t, affine.SetOffset(render.Vec3{X: -50, Y: 100, Z: 300}))

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
			out, err := Resample(context.Background(), moving, fixed, mapping, ResampleOptions{Interpolator: test.method, Padding: padding})
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
