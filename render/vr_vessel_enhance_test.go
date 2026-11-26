package render

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// angioPhantom is a synthetic CT angiography in HU on 0.7 mm voxels (x, y)
// and 1 mm slices: soft tissue at 40 HU with noise, a contrast-filled vessel
// of radius 2.5 mm along z at 350 HU, a thin vessel of radius 1 mm along x
// at 300 HU, a 3 mm plate of cortical bone at 1000 HU and a 7 mm ball of
// bone at 700 HU. It returns the values, x-fastest, and the voxel of each
// structure's centre.
func angioPhantom() (values []float32, dims [3]int, spacing [3]float64, at map[string]int) {
	dims = [3]int{72, 72, 48}
	spacing = [3]float64{0.7, 0.7, 1}
	rng := rand.New(rand.NewSource(1099))
	values = make([]float32, dims[0]*dims[1]*dims[2])
	pos := func(i, axis int) float64 { return (float64(i) - float64(dims[axis]-1)/2) * spacing[axis] }
	index := func(x, y, z int) int { return (z*dims[1]+y)*dims[0] + x }
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				px, py, pz := pos(x, 0), pos(y, 1), pos(z, 2)
				hu := 40.0
				switch {
				case math.Hypot(px+8, py+8) <= 2.5:
					hu = 350
				case math.Hypot(py-10, pz-6) <= 1:
					hu = 300
				case math.Abs(px-15) <= 1.5:
					hu = 1000
				case math.Sqrt((px+10)*(px+10)+(py-10)*(py-10)+(pz+8)*(pz+8)) <= 7:
					hu = 700
				}
				values[index(x, y, z)] = float32(hu + rng.NormFloat64()*12)
			}
		}
	}
	vox := func(px, py, pz float64) int {
		c := func(p float64, axis int) int {
			return int(math.Round(p/spacing[axis] + float64(dims[axis]-1)/2))
		}
		return index(c(px, 0), c(py, 1), c(pz, 2))
	}
	at = map[string]int{
		"vessel": vox(-8, -8, 0), "vessel wall": vox(-8+2.1, -8, 0), "thin vessel": vox(0, 10, 6),
		"plate": vox(15, 0, 0), "ball": vox(-10, 10, -8),
	}
	return values, dims, spacing, at
}

func TestVesselEnhanceKeepsVesselsAndFadesBone(t *testing.T) {
	values, dims, spacing, at := angioPhantom()
	orig := append([]float32(nil), values...)
	out, err := VesselEnhance(context.Background(), values, dims, spacing)
	if err != nil {
		t.Fatal(err)
	}
	for i := range values {
		if values[i] != orig[i] {
			t.Fatal("VesselEnhance modified its input")
		}
	}
	floor := orig[0]
	for _, v := range orig {
		floor = min(floor, v)
	}
	// Fraction of each structure's contrast above the floor that is kept.
	kept := func(name string) float64 {
		i := at[name]
		return float64(out[i]-floor) / float64(orig[i]-floor)
	}
	for _, name := range []string{"vessel", "vessel wall", "thin vessel"} {
		if k := kept(name); k < 0.95 {
			t.Errorf("%s keeps %.2f of its value (%v of %v HU)", name, k, out[at[name]], orig[at[name]])
		}
	}
	for _, name := range []string{"plate", "ball"} {
		if k := kept(name); k > 0.05 {
			t.Errorf("%s keeps %.2f of its value (%v of %v HU)", name, k, out[at[name]], orig[at[name]])
		}
	}
	// Soft tissue: noise that looks tubular keeps part of its value, the
	// rest fades; on average most of it goes.
	tissue, sum := 0, 0.0
	for i, v := range orig {
		if v < 100 {
			tissue++
			sum += float64(out[i]-floor) / float64(v-floor)
		}
	}
	if mean := sum / float64(tissue); mean > 0.2 {
		t.Errorf("soft tissue keeps %.2f of its value on average", mean)
	}
	// A MIP along z through the enhanced volume shows the vessels brighter
	// than any bone.
	mip := func(v []float32, x, y int) float32 {
		best := float32(math.Inf(-1))
		for z := 0; z < dims[2]; z++ {
			best = max(best, v[(z*dims[1]+y)*dims[0]+x])
		}
		return best
	}
	vx, vy := at["vessel"]%dims[0], at["vessel"]/dims[0]%dims[1]
	px, py := at["plate"]%dims[0], at["plate"]/dims[0]%dims[1]
	if mip(out, vx, vy) <= mip(out, px, py) || mip(orig, vx, vy) >= mip(orig, px, py) {
		t.Errorf("MIP vessel %v / plate %v (source %v / %v)", mip(out, vx, vy), mip(out, px, py), mip(orig, vx, vy), mip(orig, px, py))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VesselEnhance(ctx, values, dims, spacing); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
	if _, err := VesselEnhance(context.Background(), values[:10], dims, spacing); err == nil {
		t.Error("short volume accepted")
	}
	if _, err := VesselEnhance(context.Background(), values, dims, [3]float64{0.7, 0, 1}); err == nil {
		t.Error("zero spacing accepted")
	}
}

func TestVesselEnhanceSigmas(t *testing.T) {
	got := VesselEnhanceSigmasMM([3]float64{0.4, 0.4, 0.6})
	if len(got) != VesselEnhanceSigmaSteps || got[0] != VesselEnhanceMinSigmaMM || got[len(got)-1] != VesselEnhanceMaxSigmaMM {
		t.Fatalf("fine voxels: %v", got)
	}
	if got := VesselEnhanceSigmasMM([3]float64{1.2, 1.2, 3}); got[0] != 1.2 {
		t.Fatalf("coarse voxels start at %v", got[0])
	}
	if got := VesselEnhanceSigmasMM([3]float64{20, 20, 20}); len(got) != 1 || got[0] != VesselEnhanceMaxSigmaMM {
		t.Fatalf("very coarse voxels: %v", got)
	}
	for v, want := range map[float64]float64{0: 0, 0.2: 0, 0.4: 0.5, 0.6: 1, 0.86: 1} {
		if got := VesselEnhanceWeight(v); math.Abs(got-want) > 1e-12 {
			t.Errorf("weight(%v) = %v, want %v", v, got, want)
		}
	}
}

// TestVesselEnhancePreparedVolume runs the prefilter through the prepared
// VR volume of a stored-value CT stack.
func TestVesselEnhancePreparedVolume(t *testing.T) {
	values, dims, _, at := angioPhantom()
	stack := &Stack{PixelSpacing: []float64{0.7, 0.7}, SliceThickness: 1}
	for z := 0; z < dims[2]; z++ {
		data := make([]byte, dims[0]*dims[1]*2)
		for i := 0; i < dims[0]*dims[1]; i++ {
			stored := int(math.Round(float64(values[z*dims[0]*dims[1]+i]))) + 1024
			binary.LittleEndian.PutUint16(data[2*i:], uint16(max(0, stored)))
		}
		stack.Frames = append(stack.Frames, &Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(dims[1]), Columns: uint16(dims[0]), SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 12, HighBit: 11, PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder: binary.LittleEndian, PixelBytes: data,
			Rescale:          Rescale{Slope: 1, Intercept: -1024},
			ImageOrientation: []float64{1, 0, 0, 0, 1, 0},
			ImagePosition:    []float64{0, 0, float64(z)},
		})
	}
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := vol.PrepareVRVolume(VRPrefilterVesselEnhance)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Prefilter != VRPrefilterVesselEnhance || prepared.Descriptor().Derivation != VolumeDerivationVRPrefiltered {
		t.Fatalf("prepared %v %v", prepared.Prefilter, prepared.Descriptor().Derivation)
	}
	sampler, err := prepared.newSampler()
	if err != nil {
		t.Fatal(err)
	}
	defer sampler.Close()
	value := func(i int) float64 {
		v, ok := sampler.valueAt(i%dims[0], i/dims[0]%dims[1], i/(dims[0]*dims[1]))
		if !ok {
			t.Fatalf("no sample at %d", i)
		}
		return v
	}
	if v := value(at["vessel"]); v < 250 {
		t.Errorf("prepared vessel %v HU", v)
	}
	if v := value(at["plate"]); v > 0 {
		t.Errorf("prepared plate %v HU", v)
	}
	if VRPrefilterVesselEnhance.String() != "vessel-enhance" {
		t.Errorf("name %q", VRPrefilterVesselEnhance.String())
	}
}
