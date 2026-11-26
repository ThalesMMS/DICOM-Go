package render

import (
	"context"
	"math"
	"testing"
)

// TestPrefiltersMatchVTK compares SmoothGaussian3D (sigma 1 and 2 mm on
// 0.8×0.6×1.5 mm voxels) with vtkImageGaussianSmooth (radius factor 3) and
// Median3x3x3 with vtkImageMedian3D on the reslice.json volume.
func TestPrefiltersMatchVTK(t *testing.T) {
	var fixture struct {
		Dims     [3]int     `json:"dims"`
		Spacing  [3]float64 `json:"spacing"`
		Gaussian []struct {
			SigmaMM float64   `json:"sigma_mm"`
			Values  []float64 `json:"values"`
		} `json:"gaussian"`
		Median []float64 `json:"median"`
	}
	loadVTKFixture(t, "prefilter.json", &fixture)
	var volume vtkResliceFixture
	loadVTKFixture(t, "reslice.json", &volume)
	source := make([]float32, len(volume.Values))
	for i, v := range volume.Values {
		source[i] = float32(v)
	}
	for _, g := range fixture.Gaussian {
		got, err := SmoothGaussian3D(context.Background(), source, fixture.Dims, fixture.Spacing, g.SigmaMM)
		if err != nil {
			t.Fatal(err)
		}
		worst := 0.0
		for i, want := range g.Values {
			worst = math.Max(worst, math.Abs(float64(got[i])-want))
		}
		// float32 intermediate passes against VTK's float64 ones.
		if worst > 1e-3 {
			t.Errorf("sigma %v mm: off VTK by %g", g.SigmaMM, worst)
		}
	}
	got, err := Median3x3x3(context.Background(), source, fixture.Dims)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range fixture.Median {
		if float64(got[i]) != want {
			t.Fatalf("median voxel %d: %v, vtkImageMedian3D %v", i, got[i], want)
		}
	}
}

func TestPrefilterValidationAndPreparedVolumes(t *testing.T) {
	if _, err := SmoothGaussian3D(context.Background(), []float32{1, 2}, [3]int{3, 1, 1}, [3]float64{1, 1, 1}, 1); err == nil {
		t.Error("short volume accepted")
	}
	if _, err := SmoothGaussian3D(context.Background(), []float32{1, 2, 3}, [3]int{3, 1, 1}, [3]float64{1, 1, 1}, 0); err == nil {
		t.Error("zero sigma accepted")
	}
	if _, err := Median3x3x3(context.Background(), []float32{1, float32(math.NaN()), 3}, [3]int{3, 1, 1}); err == nil {
		t.Error("NaN accepted")
	}
	// An impulse is removed by the median and spread symmetrically by the
	// Gaussian, with the centre weight of the normalized separable kernel
	// (the border renormalization does not keep the total mass, as in VTK).
	dims := [3]int{9, 8, 7}
	impulse := make([]float32, 9*8*7)
	impulse[(3*8+4)*9+4] = 1000
	median, _ := Median3x3x3(context.Background(), impulse, dims)
	for i, v := range median {
		if v != 0 {
			t.Fatalf("median left %v at %d", v, i)
		}
	}
	smooth, _ := SmoothGaussian3D(context.Background(), impulse, dims, [3]float64{1, 1, 2}, 0.8)
	centre := func(sigma float64, radius int) float64 {
		sum := 0.0
		for k := -radius; k <= radius; k++ {
			sum += math.Exp(-float64(k*k) / (2 * sigma * sigma))
		}
		return 1 / sum
	}
	// x and y: sigma 0.8 voxel, radius 2; z: sigma 0.4 voxel, radius 1.
	want := 1000 * centre(0.8, 2) * centre(0.8, 2) * centre(0.4, 1)
	at := func(x, y, z int) float64 { return float64(smooth[(z*8+y)*9+x]) }
	if math.Abs(at(4, 4, 3)-want) > 1e-3 || at(3, 4, 3) != at(5, 4, 3) || at(4, 3, 3) != at(4, 5, 3) || at(4, 4, 2) != at(4, 4, 4) {
		t.Errorf("Gaussian centre %v (want %v), neighbours %v/%v %v/%v %v/%v", at(4, 4, 3), want,
			at(3, 4, 3), at(5, 4, 3), at(4, 3, 3), at(4, 5, 3), at(4, 4, 2), at(4, 4, 4))
	}

	vol, err := BuildVolume(ctPhantomStack(12, 10, 8))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []VRPrefilterID{VRPrefilterGaussian1mm, VRPrefilterMedian3x3x3, VRPrefilterVesselEnhance} {
		prepared, err := vol.PrepareVRVolume(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if prepared.Prefilter != id || prepared.Descriptor().Derivation != VolumeDerivationVRPrefiltered {
			t.Errorf("%s: prepared %v %v", id, prepared.Prefilter, prepared.Descriptor().Derivation)
		}
		again, _ := vol.PrepareVRVolume(id)
		if again != prepared {
			t.Errorf("%s: prepared volume not cached", id)
		}
	}
	if _, err := vol.PrepareVRVolume(VRPrefilterVesselEnhance + 1); err == nil {
		t.Error("unknown prefilter accepted")
	}
}

// BenchmarkVRPrefilters filters a 192×192×128 CT phantom.
func BenchmarkVRPrefilters(b *testing.B) {
	vol, err := BuildVolume(ctPhantomStack(192, 192, 128))
	if err != nil {
		b.Fatal(err)
	}
	sampler, _ := newVolumeSampler(vol)
	defer sampler.Close()
	source, err := samplerModalityValues(context.Background(), sampler)
	if err != nil {
		b.Fatal(err)
	}
	dims := [3]int{192, 192, 128}
	b.Run("gaussian-1mm", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := SmoothGaussian3D(context.Background(), source, dims, [3]float64{0.8, 0.8, 1.5}, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("median-3x3x3", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := Median3x3x3(context.Background(), source, dims); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("basic-smooth-5x5", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := basicSmooth5x5Context(context.Background(), sampler); err != nil {
				b.Fatal(err)
			}
		}
	})
}
