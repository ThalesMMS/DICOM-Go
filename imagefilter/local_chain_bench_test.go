package imagefilter

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"testing"
)

// Preregistered campaign: see docs/LOCAL_FILTER_CHAIN.md. Each process uses
// the same immutable fixture and existing dense kernels as the bit oracle.
var chainCampaignPhases = []string{"materialized", "cold_roi", "warm_roi", "tail_edit_roi", "whole_demand", "materialized_whole"}

type chainCampaign struct {
	volume *Volume
	chain  *LocalChain
	phase  string
	region Region
	tail   NonLocalMeansOptions
}

func newChainCampaign(phase string) (*chainCampaign, error) {
	v := chainFixture([3]int{128, 64, 8})
	v.Data[v.Index(54, 26, 3)] = 180
	c, err := NewLocalChain(chainSource(v, 1), chainFilters(2), LocalChainOptions{
		MaxWorkingBytes: 8 << 20, CacheBytes: 64 << 10, MaxCacheEntries: 8, TileSize: [3]int{16, 12, 2},
	})
	if err != nil {
		return nil, err
	}
	r := &chainCampaign{volume: v, chain: c, phase: phase,
		region: Region{Origin: [3]int{53, 25, 3}, Size: [3]int{16, 12, 2}}, tail: chainFilters(2)[1].(NonLocalMeansOptions)}
	if phase == "whole_demand" || phase == "materialized_whole" {
		r.region = Region{Size: v.Dims}
	}
	if phase == "warm_roi" || phase == "tail_edit_roi" {
		if _, err := c.Run(context.Background(), r.region); err != nil {
			c.Close()
			return nil, err
		}
	}
	return r, nil
}

func (r *chainCampaign) run() (*LocalChainResult, error) {
	ctx := context.Background()
	switch r.phase {
	case "materialized", "materialized_whole":
		// Read complete source, run both dense public APIs, then publish only an
		// owned crop. Fixture, reader binding and session creation are external
		// to all phase operations; every input/output array here is charged.
		in := NewVolume(r.volume.Dims, r.volume.SpacingMM)
		if err := r.volume.ReadRegion(ctx, Region{Size: in.Dims}, in.Data); err != nil {
			return nil, err
		}
		out, err := chainDense(ctx, in, chainFilters(2))
		if err != nil {
			return nil, err
		}
		n, _ := voxelCount(r.region.Size)
		data := make([]float32, n)
		if err := out.ReadRegion(ctx, r.region, data); err != nil {
			return nil, err
		}
		return &LocalChainResult{Region: r.region, Data: data,
			Stats: LocalChainStats{CacheHitStage: -1, SourceReadBytes: int64(len(in.Data)) * 4}}, nil
	case "cold_roi", "whole_demand":
		r.chain.ClearCache()
	case "warm_roi":
	case "tail_edit_roi":
		if r.tail.NoiseSigma == 5 {
			r.tail.NoiseSigma = 6
		} else {
			r.tail.NoiseSigma = 5
		}
		if err := r.chain.SetStage(1, r.tail); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown chain campaign phase")
	}
	return r.chain.Run(ctx, r.region)
}

func BenchmarkLocalChainCampaign(b *testing.B) {
	for _, phase := range chainCampaignPhases {
		b.Run(phase, func(b *testing.B) {
			r, err := newChainCampaign(phase)
			if err != nil {
				b.Fatal(err)
			}
			defer r.chain.Close()
			out, err := r.run()
			if err != nil {
				b.Fatal(err)
			}
			filters := chainFilters(2)
			if phase == "tail_edit_roi" {
				filters[1] = r.tail
			}
			want, err := chainDense(context.Background(), r.volume, filters)
			if err != nil {
				b.Fatal(err)
			}
			n, _ := voxelCount(r.region.Size)
			oracle := make([]float32, n)
			if err := want.ReadRegion(context.Background(), r.region, oracle); err != nil {
				b.Fatal(err)
			}
			for i, x := range out.Data {
				if math.Float32bits(x) != math.Float32bits(oracle[i]) {
					b.Fatalf("dense preflight mismatch %d", i)
				}
			}
			b.Logf("dense bits pass phase=%s region=%v; logical reservation excludes caller fixture/results retained after return and Go runtime", phase, r.region)
			b.ReportAllocs()
			b.ResetTimer()
			var reads, internal, retained int64
			for i := 0; i < b.N; i++ {
				out, err = r.run()
				if err != nil {
					b.Fatal(err)
				}
				reads += out.Stats.SourceReadBytes
				internal += out.Stats.InternalReadBytes
				retained += out.Stats.CacheBytesAfter
			}
			b.StopTimer()
			b.ReportMetric(float64(reads)/float64(b.N), "source-read-B/op")
			b.ReportMetric(float64(internal)/float64(b.N), "internal-read-B/op")
			b.ReportMetric(float64(retained)/float64(b.N), "cache-B/op")
			// Zero is an unavailable materialized planner, not a zero live cost.
			b.ReportMetric(float64(out.Plan.WorkingBytes), "planned-B/op")
			runtime.KeepAlive(out)
		})
	}
}

func TestLocalChainMeasuredResources(t *testing.T) {
	phase := os.Getenv("LOCAL_CHAIN_RESOURCE_PHASE")
	if phase == "" {
		t.Skip("opt-in campaign subprocess; set LOCAL_CHAIN_RESOURCE_PHASE")
	}
	r, err := newChainCampaign(phase)
	if err != nil {
		t.Fatal(err)
	}
	defer r.chain.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := r.run()
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("phase=%s source_read_B=%d internal_read_B=%d planned_B=%d result_B=%d cache_B=%d live_heap_B=%d live_heap_change_B=%d heap_inuse_B=%d cache_hit_stage=%d computed=%v",
		phase, out.Stats.SourceReadBytes, out.Stats.InternalReadBytes, out.Plan.WorkingBytes,
		len(out.Data)*4, out.Stats.CacheBytesAfter, after.HeapAlloc,
		int64(after.HeapAlloc)-int64(before.HeapAlloc), after.HeapInuse,
		out.Stats.CacheHitStage, out.Stats.Computed)
	runtime.KeepAlive(r)
	runtime.KeepAlive(out)
}
