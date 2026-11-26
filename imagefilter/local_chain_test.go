package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
)

func chainFixture(dims [3]int) *Volume {
	v := NewVolume(dims, [3]float64{.7, 1.3, 2.1})
	for i := range v.Data {
		v.Data[i] = float32((i*17+i/dims[0]*11)%31) - 15.5
	}
	v.Data[0], v.Data[len(v.Data)-1] = 200, -200
	return v
}
func chainSource(v *Volume, generation uint64) LocalChainSource {
	return LocalChainSource{LocalChainIdentity{SourceID: [32]byte{1}, GeometryID: [32]byte{2}, Generation: generation, Dims: v.Dims, SpacingMM: v.SpacingMM}, v}
}
func chainFilters(workers int) []SlabFilter {
	return []SlabFilter{
		BilateralOptions{SigmaMM: [3]float64{.3, .6, .9}, RangeSigma: 10, Workers: workers},
		NonLocalMeansOptions{NoiseSigma: 5, PatchRadiusMM: [3]float64{.7, 0, 0}, SearchRadiusMM: [3]float64{.7, 1.3, 0}, BlockStep: 2, Workers: workers},
	}
}
func chainDense(ctx context.Context, v *Volume, filters []SlabFilter) (*Volume, error) {
	var err error
	for _, f := range filters {
		switch o := f.(type) {
		case BilateralOptions:
			v, err = Bilateral(ctx, v, o)
		case NonLocalMeansOptions:
			v, err = NonLocalMeans(ctx, v, o)
		}
		if err != nil {
			return nil, err
		}
	}
	return v, nil
}
func chainCompare(t *testing.T, result *LocalChainResult, want *Volume) {
	t.Helper()
	n, _ := voxelCount(result.Region.Size)
	data := make([]float32, n)
	if err := want.ReadRegion(context.Background(), result.Region, data); err != nil {
		t.Fatal(err)
	}
	for i, v := range result.Data {
		if math.Float32bits(v) != math.Float32bits(data[i]) {
			t.Fatalf("region %v sample %d bits got%08x want%08x", result.Region, i, math.Float32bits(v), math.Float32bits(data[i]))
		}
	}
}

func TestLocalChainDenseBitsHalosAndPartitions(t *testing.T) {
	for _, dims := range [][3]int{{9, 7, 5}, {7, 5, 1}, {1, 4, 3}} {
		v := chainFixture(dims)
		before := v.Clone()
		for _, workers := range []int{1, 2} {
			base := chainFilters(workers)
			nlm3 := NonLocalMeansOptions{NoiseSigma: 5, PatchRadiusMM: [3]float64{.7, 1.3, 2.1}, SearchRadiusMM: [3]float64{.7, 1.3, 2.1}, BlockStep: 3, Workers: workers}
			for ci, filters := range [][]SlabFilter{base, {nlm3, base[0]}, {base[0], nlm3, BilateralOptions{RangeSigma: 7, Workers: workers}}} {
				want, err := chainDense(context.Background(), v, filters)
				if err != nil {
					t.Fatal(err)
				}
				for _, tile := range [][3]int{{1, 1, 1}, {3, 2, 2}, {}} {
					c, err := NewLocalChain(chainSource(v, 1), filters, LocalChainOptions{TileSize: tile})
					if err != nil {
						t.Fatal(err)
					}
					for _, requested := range []Region{{Size: dims}, regionalInner(dims), {Origin: [3]int{dims[0] - 1, dims[1] - 1, dims[2] - 1}, Size: [3]int{1, 1, 1}}} {
						t.Run(fmt.Sprintf("d%v/c%d/w%d/t%v/r%v", dims, ci, workers, tile, requested), func(t *testing.T) {
							r, err := c.Run(context.Background(), requested)
							if err != nil {
								t.Fatal(err)
							}
							chainCompare(t, r, want)
							if r.Plan.WorkingBytes > DefaultSlabWorkingBytes || r.Stats.SourceReadBytes != r.Plan.SourceReadBytes {
								t.Fatal("plan/admission accounting mismatch")
							}
							region := requested
							for i := len(filters) - 1; i >= 0; i-- {
								k, _ := regionalParams(filters[i], dims, v.SpacingMM)
								region = regionalWindow(region, dims, k.radius)
								if r.Plan.Stage[i].Input != region {
									t.Fatal("transitive halo wrong")
								}
							}
						})
					}
					c.Close()
				}
			}
		}
		if !reflect.DeepEqual(v, before) {
			t.Fatal("chain modified source")
		}
	}
}

func TestLocalChainCachePrefixInvalidationAndOwnership(t *testing.T) {
	v := chainFixture([3]int{21, 17, 5})
	filters := chainFilters(2)
	first := filters[0].(BilateralOptions)
	c, err := NewLocalChain(chainSource(v, 1), []SlabFilter{&first, filters[1]}, LocalChainOptions{CacheBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first.RangeSigma = 999 // Constructor holds the original value snapshot.
	roi := Region{Origin: [3]int{7, 5, 1}, Size: [3]int{4, 4, 2}}
	r, err := c.Run(context.Background(), roi)
	if err != nil || r.Stats.CacheHitStage != -1 || !r.Stats.Computed[0] || !r.Stats.Computed[1] {
		t.Fatal(r, err)
	}
	want, _ := chainDense(context.Background(), v, filters)
	chainCompare(t, r, want)
	r.Data[0] = 999
	warm, err := c.Run(context.Background(), roi)
	if err != nil || warm.Stats.CacheHitStage != 1 || warm.Stats.SourceReadBytes != 0 || warm.Stats.Computed != ([3]bool{}) {
		t.Fatal(warm, err)
	}
	chainCompare(t, warm, want)
	tail := filters[1].(NonLocalMeansOptions)
	tail.NoiseSigma = 6
	if err = c.SetStage(1, &tail); err != nil {
		t.Fatal(err)
	}
	tail.NoiseSigma = 999
	filters[1] = NonLocalMeansOptions{NoiseSigma: 6, PatchRadiusMM: [3]float64{.7, 0, 0}, SearchRadiusMM: [3]float64{.7, 1.3, 0}, BlockStep: 2, Workers: 2}
	r, err = c.Run(context.Background(), roi)
	if err != nil || r.Stats.CacheHitStage != 0 || r.Stats.Computed[0] || !r.Stats.Computed[1] || r.Stats.SourceReadBytes != 0 {
		t.Fatal("tail edit invalidated prefix", r, err)
	}
	want, _ = chainDense(context.Background(), v, filters)
	chainCompare(t, r, want)
	first = filters[0].(BilateralOptions)
	first.RangeSigma = 8
	filters[0] = first
	if err = c.SetStage(0, first); err != nil {
		t.Fatal(err)
	}
	r, err = c.Run(context.Background(), roi)
	if err != nil || r.Stats.CacheHitStage != -1 || !r.Stats.Computed[0] || !r.Stats.Computed[1] {
		t.Fatal("first edit did not invalidate descendants", r, err)
	}
	for _, change := range []string{"generation", "geometry", "source", "spacing", "dimensions"} {
		t.Run(change, func(t *testing.T) {
			u := v.Clone()
			binding := chainSource(u, 1)
			switch change {
			case "generation":
				binding.Identity.Generation = 2
			case "geometry":
				binding.Identity.GeometryID[0]++
			case "source":
				binding.Identity.SourceID[0]++
				u.Data[0]++
			case "spacing":
				u.SpacingMM[0] = 1.4
				binding.Identity.SpacingMM = u.SpacingMM
			case "dimensions":
				u = chainFixture([3]int{23, 17, 5})
				binding = chainSource(u, 1)
			}
			if err := c.ReplaceSource(binding); err != nil || c.CacheBytes() != 0 {
				t.Fatal("source rebind retained cache", err)
			}
			r, err := c.Run(context.Background(), roi)
			if err != nil || r.Stats.CacheHitStage != -1 || r.Plan.Identity != binding.Identity {
				t.Fatal(r, err)
			}
			want, err := chainDense(context.Background(), u, filters)
			if err != nil {
				t.Fatal(err)
			}
			chainCompare(t, r, want)
		})
	}
}

func TestLocalChainBudgetBeforeIOAndUnsupportedGlobalFilters(t *testing.T) {
	v := chainFixture([3]int{21, 17, 5})
	spy := &regionalSpy{v: v}
	binding := chainSource(v, 1)
	binding.Reader = spy
	filters := chainFilters(1)
	c, err := NewLocalChain(binding, filters, LocalChainOptions{CacheBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	requested := Region{Origin: [3]int{5, 4, 1}, Size: [3]int{7, 6, 3}}
	p, err := c.Plan(requested)
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []LocalChainOptions{
		{MaxWorkingBytes: p.WorkingBytes - 1, CacheBytes: 1024},
		{MaxReadBytes: p.SourceReadBytes - 1, CacheBytes: 1024},
	} {
		limited, err := NewLocalChain(binding, filters, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = limited.Run(context.Background(), requested); !errors.Is(err, ErrLocalChainBudget) || spy.reads != 0 {
			t.Fatal("budget allocated/read first", err, spy.reads)
		}
		limited.Close()
	}
	exact, err := NewLocalChain(binding, filters, LocalChainOptions{MaxWorkingBytes: p.WorkingBytes, CacheBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = exact.Run(context.Background(), requested); err != nil {
		t.Fatal("exact reservation did not fit", err)
	}
	for _, bad := range []SlabFilter{MedianOptions{}, UnsharpMaskOptions{}, nil} {
		if _, err = NewLocalChain(binding, []SlabFilter{filters[0], bad}, LocalChainOptions{}); !errors.Is(err, ErrInvalidVolume) {
			t.Fatal("global/unsupported filter admitted", err)
		}
	}
	for _, opts := range []LocalChainOptions{{MaxWorkingBytes: -1}, {CacheBytes: -1}, {MaxCacheEntries: 129}, {TileSize: [3]int{-1, 0, 0}}, {CacheBytes: 1024, MaxWorkingBytes: 1024}} {
		if _, err = NewLocalChain(binding, filters, opts); err == nil {
			t.Fatal("invalid bounds accepted")
		}
	}
	c.Close()
	exact.Close()
}

func TestLocalChainEvictionAndAtomicFailures(t *testing.T) {
	v := chainFixture([3]int{11, 9, 1})
	filters := []SlabFilter{BilateralOptions{RangeSigma: 3}, NonLocalMeansOptions{NoiseSigma: 2, PatchRadiusMM: [3]float64{.7, 0, 0}, SearchRadiusMM: [3]float64{.7, 0, 0}}}
	c, err := NewLocalChain(chainSource(v, 1), filters, LocalChainOptions{CacheBytes: 128, MaxCacheEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	roi := Region{Origin: [3]int{3, 2, 0}, Size: [3]int{3, 3, 1}}
	r, err := c.Run(context.Background(), roi)
	if err != nil || r.Stats.Evictions != 1 || c.CacheBytes() != 36 {
		t.Fatal(r, err, c.CacheBytes())
	}
	if r, err = c.Run(context.Background(), roi); err != nil || r.Stats.CacheHitStage != 1 {
		t.Fatal(r, err)
	}
	c.ClearCache()
	if c.CacheBytes() != 0 {
		t.Fatal("clear retained cache payload")
	}
	c.Close()
	cause := errors.New("chain source failure")
	for _, mode := range []string{"cancel-before", "cancel-after-read", "read", "nonfinite"} {
		ctx, cancel := context.WithCancel(context.Background())
		spy := &regionalSpy{v: v, cause: cause}
		switch mode {
		case "cancel-before":
			cancel()
		case "cancel-after-read":
			spy.cancel, spy.afterRead = cancel, true
		case "read":
			spy.failRead = 1
		case "nonfinite":
			spy.v = v.Clone()
			spy.v.Data[spy.v.Index(3, 2, 0)] = float32(math.NaN())
		}
		binding := chainSource(v, 1)
		binding.Reader = spy
		c, _ := NewLocalChain(binding, filters, LocalChainOptions{CacheBytes: 1024})
		r, err := c.Run(ctx, roi)
		cancel()
		if r != nil || err == nil || c.CacheBytes() != 0 {
			t.Fatalf("%s published partial output/cache: %v %v", mode, r, err)
		}
		c.Close()
	}
}

type chainBlockedSource struct {
	v       *Volume
	entered chan struct{}
	release chan struct{}
}

func (s *chainBlockedSource) ReadRegion(ctx context.Context, r Region, data []float32) error {
	close(s.entered)
	select {
	case <-s.release:
		return s.v.ReadRegion(ctx, r, data)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestLocalChainConcurrentSupersessionQueueAndClose(t *testing.T) {
	for _, mode := range []string{"stage", "source", "close", "queue"} {
		t.Run(mode, func(t *testing.T) {
			v := chainFixture([3]int{11, 9, 3})
			src := &chainBlockedSource{v, make(chan struct{}), make(chan struct{})}
			binding := chainSource(v, 1)
			binding.Reader = src
			c, _ := NewLocalChain(binding, chainFilters(2), LocalChainOptions{CacheBytes: 4096})
			defer c.Close()
			finished := make(chan error, 1)
			go func() {
				r, err := c.Run(context.Background(), Region{Size: v.Dims})
				if r != nil {
					err = errors.New("superseded request published output")
				}
				finished <- err
			}()
			<-src.entered
			switch mode {
			case "stage":
				if err := c.SetStage(1, NonLocalMeansOptions{NoiseSigma: 6}); err != nil {
					t.Fatal(err)
				}
			case "source":
				if err := c.ReplaceSource(chainSource(v.Clone(), 2)); err != nil {
					t.Fatal(err)
				}
			case "close":
				c.Close()
			case "queue":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if r, err := c.Run(ctx, Region{Size: v.Dims}); r != nil || !errors.Is(err, context.Canceled) {
					t.Fatal("queued cancellation failed", r, err)
				}
				c.Close()
			}
			err := <-finished
			if mode == "stage" || mode == "source" {
				if !errors.Is(err, ErrLocalChainSuperseded) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrLocalChainClosed) {
				t.Fatal(err)
			}
			if c.CacheBytes() != 0 {
				t.Fatal("superseded request inserted cache")
			}
		})
	}
	v := chainFixture([3]int{9, 7, 3})
	c, _ := NewLocalChain(chainSource(v, 1), chainFilters(2), LocalChainOptions{CacheBytes: 4096})
	defer c.Close()
	want, _ := chainDense(context.Background(), v, chainFilters(2))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.Run(context.Background(), Region{Size: v.Dims})
			if err != nil {
				t.Error(err)
				return
			}
			chainCompare(t, r, want)
		}()
	}
	wg.Wait()
}

func TestLocalChainCloseReleasesBinding(t *testing.T) {
	v := chainFixture([3]int{9, 7, 3})
	c, err := NewLocalChain(chainSource(v, 1), chainFilters(2), LocalChainOptions{CacheBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Run(context.Background(), Region{Size: v.Dims})
	if err != nil || c.CacheBytes() == 0 {
		t.Fatal(r, err)
	}
	want, _ := chainDense(context.Background(), v, chainFilters(2))
	c.Close()
	c.Close()
	if c.source.Reader != nil || c.stages != ([3]localChainStage{}) || c.cache != nil || c.active != nil || c.CacheBytes() != 0 {
		t.Fatal("live closed handle retains a source, stage, cache or active request")
	}
	chainCompare(t, r, want)
	if p, err := c.Plan(Region{Size: v.Dims}); !errors.Is(err, ErrLocalChainClosed) || p.Stages != 0 {
		t.Fatal(p, err)
	}
	if r, err := c.Run(context.Background(), Region{Size: v.Dims}); r != nil || !errors.Is(err, ErrLocalChainClosed) {
		t.Fatal(r, err)
	}
}

func TestLocalChainEquivalentDefaultsPreserveActiveAndCache(t *testing.T) {
	v := chainFixture([3]int{9, 7, 3})
	src := &chainBlockedSource{v, make(chan struct{}), make(chan struct{})}
	binding := chainSource(v, 1)
	binding.Reader = src
	filters := chainFilters(2)
	nlm := filters[1].(NonLocalMeansOptions)
	nlm.BlockStep = 0
	filters[1] = nlm
	c, err := NewLocalChain(binding, filters, LocalChainOptions{CacheBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	finished := make(chan error, 1)
	go func() {
		_, err := c.Run(context.Background(), Region{Size: v.Dims})
		finished <- err
	}()
	<-src.entered
	bilateral := filters[0].(BilateralOptions)
	bilateral.MaxKernelSamples = 1 << 16
	nlm.Beta, nlm.BlockStep, nlm.MaxSearchSamples = 1, 2, 1<<16
	if err := c.SetStage(0, bilateral); err != nil {
		t.Fatal(err)
	}
	if err := c.SetStage(1, nlm); err != nil {
		t.Fatal(err)
	}
	close(src.release)
	if err := <-finished; err != nil {
		t.Fatalf("equivalent explicit defaults superseded active request: %v", err)
	}
	if err := c.SetStage(1, filters[1]); err != nil {
		t.Fatal(err)
	}
	r, err := c.Run(context.Background(), Region{Size: v.Dims})
	if err != nil || r.Stats.CacheHitStage != 1 || r.Stats.SourceReadBytes != 0 {
		t.Fatal("equivalent defaults invalidated final cache", r, err)
	}
}

func TestLocalChainRadiusRepresentationInvalidatesSnapshots(t *testing.T) {
	v := chainFixture([3]int{9, 7, 3})
	filters := []SlabFilter{chainFilters(2)[0], NonLocalMeansOptions{NoiseSigma: 5, Workers: 2}}
	explicit := filters[1].(NonLocalMeansOptions)
	explicit.PatchRadiusMM = v.SpacingMM
	for a, spacing := range v.SpacingMM {
		explicit.SearchRadiusMM[a] = 5 * spacing
	}
	defaultDense, err := chainDense(context.Background(), v, filters)
	if err != nil {
		t.Fatal(err)
	}
	explicitDense, err := chainDense(context.Background(), v, []SlabFilter{filters[0], explicit})
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range defaultDense.Data {
		if math.Float32bits(value) != math.Float32bits(explicitDense.Data[i]) {
			t.Fatal("fixture did not produce equivalent default/explicit radius kernels")
		}
	}
	// Parameter snapshots are explicit, rather than a general semantic kernel
	// identity. Equal physical kernels with different radius representations
	// intentionally cancel active work and invalidate only the edited stage.
	src := &chainBlockedSource{v, make(chan struct{}), make(chan struct{})}
	binding := chainSource(v, 1)
	binding.Reader = src
	c, err := NewLocalChain(binding, filters, LocalChainOptions{CacheBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		r, err := c.Run(context.Background(), Region{Size: v.Dims})
		if r != nil {
			err = errors.New("changed radius snapshot published active output")
		}
		finished <- err
	}()
	<-src.entered
	if err := c.SetStage(1, explicit); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, ErrLocalChainSuperseded) || c.CacheBytes() != 0 {
		t.Fatal("radius representation change did not supersede active work", err)
	}
	c.Close()
	c, err = NewLocalChain(chainSource(v, 1), filters, LocalChainOptions{CacheBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Run(context.Background(), Region{Size: v.Dims}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetStage(1, explicit); err != nil {
		t.Fatal(err)
	}
	r, err := c.Run(context.Background(), Region{Size: v.Dims})
	if err != nil || r.Stats.CacheHitStage != 0 || r.Stats.SourceReadBytes != 0 || r.Stats.Computed != ([3]bool{false, true, false}) {
		t.Fatal("radius representation edit did not retain exact upstream region", r, err)
	}
	chainCompare(t, r, explicitDense)
}
