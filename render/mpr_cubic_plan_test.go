package render

import (
	"context"
	"math"
	"testing"
)

// cubicPlanesFor returns separable reslices of vol at fractional voxel
// positions (axial, coronal, sagittal, one reaching past the volume) and an
// oblique one that keeps the per-sample path.
func cubicPlanesFor(vol *Volume) map[string]Plane {
	nx, ny, nz := float64(vol.Cols-1), float64(vol.Rows-1), float64(vol.Depth-1)
	at := func(x, y, z float64) Vec3 { return vol.VoxelToPatient(Vec3{X: x, Y: y, Z: z}) }
	plane := func(o, u, v Vec3) Plane {
		origin := at(o.X, o.Y, o.Z)
		return Plane{Origin: origin, U: at(u.X, u.Y, u.Z).Sub(origin), V: at(v.X, v.Y, v.Z).Sub(origin)}
	}
	return map[string]Plane{
		"axial":    plane(Vec3{X: 0.3, Y: 0.7, Z: 0.41 * nz}, Vec3{X: nx - 0.2, Y: 0.7, Z: 0.41 * nz}, Vec3{X: 0.3, Y: ny - 0.6, Z: 0.41 * nz}),
		"coronal":  plane(Vec3{X: 0.6, Y: 0.53 * ny, Z: 0.2}, Vec3{X: nx - 0.1, Y: 0.53 * ny, Z: 0.2}, Vec3{X: 0.6, Y: 0.53 * ny, Z: nz - 0.3}),
		"sagittal": plane(Vec3{X: 0.47 * nx, Y: 0.4, Z: 0.1}, Vec3{X: 0.47 * nx, Y: ny - 0.3, Z: 0.1}, Vec3{X: 0.47 * nx, Y: 0.4, Z: nz - 0.2}),
		"beyond":   plane(Vec3{X: -1.7, Y: 2.25, Z: 3}, Vec3{X: nx + 1.9, Y: 2.25, Z: 3}, Vec3{X: -1.7, Y: ny + 1.4, Z: 3}),
		"oblique":  plane(Vec3{X: 1.5, Y: 2.25, Z: 1.75}, Vec3{X: nx - 3.3, Y: 0.4 * ny, Z: 0.6 * nz}, Vec3{X: 0.2 * nx, Y: ny - 1.3, Z: 0.9 * nz}),
	}
}

// cubicTestVolumes are a packed 16-bit CT phantom, an 8-bit volume read as
// float32 values, and a volume with irregular slice positions (positionToSlice
// through the position table).
func cubicTestVolumes(t *testing.T) map[string]*Volume {
	t.Helper()
	irregular := gradientXZStack(13, 17, 9)
	for z, frame := range irregular.Frames {
		frame.ImagePosition = []float64{0, 0, float64(z) + 2e-4*float64(z*z)}
	}
	volumes := map[string]*Volume{}
	for name, stack := range map[string]*Stack{
		"ct-packed16": ctPhantomStack(37, 29, 21),
		"gradient8":   gradientXZStack(19, 23, 11),
		"irregular":   irregular,
	} {
		vol, err := BuildVolume(stack)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		volumes[name] = vol
	}
	if irregular := volumes["irregular"]; irregular.Geometry().RequiresResampling || irregular.uniformSliceStep() != 0 {
		t.Fatal("the irregular volume should map slice positions through its table")
	}
	return volumes
}

func sameBits(a, b float64) bool { return math.Float64bits(a) == math.Float64bits(b) }

// TestSeparableCubicResliceIsBitIdentical (#1256): reslices and slabs that
// read precomputed taps equal the per-sample cubic path bit for bit, on
// packed 16-bit, float32 and payload samplers, inside and past the volume.
func TestSeparableCubicResliceIsBitIdentical(t *testing.T) {
	for volumeName, vol := range cubicTestVolumes(t) {
		sampler, ok := newVolumeSampler(vol)
		if !ok {
			t.Fatalf("%s: no sampler", volumeName)
		}
		defer sampler.Close()
		vol := sampler.vol
		for planeName, plane := range cubicPlanesFor(vol) {
			name := volumeName + "/" + planeName
			const outW, outH = 41, 29
			got, err := resliceScalarWithSamplerContext(context.Background(), sampler, plane, outW, outH, defaultScalarVOI(), MPRLimits{}, InterpolationCubic)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			denomW, denomH := float64(outW-1), float64(outH-1)
			separable := 0
			for y := 0; y < outH; y++ {
				row := vol.voxelLine(plane.Origin.Add(plane.V.Scale(float64(y)/denomH)), plane.U)
				if _, ok := row.separableAxis(); ok {
					separable++
				}
				for x := 0; x < outW; x++ {
					want, wantOK := sampler.cubicAt(row.at(float64(x) / denomW))
					value, valueOK := got.ValueAt(x, y)
					if valueOK != wantOK || (wantOK && !sameBits(value, want)) {
						t.Fatalf("%s (%d,%d) = %v %v, per-sample %v %v", name, x, y, value, valueOK, want, wantOK)
					}
				}
			}
			if (separable == outH) != (planeName != "oblique") {
				t.Fatalf("%s: %d of %d rows separable", name, separable, outH)
			}

			// Slabs along the plane normal, for each reduction.
			normal := plane.U.Cross(plane.V).Normalize()
			for _, test := range []struct {
				mode      SlabMode
				trapezoid bool
			}{{SlabMIP, false}, {SlabMinIP, false}, {SlabAverage, true}, {SlabSum, false}} {
				const samples, step, first = 7, 0.37, -3.0
				slab, err := resliceScalarSlabWithSamplerContext(context.Background(), sampler, plane, normal, 13, 11,
					first, samples, step, test.mode, defaultScalarVOI(), MPRLimits{}, InterpolationCubic, test.trapezoid)
				if err != nil {
					t.Fatalf("%s slab %d: %v", name, test.mode, err)
				}
				for y := 0; y < 11; y++ {
					for x := 0; x < 13; x++ {
						ray := vol.voxelLine(plane.At(float64(x)/12, float64(y)/10), normal.Normalize())
						sample := func(i int) (float64, bool) { return sampler.cubicAt(ray.at((first + float64(i)) * step)) }
						var want float64
						var wantOK bool
						if test.trapezoid {
							want, wantOK = reduceTrapezoid(test.mode, samples, sample)
						} else {
							want, wantOK = reduceCachedScalarContext(context.Background(), test.mode, 0, samples-1, sample)
						}
						value, valueOK := slab.ValueAt(x, y)
						if valueOK != wantOK || (wantOK && !sameBits(value, want)) {
							t.Fatalf("%s slab %d (%d,%d) = %v %v, per-sample %v %v", name, test.mode, x, y, value, valueOK, want, wantOK)
						}
					}
				}
			}
		}
	}
}

// TestCubicFromTapsMatchesCubicAtOnEverySampler compares the precomputed
// taps with cubicAt at scattered points for the float32, packed 16-bit and
// payload readers.
func TestCubicFromTapsMatchesCubicAtOnEverySampler(t *testing.T) {
	vol, err := BuildVolume(ctPhantomStack(19, 11, 9))
	if err != nil {
		t.Fatal(err)
	}
	packed, ok := newVolumeSampler(vol)
	if !ok {
		t.Fatal("no sampler")
	}
	defer packed.Close()
	decoded, ok := newDecodedVolumeSampler(vol)
	if !ok {
		t.Fatal("no decoded sampler")
	}
	defer decoded.Close()
	payload := *packed
	payload.values, payload.signed16, payload.unsigned16 = nil, nil, nil
	samplers := map[string]*volumeSampler{"packed": packed, "float32": decoded, "payload": &payload}
	if !packed.packed16() || !decoded.directValues() || payload.packed16() || payload.directValues() {
		t.Fatalf("readers: packed %v, float32 %v", packed.packed16(), decoded.directValues())
	}
	for name, s := range samplers {
		for i := 0; i < 400; i++ {
			p := Vec3{X: math.Mod(float64(i)*0.731, 19.6) - 0.55, Y: math.Mod(float64(i)*0.377, 11.4) - 0.45, Z: math.Mod(float64(i)*0.519, 9.2) - 0.3}
			if i%7 == 0 {
				p.X = math.Floor(p.X)
			}
			x := newCubicAxisTaps(p.X, s.cols, 1)
			y := newCubicAxisTaps(p.Y, s.rows, s.cols)
			z := newCubicAxisTaps(p.Z, s.depth, s.cols*s.rows)
			got, gotOK := s.cubicFromTaps(&x, &y, &z)
			want, wantOK := s.cubicAt(p)
			if gotOK != wantOK || (wantOK && !sameBits(got, want)) {
				t.Fatalf("%s at %v: %v %v, cubicAt %v %v", name, p, got, gotOK, want, wantOK)
			}
		}
	}
}
