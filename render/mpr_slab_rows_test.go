package render

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// The reference keeps the original pixel-outer/sample-inner traversal and
// #1365's planner/reducer; the candidate changes only composition traversal.
func slabRowsOrthogonalReference(ctx context.Context, sampler *volumeSampler, axis MPRPlane,
	width, height, start, end, samples int, mode SlabMode, interpolation Interpolation, trapezoid bool,
) *ScalarPlane {
	p, _ := newScalarPlane(width, height, samples, sampler.photometric, defaultScalarVOI(), MPRLimits{})
	for y := 0; y < height; y++ {
		z := orthogonalDisplaySliceIndex(y, height, sampler.depth)
		for x := 0; x < width; x++ {
			value, ok := reduceSampledScalarContext(ctx, mode, start, end, samples, trapezoid, func(coordinate float64) (float64, bool) {
				position := Vec3{X: float64(x), Y: float64(y), Z: coordinate}
				if axis == MPRPlaneCoronal {
					position.Y, position.Z = coordinate, float64(z)
				} else if axis == MPRPlaneSagittal {
					position.X, position.Y, position.Z = coordinate, float64(x), float64(z)
				}
				if coordinate == math.Trunc(coordinate) {
					return sampler.valueAt(int(position.X), int(position.Y), int(position.Z))
				}
				return sampler.sampleAt(position, interpolation)
			})
			p.set(x, y, value, ok)
		}
	}
	return p
}

func assertSlabRowsExact(t *testing.T, got, want *ScalarPlane) {
	t.Helper()
	if got == nil || want == nil || got.width != want.width || got.height != want.height {
		t.Fatalf("scalar dimensions differ: %v/%v", got, want)
	}
	for i := range want.values {
		if got.valid[i] != want.valid[i] || math.Float64bits(got.values[i]) != math.Float64bits(want.values[i]) {
			t.Fatalf("scalar[%d] = %x/%d, want %x/%d", i, math.Float64bits(got.values[i]), got.valid[i], math.Float64bits(want.values[i]), want.valid[i])
		}
	}
}

func TestSlabRowsCachedAxesPreservePerPixelBits(t *testing.T) {
	vol, err := BuildVolume(ctPhantomStack(13, 11, 9))
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		t.Fatal("no sampler")
	}
	defer sampler.Close()
	for _, axis := range []MPRPlane{MPRPlaneAxial, MPRPlaneCoronal, MPRPlaneSagittal} {
		for _, interpolation := range []Interpolation{InterpolationNearest, InterpolationLinear, InterpolationCubic} {
			for _, thickness := range []int{0, 1, 2, 9} {
				for _, fraction := range []float64{0, .5, .3} {
					for _, trapezoid := range []bool{false, true} {
						for _, mode := range []SlabMode{SlabMIP, SlabMinIP, SlabAverage, SlabSum} {
							name := fmt.Sprintf("%s/i%d/t%d/f%g/trap%v/m%d", axis, interpolation, thickness, fraction, trapezoid, mode)
							t.Run(name, func(t *testing.T) {
								options := MPRRenderOptions{Interpolation: interpolation, Slab: SlabSampling{SpacingFraction: fraction, Trapezoid: trapezoid}}
								got, rendered, err := renderCachedScalarSlabContext(context.Background(), vol, sampler, axis, 1, thickness, mode, false, defaultScalarVOI(), MPRLimits{}, options)
								if err != nil || !rendered {
									t.Fatalf("cached slab=%v/%v", rendered, err)
								}
								axisCount := sampler.depth
								if axis == MPRPlaneCoronal {
									axisCount = sampler.rows
								} else if axis == MPRPlaneSagittal {
									axisCount = sampler.cols
								}
								start, end := scalarProjectionRange(1, thickness, axisCount, mode)
								samples, _ := options.Slab.SampleCount(end-start+1, MPRLimits{})
								want := slabRowsOrthogonalReference(context.Background(), sampler, axis, got.width, got.height, start, end, samples, mode, interpolation, trapezoid)
								assertSlabRowsExact(t, got, want)
							})
						}
					}
				}
			}
		}
	}
}

func TestSlabRowsObliquePreservesRaysBoundsAndFallbacks(t *testing.T) {
	for name, vol := range cubicTestVolumes(t) {
		defer vol.Close()
		sampler, ok := newVolumeSampler(vol)
		if !ok {
			t.Fatal("no sampler")
		}
		defer sampler.Close()
		for planeName, plane := range cubicPlanesFor(vol) {
			for _, interpolation := range []Interpolation{InterpolationNearest, InterpolationLinear, InterpolationCubic} {
				for _, samples := range []int{1, 3, 5, 17} {
					for _, mode := range []SlabMode{SlabMIP, SlabMinIP, SlabAverage, SlabSum} {
						for _, trapezoid := range []bool{false, true} {
							t.Run(fmt.Sprintf("%s/%s/i%d/n%d/m%d/trap%v", name, planeName, interpolation, samples, mode, trapezoid), func(t *testing.T) {
								const width, height, step = 13, 11, .37
								first := -float64(samples-1) / 2
								normal := plane.U.Cross(plane.V).Normalize()
								got, err := resliceScalarSlabWithSamplerContext(context.Background(), sampler, plane, normal, width, height, first, samples, step, mode, defaultScalarVOI(), MPRLimits{}, interpolation, trapezoid)
								if err != nil {
									t.Fatal(err)
								}
								want, _ := newScalarPlane(width, height, samples, sampler.photometric, defaultScalarVOI(), MPRLimits{})
								for y := 0; y < height; y++ {
									for x := 0; x < width; x++ {
										ray := vol.voxelLine(plane.At(float64(x)/float64(width-1), float64(y)/float64(height-1)), normal.Normalize())
										sample := func(k int) (float64, bool) { return sampler.sampleAt(ray.at((first+float64(k))*step), interpolation) }
										var value float64
										var valid bool
										if trapezoid && (mode == SlabAverage || mode == SlabSum) {
											value, valid = reduceTrapezoid(mode, samples, sample)
										} else {
											value, valid = reduceCachedScalarContext(context.Background(), mode, 0, samples-1, sample)
										}
										want.set(x, y, value, valid)
									}
								}
								assertSlabRowsExact(t, got, want)
							})
						}
					}
				}
			}
		}
	}
}

func TestSlabRowsMissingSamplesSignedZerosAndNaNs(t *testing.T) {
	profiles := [][]float64{
		{math.Copysign(0, -1), 0, -3.5, 9, -11},
		{math.Float64frombits(0x7ff8000000000031), 9, -11},
		{1e20, 3, -1e20, 5},
		{math.Inf(1), math.Inf(-1), 9},
	}
	for _, profile := range profiles {
		for _, mode := range []SlabMode{SlabMIP, SlabMinIP, SlabAverage, SlabSum} {
			for _, trapezoid := range []bool{false, true} {
				for _, missing := range []int{-1, 0, len(profile) - 1} {
					p, _ := newScalarPlane(1, 1, len(profile), "", defaultScalarVOI(), MPRLimits{})
					row := newScalarSlabRow(1, len(profile), mode, trapezoid)
					row.bind(p, 0)
					for k, v := range profile {
						row.add(0, k, len(profile), v, k != missing)
					}
					row.finish(context.Background())
					sample := func(k int) (float64, bool) { return profile[k], k != missing }
					var want float64
					var valid bool
					if trapezoid && (mode == SlabAverage || mode == SlabSum) {
						want, valid = reduceTrapezoid(mode, len(profile), sample)
					} else {
						want, valid = reduceCachedScalarContext(context.Background(), mode, 0, len(profile)-1, sample)
					}
					if (p.valid[0] != 0) != valid || !sameBits(p.values[0], want) {
						t.Fatalf("profile%v mode%d trap%v missing%d = %x, want %x", profile, mode, trapezoid, missing, math.Float64bits(p.values[0]), math.Float64bits(want))
					}
				}
			}
		}
	}
}

func TestSlabRowsScratchAdmissionAndCancellation(t *testing.T) {
	vol, err := BuildVolume(gradientXZStack(32, 40, 18))
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		t.Fatal("no sampler")
	}
	plane := obliqueInteriorPlane(vol)
	normal := plane.U.Cross(plane.V).Normalize()
	if scalarSlabRowsEligible(sampler, normal, 4, SlabAverage, InterpolationLinear) ||
		!scalarSlabRowsEligible(sampler, normal, 5, SlabAverage, InterpolationLinear) {
		t.Fatal("Average origin+weight scratch must fit the admitted extra sample work")
	}
	sampler.Close()
	const width, height, samples = 9, 7, 5
	workingBytes := int64(width * height * (9 + 8*(samples-1)))
	for _, budget := range []int64{workingBytes - 1, workingBytes} {
		p, err := ResliceObliqueSlabScalarWithOptionsContext(context.Background(), vol, plane, width, height, samples, SlabAverage, MPRResliceOptions{Limits: MPRLimits{MaxWorkingBytes: budget}})
		if budget < workingBytes {
			if p != nil || !errors.Is(err, ErrMPRResourceLimit) {
				t.Fatalf("under-budget allocation=%v/%v", p, err)
			}
		} else if err != nil || p == nil {
			t.Fatalf("exact admitted budget=%v/%v", p, err)
		}
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := newCancellationBarrierContext(base, 128)
	done := make(chan error, 1)
	go func() {
		p, err := ResliceObliqueSlabScalarWithOptionsContext(ctx, vol, plane, 64, 48, 31, SlabAverage, MPRResliceOptions{Interpolation: InterpolationCubic, Slab: SlabSampling{Trapezoid: true}})
		if p != nil {
			err = errors.New("cancelled oblique slab published a partial plane")
		}
		done <- err
	}()
	select {
	case <-ctx.renderStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("oblique row work did not reach cancellation barrier")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled oblique row workers did not join")
	}
	if parallel.InUse() != 0 || vol.VolumeStoreStats().ActiveLeases != 0 {
		t.Fatalf("cancel retained helpers/leases: %d/%+v", parallel.InUse(), vol.VolumeStoreStats())
	}
}

func TestSlabRowsConcurrentRendersShareBudgetAndOwnOutputs(t *testing.T) {
	previous := parallel.SetBudget(1)
	defer parallel.SetBudget(previous)
	parallel.ResetPeak()
	stack := ctPhantomStack(40, 36, 31)
	vol, err := stack.Volume()
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()
	plane := obliqueInteriorPlane(vol)
	render := func(i int) (*ScalarPlane, error) {
		if i == 0 {
			return RenderScalarSlabWithOptionsContext(context.Background(), stack, MPRPlaneAxial, 15, 25, SlabAverage, MPRRenderOptions{Slab: SlabSampling{Trapezoid: true}})
		}
		return ResliceObliqueSlabScalarContext(context.Background(), vol, plane, 43, 37, 17, []SlabMode{SlabAverage, SlabMIP, SlabSum}[i])
	}
	wants := make([]*ScalarPlane, 3)
	for i := range wants {
		wants[i], err = render(i)
		if err != nil {
			t.Fatal(err)
		}
	}
	gots := make([]*ScalarPlane, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range gots {
		wg.Add(1)
		go func(i int) { defer wg.Done(); gots[i], errs[i] = render(i) }(i)
	}
	wg.Wait()
	for i := range gots {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		assertSlabRowsExact(t, gots[i], wants[i])
		gots[i].values[0]++
		if sameBits(gots[i].values[0], wants[i].values[0]) {
			t.Fatal("output ownership unexpectedly shared")
		}
	}
	if parallel.Peak() > 1 || parallel.InUse() != 0 || vol.VolumeStoreStats().ActiveLeases != 0 {
		t.Fatalf("concurrent renders exceeded/leaked budget or leases: peak%d active%d leases%+v", parallel.Peak(), parallel.InUse(), vol.VolumeStoreStats())
	}
}
