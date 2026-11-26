package registration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func sincTestGrid(size [3]int) Grid {
	return Grid{Size: size, Spacing: render.Vec3{X: 1, Y: 1, Z: 1}, Direction: Identity3()}
}

func sincTestImage(t *testing.T, size [3]int, value func(x, y, z int) float32) *Image {
	t.Helper()
	data := make([]float32, size[0]*size[1]*size[2])
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				data[(z*size[1]+y)*size[0]+x] = value(x, y, z)
			}
		}
	}
	g := sincTestGrid(size)
	im, err := NewImage(size, data, g.Origin, g.Spacing, g.Direction)
	if err != nil {
		t.Fatal(err)
	}
	return im
}

func sincIdentity(p render.Vec3) (render.Vec3, bool) { return p, true }

func TestResampleWindowedSincIdentityAndLegacyEnums(t *testing.T) {
	for i, mode := range []Interpolator{InterpolateLinear, InterpolateNearest, InterpolateLabelGaussian, InterpolateCubicBSpline, InterpolateLanczosWindowedSinc, InterpolateWelchWindowedSinc} {
		if int(mode) != i {
			t.Fatalf("interpolator %d changed enum value to %d", i, mode)
		}
	}
	for _, size := range [][3]int{{7, 6, 5}, {7, 6, 1}, {1, 1, 1}} {
		moving := sincTestImage(t, size, func(x, y, z int) float32 { return float32(x*x+3*y-2*z) + .25 })
		for _, mode := range []Interpolator{InterpolateLanczosWindowedSinc, InterpolateWelchWindowedSinc} {
			t.Run(fmt.Sprintf("%d/%v", mode, size), func(t *testing.T) {
				out, err := Resample(nil, moving, moving.Grid(), sincIdentity, ResampleOptions{Interpolator: mode})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(out.Image.Data(), moving.Data()) || out.Outside != 0 || !out.Image.Grid().SameAs(moving.Grid(), 0) {
					t.Fatal("identity changed values, geometry or coverage")
				}
				for i, inside := range out.Inside {
					if !inside {
						t.Fatalf("identity sample %d is outside", i)
					}
				}
				for z, sources := range out.SourceSlices {
					if sources != (SliceRange{z, z}) {
						t.Fatalf("identity slice %d references %v", z, sources)
					}
				}
			})
		}
		zero, err := Resample(nil, moving, moving.Grid(), sincIdentity, ResampleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		linear, err := Resample(context.Background(), moving, moving.Grid(), sincIdentity, ResampleOptions{Interpolator: InterpolateLinear})
		if err != nil || !reflect.DeepEqual(zero, linear) {
			t.Fatalf("zero options changed linear semantics: %v", err)
		}
	}
}

func TestResampleWindowedSincSourceSlicesAndUndefinedMappings(t *testing.T) {
	moving := sincTestImage(t, [3]int{1, 1, 15}, func(_, _, z int) float32 { return float32(z) })
	for _, mode := range []Interpolator{InterpolateLanczosWindowedSinc, InterpolateWelchWindowedSinc} {
		for _, tc := range []struct {
			name string
			z    float64
			want SliceRange
		}{{"integer", 5, SliceRange{5, 5}}, {"near-integer", 5 + 1e-14, SliceRange{5, 5}}, {"fractional", 5.25, SliceRange{1, 10}}, {"lower-border", -.25, SliceRange{0, 4}}, {"upper-border", 14.25, SliceRange{10, 14}}} {
			t.Run(fmt.Sprintf("%d/%s", mode, tc.name), func(t *testing.T) {
				g := sincTestGrid([3]int{1, 1, 1})
				g.Origin.Z = tc.z
				out, err := Resample(context.Background(), moving, g, sincIdentity, ResampleOptions{Interpolator: mode})
				if err != nil || out.SourceSlices[0] != tc.want || !out.Inside[0] {
					t.Fatalf("range at %g: out=%v err=%v want=%v", tc.z, out, err, tc.want)
				}
			})
		}
		g := sincTestGrid([3]int{4, 1, 1})
		mapping := func(p render.Vec3) (render.Vec3, bool) {
			switch p.X {
			case 0:
				return render.Vec3{Z: 5.25}, true
			case 1:
				return render.Vec3{Z: 12.25}, true
			case 2:
				return render.Vec3{}, false
			default:
				return render.Vec3{X: math.NaN()}, true
			}
		}
		out, err := Resample(context.Background(), moving, g, mapping, ResampleOptions{Interpolator: mode, Padding: -999})
		if err != nil || out.Outside != 2 || !reflect.DeepEqual(out.Inside, []bool{true, true, false, false}) || out.SourceSlices[0] != (SliceRange{1, 14}) || out.Image.Data()[2] != -999 || out.Image.Data()[3] != -999 {
			t.Fatalf("mixed mappings: out=%v err=%v", out, err)
		}
		out, err = Resample(context.Background(), moving, g, func(render.Vec3) (render.Vec3, bool) { return render.Vec3{}, false }, ResampleOptions{Interpolator: mode, Padding: 3})
		if err != nil || out.Outside != 4 || !out.SourceSlices[0].Empty() || !reflect.DeepEqual(out.Image.Data(), []float32{3, 3, 3, 3}) {
			t.Fatalf("undefined mapping must pad with no source slices: out=%v err=%v", out, err)
		}
	}
}

func TestResampleWindowedSincDoesNotSnapNearIntegerCoordinates(t *testing.T) {
	moving := sincTestImage(t, [3]int{1, 1, 15}, func(_, _, z int) float32 {
		if z == 6 {
			return 1e20
		}
		return 0
	})
	g := sincTestGrid([3]int{1, 1, 1})
	g.Origin.Z = 5 + 1e-14
	for _, mode := range []Interpolator{InterpolateLanczosWindowedSinc, InterpolateWelchWindowedSinc} {
		out, err := Resample(context.Background(), moving, g, sincIdentity, ResampleOptions{Interpolator: mode})
		if err != nil {
			t.Fatal(err)
		}
		// The metadata omits tiny neighbouring weights, but interpolation must
		// retain them. Snapping to the integer would erase this finite signal.
		if value := out.Image.Data()[0]; !(value > 0) || !finite(float64(value)) || out.SourceSlices[0] != (SliceRange{5, 5}) {
			t.Fatalf("mode %d snapped or discarded tiny interpolation weights: value=%g sources=%v", mode, value, out.SourceSlices[0])
		}
	}
}

func TestResampleWindowedSincComposesFieldAndClonedTransform(t *testing.T) {
	moving := sincTestImage(t, [3]int{9, 7, 5}, func(x, y, z int) float32 { return float32(x + 10*y + 100*z) })
	g := sincTestGrid([3]int{3, 2, 1})
	vectors := make([]render.Vec3, 6)
	for i := range vectors {
		vectors[i] = render.Vec3{X: 1, Y: 1, Z: 1}
	}
	field, err := FieldMapping(&DisplacementField{Size: g.Size, Origin: g.Origin, Spacing: g.Spacing, Direction: g.Direction, Vectors: vectors})
	if err != nil {
		t.Fatal(err)
	}
	affine := NewAffine()
	affine.SetTranslation(render.Vec3{X: 1, Y: 1, Z: 1})
	cloned, err := TransformMapping(affine)
	if err != nil {
		t.Fatal(err)
	}
	affine.SetTranslation(render.Vec3{X: 100})
	for _, mode := range []Interpolator{InterpolateLanczosWindowedSinc, InterpolateWelchWindowedSinc} {
		out, err := Resample(context.Background(), moving, g, ComposeMappings(field, cloned), ResampleOptions{Interpolator: mode})
		if err != nil || out.Outside != 0 {
			t.Fatalf("composed mapping failed: %v", err)
		}
		for y := 0; y < 2; y++ {
			for x := 0; x < 3; x++ {
				if got, want := out.Image.At(x, y, 0), moving.At(x+2, y+2, 2); got != want {
					t.Fatalf("mapping direction or transform clone changed: %g want %g", got, want)
				}
			}
		}
	}
}

func TestResampleWindowedSincRejectsNonFiniteStencilAndOutput(t *testing.T) {
	for _, mode := range []Interpolator{InterpolateLanczosWindowedSinc, InterpolateWelchWindowedSinc} {
		for _, invalid := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
			for _, sourceX := range []int{8, 9} {
				moving := sincTestImage(t, [3]int{17, 1, 1}, func(x, _, _ int) float32 {
					if x == sourceX {
						return invalid
					}
					return 1
				})
				g := sincTestGrid([3]int{1, 1, 1})
				g.Origin.X = 8 // sourceX=9 has zero weight but is still in the visited stencil.
				out, err := Resample(context.Background(), moving, g, sincIdentity, ResampleOptions{Interpolator: mode})
				if out != nil || !errors.Is(err, ErrResample) {
					t.Fatalf("mode %d invalid %g at %d returned image=%v err=%v", mode, invalid, sourceX, out, err)
				}
			}
		}
		moving := sincTestImage(t, [3]int{17, 1, 1}, func(x, _, _ int) float32 {
			if x == 0 {
				return float32(math.NaN())
			}
			return 1
		})
		g := sincTestGrid([3]int{1, 1, 1})
		g.Origin.X = 8
		if out, err := Resample(context.Background(), moving, g, sincIdentity, ResampleOptions{Interpolator: mode}); err != nil || out.Image.Data()[0] != 1 {
			t.Fatalf("invalid sample outside stencil poisoned finite sample: %v", err)
		}
		g.Origin.X = 100
		if out, err := Resample(context.Background(), moving, g, sincIdentity, ResampleOptions{Interpolator: mode, Padding: 7}); err != nil || out.Image.Data()[0] != 7 || out.Inside[0] {
			t.Fatalf("outside padding visited invalid samples: %v", err)
		}
		moving = sincTestImage(t, [3]int{17, 1, 1}, func(x, _, _ int) float32 {
			if x >= 8 {
				return math.MaxFloat32
			}
			return 0
		})
		g.Origin.X = 8.25
		if out, err := Resample(context.Background(), moving, g, sincIdentity, ResampleOptions{Interpolator: mode}); out != nil || !errors.Is(err, ErrResample) {
			t.Fatalf("float32 ringing overflow returned image=%v err=%v", out, err)
		}
	}
}

func TestResampleWindowedSincOutputBudgetBeforeMappingAndPrefilter(t *testing.T) {
	moving := sincTestImage(t, [3]int{1, 1, 1}, func(_, _, _ int) float32 { return 1 })
	const payload = int64(5*4 + 2*(strconv.IntSize/8))
	maxInt := int(^uint(0) >> 1)
	for mode := InterpolateLinear; mode <= InterpolateWelchWindowedSinc; mode++ {
		for _, tc := range []struct {
			name  string
			size  [3]int
			limit int64
		}{{"negative", [3]int{2, 2, 1}, -1}, {"one-byte-short", [3]int{2, 2, 1}, payload - 1}, {"count-overflow", [3]int{maxInt, 2, 1}, 0}, {"bytes-overflow", [3]int{maxInt / 3, 1, 1}, 0}, {"ranges-overflow", [3]int{1, 1, maxInt / 4}, 0}} {
			t.Run(fmt.Sprintf("%d/%s", mode, tc.name), func(t *testing.T) {
				var mappings atomic.Int64
				out, err := Resample(context.Background(), moving, sincTestGrid(tc.size), func(p render.Vec3) (render.Vec3, bool) { mappings.Add(1); return p, true }, ResampleOptions{Interpolator: mode, MaxOutputBytes: tc.limit})
				if out != nil || !errors.Is(err, ErrResample) || mappings.Load() != 0 {
					t.Fatalf("must reject before mapping/prefilter/allocation: out=%v err=%v mapping calls=%d", out, err, mappings.Load())
				}
			})
		}
		if out, err := Resample(context.Background(), moving, sincTestGrid([3]int{2, 2, 1}), sincIdentity, ResampleOptions{Interpolator: mode, MaxOutputBytes: payload}); err != nil || len(out.Inside) != 4 {
			t.Fatalf("exact budget rejected for %d: %v", mode, err)
		}
	}
	large := [3]int{12000, 12000, 1} // 720,000,016-byte returned payload on 64-bit systems; never allocated here.
	for mode := InterpolateLinear; mode <= InterpolateWelchWindowedSinc; mode++ {
		count, err := resampleOutputCount(large, ResampleOptions{Interpolator: mode})
		if isWindowedSinc(mode) {
			if count != 0 || !errors.Is(err, ErrResample) {
				t.Fatalf("new mode %d zero budget bypassed default: count=%d err=%v", mode, count, err)
			}
			var mappings atomic.Int64
			if out, err := Resample(context.Background(), moving, sincTestGrid(large), func(p render.Vec3) (render.Vec3, bool) { mappings.Add(1); return p, true }, ResampleOptions{Interpolator: mode}); out != nil || !errors.Is(err, ErrResample) || mappings.Load() != 0 {
				t.Fatalf("default must refuse before sampling: image=%v err=%v calls=%d", out, err, mappings.Load())
			}
		} else if err != nil || count != 144000000 {
			t.Fatalf("legacy zero budget lost compatibility: mode=%d count=%d err=%v", mode, count, err)
		}
		if count, err := resampleOutputCount(large, ResampleOptions{Interpolator: mode, MaxOutputBytes: 800 << 20}); err != nil || count != 144000000 {
			t.Fatalf("explicit larger cap failed: mode=%d count=%d err=%v", mode, count, err)
		}
	}
}

func TestResampleWindowedSincCancellationAndProgress(t *testing.T) {
	moving := sincTestImage(t, [3]int{16, 2, 8}, func(x, y, z int) float32 { return float32(x + y + z) })
	for _, mode := range []Interpolator{InterpolateLanczosWindowedSinc, InterpolateWelchWindowedSinc} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var mappings atomic.Int64
		if out, err := Resample(ctx, moving, sincTestGrid([3]int{12000, 12000, 1}), func(p render.Vec3) (render.Vec3, bool) { mappings.Add(1); return p, true }, ResampleOptions{Interpolator: mode}); out != nil || !errors.Is(err, context.Canceled) || mappings.Load() != 0 {
			t.Fatalf("pre-cancel reached mapping/allocation: image=%v err=%v calls=%d", out, err, mappings.Load())
		}
		ctx, cancel = context.WithCancel(context.Background())
		mappings.Store(0)
		var progress atomic.Int64
		out, err := Resample(ctx, moving, sincTestGrid([3]int{256, 1, 1}), func(p render.Vec3) (render.Vec3, bool) {
			if mappings.Add(1) == 3 {
				cancel()
			}
			return render.Vec3{X: p.X / 32}, true
		}, ResampleOptions{Interpolator: mode, Progress: func(_, _ int) { progress.Add(1) }})
		if out != nil || !errors.Is(err, context.Canceled) || mappings.Load() != 3 || progress.Load() != 0 {
			t.Fatalf("single-slice cancellation returned partial result or progress: image=%v err=%v calls=%d progress=%d", out, err, mappings.Load(), progress.Load())
		}
		cancel()
		var dones []int
		out, err = Resample(context.Background(), moving, moving.Grid(), sincIdentity, ResampleOptions{Interpolator: mode, Progress: func(done, total int) {
			if total != 8 {
				t.Errorf("progress total=%d", total)
			}
			dones = append(dones, done) // callback serialization is required, checked under -race.
		}})
		if err != nil || out == nil || !reflect.DeepEqual(dones, []int{1, 2, 3, 4, 5, 6, 7, 8}) {
			t.Fatalf("complete progress sequence=%v err=%v", dones, err)
		}
		invalid := sincTestImage(t, [3]int{16, 2, 8}, func(_, _, _ int) float32 { return float32(math.NaN()) })
		out, err = Resample(context.Background(), invalid, invalid.Grid(), sincIdentity, ResampleOptions{Interpolator: mode})
		if out != nil || !errors.Is(err, ErrResample) {
			t.Fatalf("concurrent kernel errors returned partial image: image=%v err=%v", out, err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		out, err = Resample(ctx, invalid, sincTestGrid([3]int{1, 1, 1}), func(p render.Vec3) (render.Vec3, bool) { cancel(); return p, true }, ResampleOptions{Interpolator: mode})
		cancel()
		if out != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation lost priority: image=%v err=%v", out, err)
		}
	}
}
