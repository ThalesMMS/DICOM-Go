package render

import (
	"context"
	"encoding/binary"
	"image"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// sdfSphereStack is n³ voxels of value 127.5 + 20·(radius − distance to the
// centre): the 127.5 iso-surface is the sphere, and trilinear interpolation of
// the near-linear field puts it within a few hundredths of a voxel.
func sdfSphereStack(n int, radius float64) *Stack {
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	center := float64(n-1) / 2
	for z := 0; z < n; z++ {
		data := make([]byte, n*n*2)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				dx, dy, dz := float64(x)-center, float64(y)-center, float64(z)-center
				value := 127.5 + 20*(radius-math.Sqrt(dx*dx+dy*dy+dz*dz))
				stored := math.Round((value - 27.5) / 0.05)
				stored = math.Max(0, math.Min(4095, stored))
				binary.LittleEndian.PutUint16(data[2*(y*n+x):], uint16(stored))
			}
		}
		stack.Frames = append(stack.Frames, &Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(n), Columns: uint16(n), SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 12, HighBit: 11, PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder:        binary.LittleEndian,
			PixelBytes:       data,
			Rescale:          Rescale{Slope: 0.05, Intercept: 27.5},
			ImageOrientation: []float64{1, 0, 0, 0, 1, 0},
			ImagePosition:    []float64{0, 0, float64(z)},
		})
	}
	return stack
}

// sphereHitError is the distance of a patient point from the sphere of
// radius around the volume centre, in voxels (1 mm).
func sphereHitError(vol *Volume, p WorldPoint, radius float64) float64 {
	c := vol.VoxelToPatient(Vec3{X: float64(vol.Cols-1) / 2, Y: float64(vol.Rows-1) / 2, Z: float64(vol.Depth-1) / 2})
	return math.Abs(math.Sqrt((p.X-c.X)*(p.X-c.X)+(p.Y-c.Y)*(p.Y-c.Y)+(p.Z-c.Z)*(p.Z-c.Z)) - radius)
}

// TestSurfaceHitIsSubVoxel marches rays at a step of about 2.3 voxels: the
// bisection puts the hits on the analytic sphere within 0.2 voxel, where the
// first sample at or above the iso-value was up to a step off.
func TestSurfaceHitIsSubVoxel(t *testing.T) {
	const radius = 11
	vol, err := BuildVolume(sdfSphereStack(33, radius))
	if err != nil {
		t.Fatal(err)
	}
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		t.Fatal("no sampler")
	}
	defer sampler.Close()
	size := vol.PhysicalSize()
	boxMin, boxMax := size.Scale(-0.5), size.Scale(0.5)
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	step := size.Length() / 24
	rayGen := newVRRayGenerator(cam, 40, 40)
	hits, worstRefined, worstFirst := 0, 0.0, 0.0
	for py := 0; py < 40; py++ {
		row := rayGen.rowBase(py)
		for px := 0; px < 40; px++ {
			o, d := rayGen.ray(px, row)
			t0, t1, hit := intersectAABB(o, d, boxMin, boxMax)
			if !hit {
				continue
			}
			tex, ok := firstVRSurfaceHitContext(context.Background(), sampler, o, d, t0, t1, step, boxMin, size, 127.5, nil)
			if !ok {
				continue
			}
			hits++
			p := vol.VoxelToPatient(vol.TextureToVoxel(tex))
			worstRefined = math.Max(worstRefined, sphereHitError(vol, WorldPoint{X: p.X, Y: p.Y, Z: p.Z}, radius))
			// The unrefined hit: the first sample at or above the iso-value.
			for tt := t0; tt <= t1; tt += step {
				pw := o.Add(d.Scale(tt))
				texFirst := Vec3{X: (pw.X - boxMin.X) / size.X, Y: (pw.Y - boxMin.Y) / size.Y, Z: (pw.Z - boxMin.Z) / size.Z}
				if value, ok := sampler.textureAt(texFirst); ok && value >= 127.5 {
					q := vol.VoxelToPatient(vol.TextureToVoxel(texFirst))
					worstFirst = math.Max(worstFirst, sphereHitError(vol, WorldPoint{X: q.X, Y: q.Y, Z: q.Z}, radius))
					break
				}
			}
		}
	}
	if hits < 300 {
		t.Fatalf("%d hits", hits)
	}
	if worstRefined > 0.2 {
		t.Errorf("refined hits up to %.3f voxel off the sphere", worstRefined)
	}
	if worstFirst < 1 {
		t.Errorf("first-sample hits only %.3f voxel off: the test does not exercise refinement", worstFirst)
	}
}

// TestPickIsSubVoxel picks points on the sphere with an opaque-above preset.
func TestPickIsSubVoxel(t *testing.T) {
	const radius = 11
	vol, err := BuildVolume(sdfSphereStack(33, radius))
	if err != nil {
		t.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	tf := NewVRTransferFunction(
		[]TFColorStop{{HU: 0, R: 1, G: 1, B: 1}, {HU: 255, R: 1, G: 1, B: 1}},
		[]TFAlphaStop{{HU: -1000, A: 0}, {HU: 127.4, A: 0}, {HU: 127.6, A: 1}, {HU: 2000, A: 1}},
	)
	preset := VRPreset{Name: "opaque", TF: tf, Mode: VRModeDVR}
	window := WindowLevel{Center: -2000, Width: 2} // no window attenuation above it
	picked, worst := 0, 0.0
	for y := 4; y < 40; y += 3 {
		for x := 4; x < 40; x += 3 {
			p, ok := PickVR(vol, cam, preset, window, nil, x, y, 44, 44)
			if !ok {
				continue
			}
			picked++
			worst = math.Max(worst, sphereHitError(vol, p, radius))
		}
	}
	if picked < 40 {
		t.Fatalf("%d picks", picked)
	}
	if worst > 0.25 {
		t.Errorf("picks up to %.3f voxel off the sphere", worst)
	}
}

func TestVRRayJitter(t *testing.T) {
	// The values are part of the CPU/WebGPU contract (the WGSL hash).
	for _, c := range []struct {
		x, y int
		want float64
	}{{0, 0, 0}, {1, 0, VRRayJitter(1, 0)}, {123, 456, VRRayJitter(123, 456)}} {
		if got := VRRayJitter(c.x, c.y); got != c.want || got < 0 || got >= 1 {
			t.Errorf("VRRayJitter(%d,%d) = %v", c.x, c.y, got)
		}
	}
	sum, distinct := 0.0, map[float64]bool{}
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			j := VRRayJitter(x, y)
			if j < 0 || j >= 1 || j*(1<<24) != math.Floor(j*(1<<24)) {
				t.Fatalf("VRRayJitter(%d,%d) = %v", x, y, j)
			}
			sum += j
			distinct[j] = true
		}
	}
	if mean := sum / 4096; math.Abs(mean-0.5) > 0.03 || len(distinct) < 4000 {
		t.Errorf("jitter mean %.3f, %d distinct of 4096", mean, len(distinct))
	}
}

// TestJitterReducesRings renders a thin opaque shell (1.5 voxels) at a step of
// 1.8 voxels, with and without jitter, against a finely sampled reference.
// Rings are error coherent across neighbouring pixels; jitter turns them into
// per-pixel noise, so the error's correlation between horizontal and vertical
// neighbours must fall.
func TestJitterReducesRings(t *testing.T) {
	vol, err := BuildVolume(sdfSphereStack(41, 15))
	if err != nil {
		t.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	tf := NewVRTransferFunction(
		[]TFColorStop{{HU: 0, R: 1, G: 0.8, B: 0.6}, {HU: 255, R: 1, G: 0.8, B: 0.6}},
		[]TFAlphaStop{{HU: -1000, A: 0}, {HU: 120, A: 0}, {HU: 135, A: 0.9}, {HU: 150, A: 0}, {HU: 2000, A: 0}},
	)
	preset := VRPreset{Name: "shell", TF: tf, Mode: VRModeDVR}
	window := WindowLevel{Center: -2000, Width: 2}
	const size = 96
	render := func(q VRQuality) *image.NRGBA {
		return RenderVR(vol, cam, preset, window, false, nil, q).(*image.NRGBA)
	}
	reference := render(VRQuality{Width: size, Height: size, MaxSteps: DefaultVRMaxSteps, SpacingFraction: 0.025})
	// correlation is the lag-1 autocorrelation of the red-channel error.
	correlation := func(img *image.NRGBA) float64 {
		e := func(x, y int) float64 {
			i := img.PixOffset(x, y)
			return float64(img.Pix[i]) - float64(reference.Pix[i])
		}
		mean := 0.0
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				mean += e(x, y)
			}
		}
		mean /= size * size
		energy, lagged := 0.0, 0.0
		for y := 0; y < size-1; y++ {
			for x := 0; x < size-1; x++ {
				v := e(x, y) - mean
				energy += v * v
				lagged += v*(e(x+1, y)-mean) + v*(e(x, y+1)-mean)
			}
		}
		return lagged / (2 * energy)
	}
	coarse := correlation(render(VRQuality{Width: size, Height: size, MaxSteps: DefaultVRMaxSteps, SpacingFraction: vol.PhysicalSize().Length() / 40 / math.Min(vol.ColSpacing, math.Min(vol.RowSpacing, vol.SliceSpacing))}))
	jittered := correlation(render(VRQuality{Width: size, Height: size, MaxSteps: DefaultVRMaxSteps, SpacingFraction: vol.PhysicalSize().Length() / 40 / math.Min(vol.ColSpacing, math.Min(vol.RowSpacing, vol.SliceSpacing)), Jitter: true}))
	t.Logf("neighbour error correlation: coarse %.2f, jittered %.2f", coarse, jittered)
	if coarse < 0.6 || jittered > coarse/2 {
		t.Errorf("neighbour error correlation: coarse %.2f, jittered %.2f", coarse, jittered)
	}
}
