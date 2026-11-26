package render

import (
	"context"
	"math"
	"testing"
)

// uniformCTVolume is a CT volume of soft tissue (40 HU) whose slices differ
// only by +1 HU per slice, so a blend with 0 (water) at the border shows.
func uniformCTVolume(t *testing.T, n int) *Volume {
	t.Helper()
	stack := ctPhantomStack(n, n, n)
	for z, frame := range stack.Frames {
		for i := 0; i+1 < len(frame.PixelBytes); i += 2 {
			stored := uint16(1024 + 40 + z)
			frame.PixelBytes[i], frame.PixelBytes[i+1] = byte(stored), byte(stored>>8)
		}
	}
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	return vol
}

// TestSamplerBorderClampsWithinHalfVoxel checks the border policy (#1005):
// within half a voxel of the grid the edge voxels repeat (no blend with 0,
// which is water in CT); farther out the sample is outside.
func TestSamplerBorderClampsWithinHalfVoxel(t *testing.T) {
	const n = 6
	vol := uniformCTVolume(t, n)
	reader, err := vol.AcquireReader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	last := float64(n - 1)
	for _, c := range []struct {
		p    Vec3
		want float64
		ok   bool
	}{
		{Vec3{X: -0.4, Y: 2, Z: 0}, 40, true},
		{Vec3{X: 2, Y: last + 0.49, Z: 3}, 43, true},
		{Vec3{X: 1, Y: 1, Z: -0.4999999}, 40, true},
		{Vec3{X: 1, Y: 1, Z: last + 0.4999999}, 40 + last, true},
		{Vec3{X: -0.45, Y: -0.45, Z: last + 0.45}, 40 + last, true},
		{Vec3{X: 1, Y: 1, Z: 2.25}, 42.25, true},
		{Vec3{X: -0.51, Y: 2, Z: 2}, 0, false},
		{Vec3{X: 2, Y: 2, Z: last + 0.51}, 0, false},
	} {
		got, ok := reader.TrilinearAt(c.p)
		if ok != c.ok || ok && math.Abs(got-c.want) > 1e-9 {
			t.Errorf("TrilinearAt(%v) = %v, %v; want %v, %v", c.p, got, ok, c.want, c.ok)
		}
	}
}

// TestMPRHalfSliceTranslationIsNotBlack moves an axial plane 0.499999 slice
// beyond the first and the last slice: it still shows the edge slice, not the
// background; 0.51 slice beyond is outside.
func TestMPRHalfSliceTranslationIsNotBlack(t *testing.T) {
	const n = 6
	vol := uniformCTVolume(t, n)
	plane := func(slice float64) Plane {
		origin := vol.VoxelToPatient(Vec3{X: 0, Y: 0, Z: slice})
		return Plane{
			Origin: origin,
			U:      vol.VoxelToPatient(Vec3{X: n - 1, Y: 0, Z: slice}).Sub(origin),
			V:      vol.VoxelToPatient(Vec3{X: 0, Y: n - 1, Z: slice}).Sub(origin),
		}
	}
	for _, c := range []struct {
		slice float64
		want  float64
		valid bool
	}{
		{-0.499999, 40, true},
		{float64(n-1) + 0.499999, 40 + n - 1, true},
		{-0.51, 0, false},
		{float64(n-1) + 0.51, 0, false},
	} {
		scalar, err := ResliceObliqueScalarContext(context.Background(), vol, plane(c.slice), n, n)
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				got, ok := scalar.ValueAt(x, y)
				if ok != c.valid || ok && math.Abs(got-c.want) > 1e-6 {
					t.Fatalf("slice %v pixel (%d,%d) = %v, %v; want %v, %v", c.slice, x, y, got, ok, c.want, c.valid)
				}
			}
		}
	}
}

// TestGradientStencilClampsAtTheBorder: the gradient of a volume uniform in x
// and y is zero across its x/y faces (the stencil repeats the edge voxels
// instead of reading 0), and 1 HU per slice along z, halved (one-sided) at the
// first and last slice.
func TestGradientStencilClampsAtTheBorder(t *testing.T) {
	const n = 6
	vol := uniformCTVolume(t, n)
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		t.Fatal("no sampler")
	}
	defer sampler.Close()
	for _, tex := range []Vec3{{X: 0, Y: 0.5, Z: 0.5}, {X: 1, Y: 1, Z: 0.5}, {X: 0.5, Y: 0, Z: 0.5}} {
		g := sampler.gradientIndexAt(tex)
		if math.Abs(g.X) > 1e-9 || math.Abs(g.Y) > 1e-9 || math.Abs(g.Z-2) > 1e-9 {
			t.Errorf("gradient at %v = %v, want (0,0,2)", tex, g)
		}
		cellSample, _ := sampler.textureSample(tex)
		var cell volumeGradientCell
		if cg := cell.gradientIndex(sampler, cellSample); math.Abs(cg.X) > 1e-9 || math.Abs(cg.Y) > 1e-9 {
			t.Errorf("cell gradient at %v = %v", tex, cg)
		}
		if tg := sampler.gradientAt(tex); math.Abs(tg.X) > 1e-9 || math.Abs(tg.Y) > 1e-9 {
			t.Errorf("texture gradient at %v = %v", tex, tg)
		}
	}
	for _, z := range []float64{0, 1} {
		if g := sampler.gradientIndexAt(Vec3{X: 0.5, Y: 0.5, Z: z}); math.Abs(g.Z-1) > 1e-9 {
			t.Errorf("one-sided z gradient at slice %v = %v, want 1", z, g.Z)
		}
	}
}
