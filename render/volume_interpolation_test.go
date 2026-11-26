package render

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"testing"
)

// quadraticStack is n³ 16-bit voxels of value x² + 2y + 3z, which the cubic
// Catmull-Rom kernel reproduces exactly between interior voxels (it has
// quadratic precision) and trilinear interpolation does not.
func quadraticStack(n int) *Stack {
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	for z := 0; z < n; z++ {
		data := make([]byte, n*n*2)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				binary.LittleEndian.PutUint16(data[2*(y*n+x):], uint16(x*x+2*y+3*z))
			}
		}
		frame := volumeTestFrame(n, n, data, z)
		frame.Metadata.BitsAllocated, frame.Metadata.BitsStored, frame.Metadata.HighBit = 16, 16, 15
		stack.Frames = append(stack.Frames, frame)
	}
	return stack
}

// labelStack is a labelmap: label 1..4 by quadrant of an n×n×n volume.
func labelStack(n int) *Stack {
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	for z := 0; z < n; z++ {
		data := make([]byte, n*n)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				data[y*n+x] = byte(1 + 2*(y*2/n) + x*2/n)
			}
		}
		stack.Frames = append(stack.Frames, volumeTestFrame(n, n, data, z))
	}
	return stack
}

func TestMPRResliceInterpolationModes(t *testing.T) {
	vol, err := BuildVolume(quadraticStack(10))
	if err != nil {
		t.Fatal(err)
	}
	// An oblique plane through the interior at fractional voxel positions.
	origin := vol.VoxelToPatient(Vec3{X: 2.3, Y: 2.6, Z: 3.1})
	plane := Plane{
		Origin: origin,
		U:      vol.VoxelToPatient(Vec3{X: 6.1, Y: 3.4, Z: 3.9}).Sub(origin),
		V:      vol.VoxelToPatient(Vec3{X: 2.9, Y: 6.7, Z: 4.4}).Sub(origin),
	}
	const size = 9
	worst := map[Interpolation]float64{}
	for _, mode := range []Interpolation{InterpolationUnknown, InterpolationLinear, InterpolationCubic} {
		scalar, err := ResliceObliqueScalarWithOptionsContext(context.Background(), vol, plane, size, size, MPRResliceOptions{Interpolation: mode})
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				got, ok := scalar.ValueAt(x, y)
				v := vol.PatientToVoxel(plane.At(float64(x)/(size-1), float64(y)/(size-1)))
				want := v.X*v.X + 2*v.Y + 3*v.Z
				if !ok {
					t.Fatalf("mode %d pixel (%d,%d) outside", mode, x, y)
				}
				worst[mode] = math.Max(worst[mode], math.Abs(got-want))
			}
		}
	}
	if worst[InterpolationCubic] > 1e-6 {
		t.Errorf("cubic reslice of a quadratic off by %g", worst[InterpolationCubic])
	}
	if worst[InterpolationLinear] < 0.05 || worst[InterpolationUnknown] != worst[InterpolationLinear] {
		t.Errorf("trilinear error %g (zero value %g): the plane does not exercise interpolation", worst[InterpolationLinear], worst[InterpolationUnknown])
	}
	// The slab variant samples with the same mode.
	slab, err := ResliceObliqueSlabScalarWithOptionsContext(context.Background(), vol, plane, size, size, 3, SlabMax, MPRResliceOptions{Interpolation: InterpolationCubic})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := slab.ValueAt(4, 4); !ok {
		t.Error("cubic slab has no centre sample")
	}
}

// TestNearestKeepsLabels reslices and curves through a labelmap: nearest
// yields only its labels where trilinear and cubic blend them.
func TestNearestKeepsLabels(t *testing.T) {
	vol, err := BuildVolume(labelStack(8))
	if err != nil {
		t.Fatal(err)
	}
	origin := vol.VoxelToPatient(Vec3{X: 0.4, Y: 0.3, Z: 2.2})
	plane := Plane{
		Origin: origin,
		U:      vol.VoxelToPatient(Vec3{X: 6.9, Y: 1.1, Z: 3.7}).Sub(origin),
		V:      vol.VoxelToPatient(Vec3{X: 1.2, Y: 6.8, Z: 2.9}).Sub(origin),
	}
	labels := func(values []float64, valid []bool) (pure bool) {
		for i, v := range values {
			if (valid == nil || valid[i]) && (v != math.Trunc(v) || v < 1 || v > 4) {
				return false
			}
		}
		return true
	}
	for _, mode := range []Interpolation{InterpolationNearest, InterpolationLinear} {
		scalar, err := ResliceObliqueScalarWithOptionsContext(context.Background(), vol, plane, 15, 15, MPRResliceOptions{Interpolation: mode})
		if err != nil {
			t.Fatal(err)
		}
		var values []float64
		for y := 0; y < 15; y++ {
			for x := 0; x < 15; x++ {
				if v, ok := scalar.ValueAt(x, y); ok {
					values = append(values, v)
				}
			}
		}
		if pure := labels(values, nil); pure != (mode == InterpolationNearest) {
			t.Errorf("reslice mode %d: only labels %v", mode, pure)
		}
		path := NewCPRPath([]Vec3{vol.VoxelToPatient(Vec3{X: 1.3, Y: 1.7, Z: 0.6}), vol.VoxelToPatient(Vec3{X: 5.6, Y: 6.2, Z: 6.4})})
		cpr, err := RenderCPRScalar(context.Background(), CPRRequest{Mode: CPRStraightened, Volume: vol, Path: path, Width: 11, CrossSpacing: 0.7, ArcSpacing: 0.6, Interpolation: mode})
		if err != nil {
			t.Fatal(err)
		}
		if pure := labels(cpr.Values, cpr.Valid); pure != (mode == InterpolationNearest) {
			t.Errorf("CPR mode %d: only labels %v", mode, pure)
		}
	}
}

// separableCubicPlanes are reslices whose rows (and slab rays) run along
// one voxel axis at fractional positions of the 128³ CT phantom: a zoomed
// axial plane, a coronal plane and a sagittal plane.
func separableCubicPlanes(vol *Volume) map[string]Plane {
	at := func(x, y, z float64) Vec3 { return vol.VoxelToPatient(Vec3{X: x, Y: y, Z: z}) }
	planes := map[string]Plane{}
	origin := at(3.2, 5.7, 40.37)
	planes["axial"] = Plane{Origin: origin, U: at(120.9, 5.7, 40.37).Sub(origin), V: at(3.2, 119.3, 40.37).Sub(origin)}
	origin = at(2.6, 63.41, 1.3)
	planes["coronal"] = Plane{Origin: origin, U: at(124.1, 63.41, 1.3).Sub(origin), V: at(2.6, 63.41, 125.8).Sub(origin)}
	origin = at(70.73, 1.9, 2.2)
	planes["sagittal"] = Plane{Origin: origin, U: at(70.73, 126.2, 2.2).Sub(origin), V: at(70.73, 1.9, 124.6).Sub(origin)}
	return planes
}

// BenchmarkMPRCubicSeparable measures cubic reslices whose rows run along a
// voxel axis (#1256): 384×384 planes and a 256×256 slab of 9 samples.
func BenchmarkMPRCubicSeparable(b *testing.B) {
	vol, err := BuildVolume(ctPhantomStack(128, 128, 128))
	if err != nil {
		b.Fatal(err)
	}
	planes := separableCubicPlanes(vol)
	for _, name := range []string{"axial", "coronal", "sagittal"} {
		plane := planes[name]
		b.Run(name, func(b *testing.B) {
			options := MPRResliceOptions{Interpolation: InterpolationCubic}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := ResliceObliqueScalarWithOptionsContext(context.Background(), vol, plane, 384, 384, options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("axial-slab9", func(b *testing.B) {
		options := MPRResliceOptions{Interpolation: InterpolationCubic}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := ResliceObliqueSlabScalarWithOptionsContext(context.Background(), vol, planes["axial"], 256, 256, 9, SlabMIP, options); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkMPRInterpolation measures one 256×256 oblique reslice of a 128³
// volume per interpolation mode.
func BenchmarkMPRInterpolation(b *testing.B) {
	vol, err := BuildVolume(ctPhantomStack(128, 128, 128))
	if err != nil {
		b.Fatal(err)
	}
	origin := vol.VoxelToPatient(Vec3{X: 5.5, Y: 10.25, Z: 20.75})
	plane := Plane{
		Origin: origin,
		U:      vol.VoxelToPatient(Vec3{X: 120.3, Y: 30.1, Z: 60.2}).Sub(origin),
		V:      vol.VoxelToPatient(Vec3{X: 20.4, Y: 118.7, Z: 90.9}).Sub(origin),
	}
	for _, mode := range []Interpolation{InterpolationNearest, InterpolationLinear, InterpolationCubic} {
		b.Run(fmt.Sprint([]string{"", "nearest", "linear", "cubic"}[mode]), func(b *testing.B) {
			options := MPRResliceOptions{Interpolation: mode}
			if _, err := ResliceObliqueScalarWithOptionsContext(context.Background(), vol, plane, 256, 256, options); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ResliceObliqueScalarWithOptionsContext(context.Background(), vol, plane, 256, 256, options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
