package imagefilter_test

// External tests: comparisons with render's FIR Gaussian and the adapters
// for render.VolumeSnapshot and roi.VolumeMask. They live in the external
// test package so render may import imagefilter without an import cycle.

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

func smoothField(dims [3]int, spacing [3]float64) *imagefilter.Volume {
	v := imagefilter.NewVolume(dims, spacing)
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				px, py, pz := float64(x)*spacing[0], float64(y)*spacing[1], float64(z)*spacing[2]
				dx, dy, dz := px-35, py-30, pz-40
				v.Data[v.Index(x, y, z)] = float32(100*math.Sin(px/9)*math.Cos(py/11) +
					400*math.Exp(-(dx*dx+dy*dy+dz*dz)/(2*64)) + 3*pz - 1000)
			}
		}
	}
	return v
}

// smoothFieldExact is smoothField convolved with an isotropic Gaussian of σ
// mm: each Fourier mode is damped by exp(−σ²k²/2), the blob widens to
// √(64+σ²) and the ramp is unchanged.
func smoothFieldExact(px, py, pz, sigma float64) float64 {
	s2 := sigma * sigma
	dx, dy, dz := px-35, py-30, pz-40
	w2 := 64 + s2
	return 100*math.Sin(px/9)*math.Cos(py/11)*math.Exp(-s2/(2*81)-s2/(2*121)) +
		400*math.Pow(64/w2, 1.5)*math.Exp(-(dx*dx+dy*dy+dz*dz)/(2*w2)) + 3*pz - 1000
}

// TestRecursiveGaussianMatchesRenderFIR compares the recursive smoothing with
// render.SmoothGaussian3D, the separable FIR Gaussian (3σ support) validated
// against VTK's vtkImageGaussianSmooth, on a smooth CT-like field with
// anisotropic spacing, and both with the analytic smoothing of the field.
// Away from the borders, where the two differ by design (renormalized
// clipped kernel vs replicated edge), they agree within their approximation
// errors, of which the FIR's 3σ truncation is the larger.
func TestRecursiveGaussianMatchesRenderFIR(t *testing.T) {
	dims := [3]int{96, 80, 48}
	spacing := [3]float64{0.7, 0.9, 1.5}
	src := smoothField(dims, spacing)
	ctx := context.Background()
	for _, sigma := range []float64{1, 2, 4} {
		fir, err := render.SmoothGaussian3D(ctx, src.Data, dims, spacing, sigma)
		if err != nil {
			t.Fatal(err)
		}
		iir, err := imagefilter.SmoothRecursiveGaussian(ctx, src, imagefilter.RecursiveGaussianOptions{SigmaMM: imagefilter.IsotropicSigma(sigma)})
		if err != nil {
			t.Fatal(err)
		}
		var margin [3]int
		for a := range margin {
			margin[a] = int(math.Ceil(4 * sigma / spacing[a]))
		}
		worst, sum, count := 0.0, 0.0, 0
		worstIIR, worstFIR, moved := 0.0, 0.0, 0.0
		for z := margin[2]; z < dims[2]-margin[2]; z++ {
			for y := margin[1]; y < dims[1]-margin[1]; y++ {
				for x := margin[0]; x < dims[0]-margin[0]; x++ {
					i := src.Index(x, y, z)
					d := math.Abs(float64(iir.Data[i] - fir[i]))
					worst = max(worst, d)
					sum += d
					count++
					want := smoothFieldExact(float64(x)*spacing[0], float64(y)*spacing[1], float64(z)*spacing[2], sigma)
					worstIIR = max(worstIIR, math.Abs(float64(iir.Data[i])-want))
					worstFIR = max(worstFIR, math.Abs(float64(fir[i])-want))
					moved = max(moved, math.Abs(float64(src.Data[i])-want))
				}
			}
		}
		t.Logf("σ = %g mm: interior max |IIR − FIR| = %.4f (mean %.4f); max error vs analytic: IIR %.4f, FIR %.4f; smoothing moves values by up to %.2f", sigma, worst, sum/float64(count), worstIIR, worstFIR, moved)
		// Relative to how far the smoothing moves the field: the recursive
		// filter stays within 0.6% of the analytic result (σ = 1 mm is
		// below one z sample), and the FIR, whose kernel is truncated at
		// 2.8–3σ, within 2%.
		if worstIIR > 0.006*moved {
			t.Errorf("σ = %g mm: recursive smoothing is %.4f from the analytic result", sigma, worstIIR)
		}
		if worst > 0.02*moved {
			t.Errorf("σ = %g mm: IIR and FIR differ by %.4f in the interior", sigma, worst)
		}
	}
}

func TestVolumeFromRenderSnapshotAndMask(t *testing.T) {
	dims := [3]int{7, 5, 3}
	values := make([]float32, 7*5*3)
	for i := range values {
		values[i] = float32(i)*0.5 - 3
	}
	identity := render.GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	descriptor := render.VolumeDescriptor{
		ContractVersion:   render.VolumeSnapshotContractVersion,
		HeaderSize:        render.VolumeSnapshotHeaderSizeV1,
		VolumeGeneration:  1,
		Derivation:        render.VolumeDerivationNormalized,
		Dimensions:        [3]uint32{7, 5, 3},
		Components:        1,
		ScalarFormat:      render.VolumeScalarF32ModalityLE,
		SampleDomain:      render.VolumeSampleDomainModality,
		RowStrideBytes:    7 * 4,
		SliceStrideBytes:  7 * 5 * 4,
		ByteLength:        7 * 5 * 3 * 4,
		RescaleSlope:      1,
		SpacingMM:         [3]float64{1, 1, 1},
		IndexToPatientLPS: identity,
		PatientLPSToIndex: identity,
	}
	store := render.NewVolumeStore()
	defer store.Close()
	if _, err := store.ReplaceFloat32(descriptor, values); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireCurrent()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d, err := snapshot.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	vol, err := imagefilter.VolumeFromModalityF32(context.Background(), snapshot,
		[3]int{int(d.Dimensions[0]), int(d.Dimensions[1]), int(d.Dimensions[2])}, d.SpacingMM)
	if err != nil {
		t.Fatal(err)
	}
	if vol.SpacingMM != descriptor.SpacingMM || vol.Dims != dims {
		t.Fatalf("snapshot volume geometry %v %v", vol.Dims, vol.SpacingMM)
	}
	for i, v := range values {
		if vol.Data[i] != v {
			t.Fatalf("snapshot sample %d = %v, want %v", i, vol.Data[i], v)
		}
	}
	if _, err := imagefilter.VolumeFromModalityF32(context.Background(), snapshot, [3]int{7, 5, 4}, d.SpacingMM); err == nil {
		t.Fatal("a snapshot smaller than the dimensions was accepted")
	}

	mask := roi.NewVolumeMask(7, 5, 3, [3]float64{0.5, 0.75, 2})
	mask.Voxels[mask.Columns*mask.Rows+2*mask.Columns+3] = true
	soft, err := imagefilter.VolumeFromMask([3]int{mask.Columns, mask.Rows, mask.Slices}, [3]float64{0.5, 0.75, 2}, mask.Voxels)
	if err != nil {
		t.Fatal(err)
	}
	if soft.At(3, 2, 1) != 1 || soft.At(0, 0, 0) != 0 {
		t.Fatalf("mask volume values %v, %v", soft.At(3, 2, 1), soft.At(0, 0, 0))
	}
	smooth, err := imagefilter.SmoothRecursiveGaussian(context.Background(), soft, imagefilter.RecursiveGaussianOptions{SigmaMM: imagefilter.IsotropicSigma(1)})
	if err != nil {
		t.Fatal(err)
	}
	if smooth.At(3, 2, 1) >= 1 || smooth.At(3, 2, 1) <= smooth.At(2, 2, 1) {
		t.Fatalf("smoothed mask peak %v next to %v", smooth.At(3, 2, 1), smooth.At(2, 2, 1))
	}
}

func benchVolume(n int) *imagefilter.Volume {
	v := imagefilter.NewVolume([3]int{n, n, n}, imagefilter.IsotropicSigma(1))
	rng := rand.New(rand.NewSource(1))
	for i := range v.Data {
		v.Data[i] = float32(rng.NormFloat64()*50 + 40)
	}
	return v
}

// BenchmarkRecursiveGaussian256 filters a 256³ volume at 1 mm spacing with
// σ = 1 and 8 mm: the recursive filters cost the same for both, while the FIR
// render.SmoothGaussian3D grows with its 6σ+1-tap kernel.
func BenchmarkRecursiveGaussian256(b *testing.B) {
	src := benchVolume(256)
	ctx := context.Background()
	for _, sigma := range []float64{1, 8} {
		opts := imagefilter.RecursiveGaussianOptions{SigmaMM: imagefilter.IsotropicSigma(sigma)}
		b.Run(fmt.Sprintf("smooth/sigma=%gmm", sigma), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := imagefilter.SmoothRecursiveGaussian(ctx, src, opts); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("gradient-magnitude/sigma=%gmm", sigma), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := imagefilter.GradientMagnitudeRecursiveGaussian(ctx, src, opts); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("hessian/sigma=%gmm", sigma), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := imagefilter.HessianRecursiveGaussian(ctx, src, opts); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("render-fir/sigma=%gmm", sigma), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := render.SmoothGaussian3D(ctx, src.Data, src.Dims, src.SpacingMM, sigma); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
