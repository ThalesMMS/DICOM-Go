package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
)

func TestRegionalLocalKernelsWholeBits(t *testing.T) {
	for _, dims := range [][3]int{{7, 5, 4}, {1, 3, 1}, {3, 1, 2}} {
		spacing := [3]float64{.7, 1.3, 2.1}
		v := NewVolume(dims, spacing)
		for z := 0; z < dims[2]; z++ {
			for y := 0; y < dims[1]; y++ {
				for x := 0; x < dims[0]; x++ {
					v.Data[v.Index(x, y, z)] = float32((x*17+y*13+z*11)%19) - 9.5
				}
			}
		}
		v.Data[v.Index(dims[0]/2, dims[1]/2, dims[2]/2)] = 61
		v.Data[0] = 50
		v.Data[len(v.Data)-1] = -50
		for _, workers := range []int{1, 2} {
			filters := []SlabFilter{MedianOptions{RadiusMM: [3]float64{.7, 1.3, 2.1}, Workers: workers}, BilateralOptions{SigmaMM: [3]float64{.3, .6, .9}, RangeSigma: 8, Workers: workers}, NonLocalMeansOptions{NoiseSigma: 3, PatchRadiusMM: [3]float64{.7, 0, 0}, SearchRadiusMM: [3]float64{.7, 1.3, 2.1}, BlockStep: 2, Workers: workers}}
			filters = append(filters, MedianOptions{RadiusMM: [3]float64{.35, 2.6, 0}, Workers: workers}, BilateralOptions{SigmaMM: [3]float64{0, .6, 1.3}, RangeSigma: 5, Workers: workers}, NonLocalMeansOptions{NoiseSigma: 3, PatchRadiusMM: [3]float64{.7, 1.3, 2.1}, SearchRadiusMM: [3]float64{.7, 1.3, 2.1}, BlockStep: 3, Workers: workers}, NonLocalMeansOptions{NoiseSigma: 3, PatchRadiusMM: [3]float64{.7, 2.6, 0}, SearchRadiusMM: [3]float64{1.4, 1.3, 2.1}, BlockStep: 1, Workers: workers})
			for _, f := range filters {
				var reference *Volume
				var err error
				switch o := f.(type) {
				case MedianOptions:
					reference, err = Median(context.Background(), v, o)
				case BilateralOptions:
					reference, err = Bilateral(context.Background(), v, o)
				case NonLocalMeansOptions:
					reference, err = NonLocalMeans(context.Background(), v, o)
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, tile := range [][3]int{{1, 1, 1}, {3, 2, 2}, {7, 5, 4}} {
					for _, roi := range []Region{{Size: dims}, regionalInner(dims), {Origin: [3]int{dims[0] / 2, dims[1] / 2, dims[2] / 2}, Size: [3]int{1, 1, 1}}} {
						t.Run(fmt.Sprintf("%v/%T/w%d/t%v/r%v", dims, f, workers, tile, roi), func(t *testing.T) {
							out := NewVolume(dims, spacing)
							for i := range out.Data {
								out.Data[i] = 12345
							}
							res, e := FilterRegions(context.Background(), f, v, dims, spacing, roi, out, RegionalOptions{TileSize: tile})
							if e != nil {
								t.Fatal(e)
							}
							if res.Tiles == 0 || res.ReadBytes > res.PlannedReadBytes {
								t.Fatalf("result %+v", res)
							}
							for z := 0; z < dims[2]; z++ {
								for y := 0; y < dims[1]; y++ {
									for x := 0; x < dims[0]; x++ {
										i := v.Index(x, y, z)
										want := float32(12345)
										if regionalContains(roi, x, y, z) {
											want = reference.Data[i]
										}
										if math.Float32bits(out.Data[i]) != math.Float32bits(want) {
											t.Fatalf("%d,%d,%d bits got%08x want%08x", x, y, z, math.Float32bits(out.Data[i]), math.Float32bits(want))
										}
									}
								}
							}
						})
					}
				}
			}
		}
	}
}
func regionalInner(d [3]int) Region {
	r := Region{Size: d}
	for a, n := range d {
		if n > 2 {
			r.Origin[a] = 1
			r.Size[a] = n - 2
		}
	}
	return r
}
func regionalContains(r Region, x, y, z int) bool {
	for a, p := range [3]int{x, y, z} {
		if p < r.Origin[a] || p >= r.Origin[a]+r.Size[a] {
			return false
		}
	}
	return true
}

type regionalSpy struct {
	v                   *Volume
	reads, writes       int
	failRead, failWrite int
	cancel              context.CancelFunc
	afterRead           bool
	cause               error
}

func (s *regionalSpy) ReadRegion(ctx context.Context, r Region, d []float32) error {
	s.reads++
	if s.reads == s.failRead {
		return s.cause
	}
	e := s.v.ReadRegion(ctx, r, d)
	if s.afterRead && s.cancel != nil {
		s.cancel()
	}
	return e
}
func (s *regionalSpy) WriteRegion(ctx context.Context, r Region, d []float32) error {
	s.writes++
	if s.writes == s.failWrite {
		return s.cause
	}
	return s.v.WriteRegion(ctx, r, d)
}
func TestRegionalBudgetRejectsBeforeIO(t *testing.T) {
	v := NewVolume([3]int{9, 7, 3}, [3]float64{1, 1, 1})
	s := &regionalSpy{v: v}
	filters := []SlabFilter{MedianOptions{RadiusMM: [3]float64{1, 1, 1}}, BilateralOptions{SigmaMM: [3]float64{1, 1, 1}, RangeSigma: 2}, NonLocalMeansOptions{NoiseSigma: 2}}
	for _, f := range filters {
		_, err := FilterRegions(context.Background(), f, s, v.Dims, v.SpacingMM, Region{Size: v.Dims}, s, RegionalOptions{MaxWorkingBytes: 1})
		var budget *RegionalBudgetError
		if !errors.As(err, &budget) || budget.Required <= 1 {
			t.Fatalf("%T %v", f, err)
		}
		if s.reads+s.writes != 0 {
			t.Fatal("IO before budget rejection")
		}
	}
	for _, f := range []SlabFilter{BilateralOptions{SigmaMM: [3]float64{.6, 0, 0}, RangeSigma: 1}, NonLocalMeansOptions{NoiseSigma: 1, PatchRadiusMM: [3]float64{2, 0, 0}, SearchRadiusMM: [3]float64{1, 0, 0}}} {
		_, e := FilterRegions(context.Background(), f, s, [3]int{math.MaxInt, 1, 1}, [3]float64{1, 1, 1}, Region{Origin: [3]int{math.MaxInt - 1, 0, 0}, Size: [3]int{1, 1, 1}}, s, RegionalOptions{})
		if !errors.Is(e, ErrInvalidVolume) || s.reads+s.writes != 0 {
			t.Fatalf("global halo arithmetic must reject beforeIO: %v read%d write%d", e, s.reads, s.writes)
		}
	}
	for _, d := range [][3]int{{math.MaxInt, 1, 1}, {math.MaxInt, 2, 1}} {
		if _, e := PlanRegions(MedianOptions{}, d, [3]float64{1, 1, 1}, Region{Size: d}, RegionalOptions{}); e == nil {
			t.Fatal("overflow admitted")
		}
	}
}
func TestRegionalCancelAndIOCommitFence(t *testing.T) {
	v := NewVolume([3]int{5, 4, 2}, [3]float64{1, 1, 1})
	f := BilateralOptions{RangeSigma: 1}
	cause := errors.New("regional IO sentinel")
	for _, mode := range []string{"cancel-before", "cancel-after-read", "read", "write"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &regionalSpy{v: v, cause: cause, cancel: cancel}
			switch mode {
			case "cancel-before":
				cancel()
			case "cancel-after-read":
				s.afterRead = true
			case "read":
				s.failRead = 2
			case "write":
				s.failWrite = 2
			}
			res, e := FilterRegions(ctx, f, s, v.Dims, v.SpacingMM, Region{Size: v.Dims}, s, RegionalOptions{TileSize: [3]int{2, 2, 1}})
			if e == nil {
				t.Fatal("accepted failure")
			}
			if mode == "read" || mode == "write" {
				if !errors.Is(e, cause) || res.Tiles != 1 {
					t.Fatalf("result %+v err%v", res, e)
				}
			} else if !errors.Is(e, context.Canceled) || s.writes != 0 || res.Tiles != 0 {
				t.Fatalf("cancel published %+v writes%d", res, s.writes)
			}
		})
	}
}
func TestRegionalMedianGlobalClassificationAndIsolation(t *testing.T) {
	dims := [3]int{5, 3, 1}
	roi := Region{Origin: [3]int{2, 1, 0}, Size: [3]int{1, 1, 1}}
	for _, outside := range []float32{1, .25} {
		v := NewVolume(dims, [3]float64{1, 1, 0})
		for i := range v.Data {
			v.Data[i] = float32(math.Copysign(0, -1))
		}
		v.Data[0] = outside
		f := MedianOptions{RadiusMM: [3]float64{1, 1, 0}}
		want, e := Median(context.Background(), v, f)
		if e != nil {
			t.Fatal(e)
		}
		out := NewVolume(dims, v.SpacingMM)
		res, e := FilterRegions(context.Background(), f, v, dims, v.SpacingMM, roi, out, RegionalOptions{TileSize: [3]int{1, 1, 1}})
		if e != nil {
			t.Fatal(e)
		}
		i := v.Index(2, 1, 0)
		if math.Float32bits(out.Data[i]) != math.Float32bits(want.Data[i]) {
			t.Fatal("signed-zero backend mismatch")
		}
		if res.ReadBytes < int64(len(v.Data))*4 {
			t.Fatal("missing complete classification")
		}
	}
	for _, value := range []float32{7, 13} {
		v := NewVolume(dims, [3]float64{1, 1, 0})
		for i := range v.Data {
			v.Data[i] = value
		}
		out := NewVolume(dims, v.SpacingMM)
		if _, e := FilterRegions(context.Background(), BilateralOptions{RangeSigma: 1}, v, dims, v.SpacingMM, roi, out, RegionalOptions{}); e != nil || out.At(2, 1, 0) != value {
			t.Fatalf("request source reused %v", e)
		}
	}
}

func TestRegionalConcurrentSources(t *testing.T) {
	var wg sync.WaitGroup
	for job := 0; job < 8; job++ {
		wg.Add(1)
		go func(value float32) {
			defer wg.Done()
			dims := [3]int{5, 4, 2}
			v := NewVolume(dims, [3]float64{1, 1, 1})
			for i := range v.Data {
				v.Data[i] = value
			}
			out := NewVolume(dims, v.SpacingMM)
			_, err := FilterRegions(context.Background(), NonLocalMeansOptions{NoiseSigma: 2, Workers: 2}, v, dims, v.SpacingMM, Region{Size: dims}, out, RegionalOptions{TileSize: [3]int{2, 2, 1}})
			if err != nil {
				t.Error(err)
				return
			}
			for _, got := range out.Data {
				if got != value {
					t.Errorf("source isolation got%g want%g", got, value)
					return
				}
			}
		}(float32(job))
	}
	wg.Wait()
}
