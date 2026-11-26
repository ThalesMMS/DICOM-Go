package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// Independent direct enumeration/sorting: deliberately no product kernel,
// membership, eligibility, clamp, rank-index or path helper is used.
func rankDirect(src *Volume, mask []bool, region RankRegion, o MaskedRankOptions) (*Volume, []bool) {
	out := NewVolume(region.Size, src.SpacingMM)
	ok := make([]bool, len(out.Data))
	var radius [3]int
	for a := 0; a < 3; a++ {
		if src.Dims[a] > 1 {
			// Independent conservative enumeration; exact membership below
			// rejects excess offsets without sharing the product's search.
			radius[a] = int(math.Ceil(o.RadiusMM[a]/src.SpacingMM[a])) + 1
		}
	}
	eligible := func(i int) bool {
		return (mask == nil || mask[i]) && !math.IsNaN(float64(src.Data[i])) && !math.IsInf(float64(src.Data[i]), 0)
	}
	for z := 0; z < region.Size[2]; z++ {
		for y := 0; y < region.Size[1]; y++ {
			for x := 0; x < region.Size[0]; x++ {
				gx, gy, gz := x+region.Origin[0], y+region.Origin[1], z+region.Origin[2]
				dst := (z*region.Size[1]+y)*region.Size[0] + x
				out.Data[dst] = o.EmptyValue
				if out.Data[dst] == 0 {
					out.Data[dst] = 0
				}
				if o.Centers == RequireValidCenter && !eligible((gz*src.Dims[1]+gy)*src.Dims[0]+gx) {
					continue
				}
				var vals []float32
				for dz := -radius[2]; dz <= radius[2]; dz++ {
					for dy := -radius[1]; dy <= radius[1]; dy++ {
						for dx := -radius[0]; dx <= radius[0]; dx++ {
							offs := [3]int{dx, dy, dz}
							sum := 0.0
							included := true
							for a, d := range offs {
								if src.Dims[a] == 1 {
									continue
								}
								dist := math.Abs(float64(d) * src.SpacingMM[a])
								rad := o.RadiusMM[a]
								if rad == 0 {
									if d != 0 {
										included = false
									}
									continue
								}
								if o.Neighborhood == PhysicalBox {
									if dist > rad {
										included = false
									}
								} else {
									sum += (dist / rad) * (dist / rad)
								}
							}
							if !included || (o.Neighborhood == PhysicalEllipsoid && sum > 1) {
								continue
							}
							c := [3]int{gx + dx, gy + dy, gz + dz}
							outside := false
							for a := range c {
								if c[a] < 0 || c[a] >= src.Dims[a] {
									if o.Boundary == ClipBoundary {
										outside = true
									}
									if c[a] < 0 {
										c[a] = 0
									} else {
										c[a] = src.Dims[a] - 1
									}
								}
							}
							if outside {
								continue
							}
							i := (c[2]*src.Dims[1]+c[1])*src.Dims[0] + c[0]
							if eligible(i) {
								v := src.Data[i]
								if v == 0 {
									v = 0
								}
								vals = append(vals, v)
							}
						}
					}
				}
				if len(vals) > 0 {
					sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
					k := int(math.Floor(o.Rank * float64(len(vals)-1)))
					out.Data[dst] = vals[k]
					ok[dst] = true
				}
			}
		}
	}
	return out, ok
}
func rankAssert(t *testing.T, out *MaskedRankResult, want *Volume, valid []bool) {
	t.Helper()
	if out == nil {
		t.Fatal("nil rank result")
	}
	if out.Values.Dims != want.Dims || out.Values.SpacingMM != want.SpacingMM || len(out.Valid) != len(valid) {
		t.Fatalf("rank result headers %+v", out)
	}
	for i, v := range want.Data {
		if math.Float32bits(out.Values.Data[i]) != math.Float32bits(v) || out.Valid[i] != valid[i] {
			t.Fatalf("sample%d got%v/%v want%v/%v", i, out.Values.Data[i], out.Valid[i], v, valid[i])
		}
	}
}
func rankFixture(dims [3]int, fraction bool) *Volume {
	v := NewVolume(dims, [3]float64{.5, 1, 1.5})
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				f := float32((7*x+13*y+19*z)%17 - 8)
				if fraction {
					f = f/8 + 1.0/16
				}
				v.Data[v.Index(x, y, z)] = f
			}
		}
	}
	return v
}

// This witness is authored independently of a radius-quotient enumeration:
// binary64 43*.1 is exactly 4.3, although 4.3/.1 rounds below 43. Both
// signed endpoints must belong to the one-axis box and ellipsoid predicates.
func TestMaskedRankBinary64Endpoint(t *testing.T) {
	spacing, radius := .1, 4.3
	if math.Float64bits(spacing) != 0x3fb999999999999a || math.Float64bits(radius) != 0x4011333333333333 || 43*spacing != radius || math.Floor(radius/spacing) != 42 {
		t.Fatal("binary64 witness changed")
	}
	region := RankRegion{Origin: [3]int{43, 0, 0}, Size: [3]int{1, 1, 1}}
	for _, shape := range []RankNeighborhood{PhysicalBox, PhysicalEllipsoid} {
		for _, border := range []RankBoundary{ClipBoundary, ReplicateBoundary} {
			for _, endpoint := range []int{0, 86} {
				t.Run(fmt.Sprintf("shape%d/border%d/endpoint%d", shape, border, endpoint), func(t *testing.T) {
					src := NewVolume([3]int{87, 1, 1}, [3]float64{spacing, 0, 0})
					src.Data[endpoint] = 99
					o := MaskedRankOptions{Rank: 1, RadiusMM: [3]float64{radius, 0, 0}, Neighborhood: shape, Boundary: border, Workers: 1}
					for _, path := range []rankPath{rankPathAuto, rankPathSelect, rankPathHistogram} {
						out, err := maskedRank(nil, src, nil, region, o, path)
						if err != nil || out == nil || !out.Valid[0] || out.Values.Data[0] != 99 {
							t.Fatalf("path%d omitted exact physical endpoint: output%v err%v", path, out, err)
						}
					}
					if src.Data[endpoint] != 99 || src.Data[43] != 0 {
						t.Fatal("source modified")
					}
					r, err := rankAdmit(context.Background(), src, nil, region, o)
					if err != nil || r.radius[0] != 43 || r.samples != 87 || r.rows != 1 {
						t.Fatalf("exact endpoint admission: request%v err%v", r, err)
					}
					// The adjacent representable smaller radius excludes both endpoints.
					o.RadiusMM[0] = math.Nextafter(radius, 0)
					out, err := MaskedRankRegion(nil, src, nil, region, o)
					if err != nil || out.Values.Data[0] != 0 || !out.Valid[0] {
						t.Fatalf("no endpoint inflation: output%v err%v", out, err)
					}
				})
			}
		}
	}
	for _, shape := range []RankNeighborhood{PhysicalBox, PhysicalEllipsoid} {
		src := NewVolume([3]int{87, 1, 1}, [3]float64{spacing, 0, 0})
		src.Data[43] = float32(math.NaN())
		o := MaskedRankOptions{Rank: 1, RadiusMM: [3]float64{radius, 0, 0}, Neighborhood: shape}
		for _, limits := range []RankLimits{{MaxKernelSamples: 86}, {MaxSampleVisits: 260}} {
			o.Limits = limits
			out, err := MaskedRankRegion(nil, src, nil, region, o)
			if out != nil || !errors.Is(err, ErrRankResourceLimit) {
				t.Fatalf("exact bounding/visit charge before NaN scan: shape%d limits%v out%v err%v", shape, limits, out, err)
			}
		}
		src.Data[43] = 0
		o.Limits = RankLimits{MaxKernelSamples: 87, MaxSampleVisits: 261}
		if out, err := MaskedRankRegion(nil, src, nil, region, o); err != nil || out == nil {
			t.Fatalf("exact charge admitted: shape%d out%v err%v", shape, out, err)
		}
		// The finite quota endpoint is still physically inside: refuse rather
		// than capping/shrinking a huge kernel or converting an infinite quotient.
		src.SpacingMM[0] = math.SmallestNonzeroFloat64
		o.RadiusMM[0] = math.MaxFloat64
		o.Limits = RankLimits{MaxKernelSamples: 87}
		if out, err := MaskedRankRegion(nil, src, nil, region, o); out != nil || !errors.Is(err, ErrRankResourceLimit) {
			t.Fatalf("subnormal axis quota: shape%d out%v err%v", shape, out, err)
		}
		o.RadiusMM[0] = 0
		o.RadiusMM[1] = math.MaxFloat64 // singleton axes still collapse
		o.Limits = RankLimits{MaxKernelSamples: 1, MaxSampleVisits: 89}
		if out, err := MaskedRankRegion(nil, src, nil, region, o); err != nil || out == nil || !out.Valid[0] {
			t.Fatalf("zero radius and singleton: shape%d out%v err%v", shape, out, err)
		}
	}
}
func TestMaskedRankDirect(t *testing.T) {
	for _, dims := range [][3]int{{7, 6, 1}, {7, 6, 5}} {
		for _, fraction := range []bool{false, true} {
			src := rankFixture(dims, fraction)
			before := append([]float32(nil), src.Data...)
			for _, shape := range []RankNeighborhood{PhysicalBox, PhysicalEllipsoid} {
				for _, boundary := range []RankBoundary{ClipBoundary, ReplicateBoundary} {
					for _, center := range []RankCenters{RequireValidCenter, AnyValidNeighborhood} {
						for _, q := range []float64{0, .25, .5, .75, 1} {
							for pattern := 0; pattern < 3; pattern++ {
								name := fmt.Sprintf("%v/fraction%v/shape%d/boundary%d/center%d/q%v/mask%d", dims, fraction, shape, boundary, center, q, pattern)
								t.Run(name, func(t *testing.T) {
									var mask []bool
									if pattern > 0 {
										mask = make([]bool, len(src.Data))
										for i := range mask {
											mask[i] = pattern == 1 && i%4 != 0
										}
									}
									o := MaskedRankOptions{Rank: q, RadiusMM: [3]float64{.75, 1, 1.5}, Neighborhood: shape, Boundary: boundary, Centers: center, EmptyValue: -99.5, Workers: 3}
									out, err := MaskedRank(nil, src, mask, o)
									if err != nil {
										t.Fatal(err)
									}
									want, valid := rankDirect(src, mask, RankRegion{Size: dims}, o)
									rankAssert(t, out, want, valid)
								})
							}
						}
					}
				}
			}
			if !reflect.DeepEqual(src.Data, before) {
				t.Fatal("source mutated")
			}
		}
	}
}
func TestMaskedRankEvenValidityAndReplication(t *testing.T) {
	src := &Volume{Dims: [3]int{3, 1, 1}, SpacingMM: [3]float64{1, 0, 0}, Data: []float32{1, 9, 3}}
	o := MaskedRankOptions{Rank: .5, RadiusMM: [3]float64{1, 0, 0}}
	out, err := MaskedRank(nil, src, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	// Atx0 Clip has [1,9]: lower median1. Atx2 [9,3]: lower median3.
	if !reflect.DeepEqual(out.Values.Data, []float32{1, 3, 3}) {
		t.Fatal(out.Values.Data)
	}
	mask := []bool{true, false, true}
	o.Centers = AnyValidNeighborhood
	o.Rank = 1
	out, err = MaskedRank(nil, src, mask, o)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Valid[1] || out.Values.Data[1] != 3 {
		t.Fatal(out)
	}
	o.Centers = RequireValidCenter
	out, err = MaskedRank(nil, src, mask, o)
	if err != nil {
		t.Fatal(err)
	}
	if out.Valid[1] || out.Values.Data[1] != 0 {
		t.Fatal(out)
	}
	o.Centers = AnyValidNeighborhood
	o.Boundary = ReplicateBoundary
	o.Rank = .5
	src.Data = []float32{9, 1, 3}
	out, err = MaskedRank(nil, src, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	if out.Values.Data[0] != 9 {
		t.Fatal("replicated sample multiplicity lost", out.Values.Data)
	}
}
func TestMaskedRankNonFiniteAndZero(t *testing.T) {
	src := &Volume{Dims: [3]int{5, 1, 1}, SpacingMM: [3]float64{1, 0, 0}, Data: []float32{float32(math.NaN()), float32(math.Inf(1)), math.Float32frombits(1 << 31), 0, 2}}
	mask := []bool{false, false, true, true, true}
	o := MaskedRankOptions{Rank: .5, RadiusMM: [3]float64{1, 0, 0}, EmptyValue: -7}
	out, err := MaskedRank(nil, src, mask, o)
	if err != nil {
		t.Fatal(err)
	}
	if out.Valid[0] || out.Values.Data[0] != -7 || math.Float32bits(out.Values.Data[2]) != 0 {
		t.Fatal(out.Values.Data, out.Valid)
	}
	if out, err = MaskedRank(nil, src, nil, o); out != nil || !errors.Is(err, ErrInvalidVolume) {
		t.Fatal(out, err)
	}
	o.NonFinite = ExcludeNonFinite
	o.Centers = AnyValidNeighborhood
	out, err = MaskedRank(nil, src, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	want, valid := rankDirect(src, nil, RankRegion{Size: src.Dims}, o)
	rankAssert(t, out, want, valid)
	if math.Float32bits(src.Data[2]) != 1<<31 || !math.IsNaN(float64(src.Data[0])) {
		t.Fatal("source modified")
	}
}
func TestMaskedRankPathsRegionsWorkers(t *testing.T) {
	src := rankFixture([3]int{9, 8, 5}, false)
	mask := make([]bool, len(src.Data))
	for i := range mask {
		mask[i] = i%5 != 0
	}
	for _, shape := range []RankNeighborhood{PhysicalBox, PhysicalEllipsoid} {
		for _, boundary := range []RankBoundary{ClipBoundary, ReplicateBoundary} {
			for _, q := range []float64{0, .5, 1} {
				o := MaskedRankOptions{Rank: q, RadiusMM: [3]float64{1.5, 2, 3}, Neighborhood: shape, Boundary: boundary, Centers: AnyValidNeighborhood, EmptyValue: -99}
				full, err := maskedRank(nil, src, mask, RankRegion{Size: src.Dims}, o, rankPathSelect)
				if err != nil {
					t.Fatal(err)
				}
				for _, workers := range []int{1, 2, 3} {
					o.Workers = workers
					hist, err := maskedRank(nil, src, mask, RankRegion{Size: src.Dims}, o, rankPathHistogram)
					if err != nil {
						t.Fatal(err)
					}
					rankAssert(t, hist, full.Values, full.Valid)
					for z := 0; z < src.Dims[2]; z++ {
						for x := 0; x < src.Dims[0]; x += 4 {
							reg := RankRegion{Origin: [3]int{x, 0, z}, Size: [3]int{min(4, src.Dims[0]-x), src.Dims[1], 1}}
							tile, err := MaskedRankRegion(nil, src, mask, reg, o)
							if err != nil {
								t.Fatal(err)
							}
							if tile.Region != reg {
								t.Fatal(tile.Region)
							}
							for y := 0; y < reg.Size[1]; y++ {
								for tx := 0; tx < reg.Size[0]; tx++ {
									a := tile.Values.Index(tx, y, 0)
									b := src.Index(x+tx, y, z)
									if math.Float32bits(tile.Values.Data[a]) != math.Float32bits(full.Values.Data[b]) || tile.Valid[a] != full.Valid[b] {
										t.Fatalf("tile mismatch %v/%d", reg, a)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}
func TestMaskedRankMedianCoincidenceAndPhysicalRadius(t *testing.T) {
	src := rankFixture([3]int{9, 7, 3}, true)
	radius := [3]float64{1, 2, 1.5}
	old, err := Median(nil, src, MedianOptions{RadiusMM: radius, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	out, err := MaskedRank(nil, src, nil, MaskedRankOptions{Rank: .5, RadiusMM: radius, Boundary: ReplicateBoundary, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	valid := make([]bool, len(old.Data))
	for i := range valid {
		valid[i] = true
	}
	rankAssert(t, out, old, valid)
	s := &Volume{Dims: [3]int{3, 1, 1}, SpacingMM: [3]float64{1, 0, 0}, Data: []float32{0, 9, 0}}
	n, err := MaskedRank(nil, s, nil, MaskedRankOptions{Rank: .5, RadiusMM: [3]float64{.5, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Median(nil, s, MedianOptions{RadiusMM: [3]float64{.5, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if n.Values.Data[1] != 9 || m.Data[1] != 0 {
		t.Fatal(n.Values.Data, m.Data)
	}
	// A division rounded to an integer cannot bypass exact physical membership.
	s.SpacingMM[0] = .1
	o := MaskedRankOptions{Rank: 1, RadiusMM: [3]float64{math.Nextafter(.3, 0), 0, 0}}
	a, e := MaskedRank(nil, s, nil, o)
	if e != nil {
		t.Fatal(e)
	}
	w, v := rankDirect(s, nil, RankRegion{Size: s.Dims}, o)
	rankAssert(t, a, w, v)
}
func TestMaskedRankAdmission(t *testing.T) {
	src := rankFixture([3]int{3, 3, 1}, false)
	base := MaskedRankOptions{Rank: .5, RadiusMM: [3]float64{1, 1, 0}}
	cases := []struct {
		name   string
		change func(*Volume, *MaskedRankOptions)
		target error
	}{
		{"rankNaN", func(_ *Volume, o *MaskedRankOptions) { o.Rank = math.NaN() }, ErrInvalidVolume},
		{"rankRange", func(_ *Volume, o *MaskedRankOptions) { o.Rank = 1.1 }, ErrInvalidVolume},
		{"shape", func(_ *Volume, o *MaskedRankOptions) { o.Neighborhood = 255 }, ErrInvalidVolume},
		{"boundary", func(_ *Volume, o *MaskedRankOptions) { o.Boundary = 255 }, ErrInvalidVolume},
		{"centers", func(_ *Volume, o *MaskedRankOptions) { o.Centers = 255 }, ErrInvalidVolume},
		{"finitePolicy", func(_ *Volume, o *MaskedRankOptions) { o.NonFinite = 255 }, ErrInvalidVolume},
		{"emptyInf", func(_ *Volume, o *MaskedRankOptions) { o.EmptyValue = float32(math.Inf(-1)) }, ErrInvalidVolume},
		{"workersNegative", func(_ *Volume, o *MaskedRankOptions) { o.Workers = -1 }, ErrInvalidVolume},
		{"workersCeiling", func(_ *Volume, o *MaskedRankOptions) { o.Workers = 65 }, ErrInvalidVolume},
		{"radius", func(_ *Volume, o *MaskedRankOptions) { o.RadiusMM[0] = -1 }, ErrInvalidVolume},
		{"collapsedRadiusNaN", func(_ *Volume, o *MaskedRankOptions) { o.RadiusMM[2] = math.NaN() }, ErrInvalidVolume},
		{"spacing", func(v *Volume, _ *MaskedRankOptions) { v.SpacingMM[0] = 0 }, ErrInvalidVolume},
		{"dimensionsOverflow", func(v *Volume, _ *MaskedRankOptions) { v.Dims = [3]int{math.MaxInt, 3, 1} }, ErrInvalidVolume},
		{"quotaNegative", func(_ *Volume, o *MaskedRankOptions) { o.Limits.MaxVoxels = -1 }, ErrInvalidVolume},
		{"quotaCeiling", func(_ *Volume, o *MaskedRankOptions) { o.Limits.MaxSampleVisits = (1 << 40) + 1 }, ErrInvalidVolume},
		{"sourceQuota", func(_ *Volume, o *MaskedRankOptions) { o.Limits.MaxVoxels = 1 }, ErrRankResourceLimit},
		{"kernelQuota", func(_ *Volume, o *MaskedRankOptions) { o.Limits.MaxKernelSamples = 2 }, ErrRankResourceLimit},
		{"visitQuota", func(_ *Volume, o *MaskedRankOptions) { o.Limits.MaxSampleVisits = 1 }, ErrRankResourceLimit},
		{"byteQuota", func(_ *Volume, o *MaskedRankOptions) { o.Limits.MaxWorkingBytes = 1 }, ErrRankResourceLimit},
		{"hugeQuotient", func(_ *Volume, o *MaskedRankOptions) { o.RadiusMM[0] = math.MaxFloat64 }, ErrRankResourceLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := src.Clone()
			o := base
			c.change(v, &o)
			out, err := MaskedRank(nil, v, nil, o)
			if out != nil || !errors.Is(err, c.target) {
				t.Fatal(out, err)
			}
		})
	}
	if out, err := MaskedRank(nil, nil, nil, base); out != nil || !errors.Is(err, ErrInvalidVolume) {
		t.Fatal(out, err)
	}
	if out, err := MaskedRank(nil, src, []bool{true}, base); out != nil || !errors.Is(err, ErrInvalidVolume) {
		t.Fatal(out, err)
	}
	for _, reg := range []RankRegion{{}, {Origin: [3]int{-1, 0, 0}, Size: [3]int{1, 1, 1}}, {Origin: [3]int{math.MaxInt, 0, 0}, Size: [3]int{1, 1, 1}}, {Size: [3]int{4, 1, 1}}} {
		if out, err := MaskedRankRegion(nil, src, nil, reg, base); out != nil || !errors.Is(err, ErrInvalidVolume) {
			t.Fatal(out, err)
		}
	}
	// A quota failure wins over a would-be invalid payload; no scan is performed.
	src.Data[0] = float32(math.NaN())
	base.Limits.MaxWorkingBytes = 1
	if out, err := MaskedRank(nil, src, nil, base); out != nil || !errors.Is(err, ErrRankResourceLimit) {
		t.Fatal(out, err)
	}
}

// Deterministic context cancels only after a fixed number of polls. This lets
// us exercise an internal stage without scheduler sleeps or timing guesses.
type rankCancelContext struct {
	context.Context
	cancel context.CancelFunc
	calls  atomic.Int64
	at     int64
}

func rankCancelAfter(n int) *rankCancelContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &rankCancelContext{Context: ctx, cancel: cancel, at: int64(n)}
}
func (c *rankCancelContext) Err() error {
	if c.calls.Add(1) >= c.at {
		c.cancel()
	}
	return c.Context.Err()
}
func TestMaskedRankInternalCancellation(t *testing.T) {
	src := rankFixture([3]int{128, 64, 1}, true)
	o := MaskedRankOptions{Rank: .5, RadiusMM: [3]float64{8, 8, 0}, Workers: 1}
	already, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := MaskedRank(already, src, nil, o); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(out, err)
	}
	before := append([]float32(nil), src.Data...)
	budgetBefore := parallel.InUse()
	// Include completed admissions followed by cancellations in scan/gather.
	for _, polls := range []int{2, 4, 7, 9} {
		ctx := rankCancelAfter(polls)
		out, err := MaskedRank(ctx, src, nil, o)
		if out != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("poll%d resultNil=%v err=%v", polls, out == nil, err)
		}
	}
	if parallel.InUse() != budgetBefore || !reflect.DeepEqual(src.Data, before) {
		t.Fatal("cancel ownership/helper leak")
	}
	// Direct cancellation of histogram clear, partition and heap fallback.
	r, err := rankAdmit(context.Background(), rankFixture([3]int{128, 64, 1}, false), nil, RankRegion{Size: src.Dims}, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = r.makeKernel(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := rankScratch{rows: make([]rankRow, r.rows), hist: newMedianHistogram(65536), poll: rankPoll{ctx: rankCancelAfter(2)}}
	out := &MaskedRankResult{Values: NewVolume(src.Dims, src.SpacingMM), Valid: make([]bool, len(src.Data))}
	if err = r.line(&s, out, 0); !errors.Is(err, context.Canceled) {
		t.Fatal("histogram cancellation", err)
	}
	vals := make([]float32, 65536)
	for i := range vals {
		vals[i] = float32(len(vals) - i)
	}
	for _, depth := range []int{0, 32} {
		p := rankPoll{ctx: rankCancelAfter(1)}
		if _, err := rankSelectDepth(vals, len(vals)/2, depth, &p); !errors.Is(err, context.Canceled) {
			t.Fatal("selector cancellation", err)
		}
	}
}
func TestMaskedRankConcurrentOwnership(t *testing.T) {
	src := rankFixture([3]int{15, 11, 3}, false)
	mask := make([]bool, len(src.Data))
	for i := range mask {
		mask[i] = i%7 != 0
	}
	before := src.Clone()
	mb := append([]bool(nil), mask...)
	o := MaskedRankOptions{Rank: .75, RadiusMM: [3]float64{1.5, 2, 1.5}, Workers: 3}
	want, v := rankDirect(src, mask, RankRegion{Size: src.Dims}, o)
	var wg sync.WaitGroup
	for call := 0; call < 6; call++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := MaskedRank(nil, src, mask, o)
			if err != nil {
				t.Error(err)
				return
			}
			rankAssert(t, out, want, v)
			out.Values.Data[0] = 123
			out.Valid[0] = !out.Valid[0]
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(src, before) || !reflect.DeepEqual(mask, mb) {
		t.Fatal("borrowed input mutation")
	}
}
func TestMaskedRankCapacityAndSelector(t *testing.T) {
	if unsafe.Sizeof(rankSpan{}) != 12 || unsafe.Sizeof(rankRow{}) > 16 {
		t.Fatal("rank row ledger", unsafe.Sizeof(rankSpan{}), unsafe.Sizeof(rankRow{}))
	}
	for _, dims := range [][3]int{{9, 8, 3}, {1, 1, 1}} {
		src := rankFixture(dims, false)
		o := MaskedRankOptions{RadiusMM: [3]float64{1.5, 2, 1.5}, Workers: 3}
		r, err := rankAdmit(context.Background(), src, nil, RankRegion{Size: dims}, o)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.makeKernel(context.Background()); err != nil {
			t.Fatal(err)
		}
		if cap(r.kernel) != r.rows {
			t.Fatal("kernel capacity")
		}
		actual := int64(5*r.outputs) + int64(cap(r.kernel))*int64(unsafe.Sizeof(rankSpan{})) + int64(r.workers)*(int64(r.rows)*int64(unsafe.Sizeof(rankRow{}))+4*(65536+256)+int64(unsafe.Sizeof(rankScratch{}))) + 4096
		if actual > r.working {
			t.Fatal("retained capacities exceed ledger", actual, r.working)
		}
	}
	p := rankPoll{ctx: context.Background()}
	for n := 1; n <= 257; n++ {
		vals := make([]float32, n)
		for i := range vals {
			vals[i] = float32((i*37)%53)/8 - .0625
		}
		sorted := append([]float32(nil), vals...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		for _, q := range []float64{0, .25, .5, .75, 1} {
			k := int(q * float64(n-1))
			a := append([]float32(nil), vals...)
			got, err := rankSelect(a, k, &p)
			if err != nil || got != sorted[k] {
				t.Fatal(n, k, got, sorted[k], err)
			}
			a = append([]float32(nil), vals...)
			got, err = rankSelectDepth(a, k, 0, &p)
			if err != nil || got != sorted[k] {
				t.Fatal("fallback", n, k, got, sorted[k], err)
			}
		}
	}
}

func TestMaskedRankExtremeAndDefaultBudget(t *testing.T) {
	src := &Volume{Dims: [3]int{9, 1, 1}, SpacingMM: [3]float64{.5, 0, 0}, Data: []float32{-math.MaxFloat32, math.SmallestNonzeroFloat32, -math.SmallestNonzeroFloat32, 0, math.MaxFloat32, math.Float32frombits(1 << 31), -1.125, 1.375, 2.25}}
	for _, q := range []float64{0, .1, .3333333333333333, .5, math.Nextafter(.5, 1), 1} {
		o := MaskedRankOptions{Rank: q, RadiusMM: [3]float64{1.5, 0, 0}, Boundary: ReplicateBoundary}
		out, err := MaskedRank(nil, src, nil, o)
		if err != nil {
			t.Fatal(err)
		}
		want, valid := rankDirect(src, nil, RankRegion{Size: src.Dims}, o)
		rankAssert(t, out, want, valid)
	}
	// Default capture caps before adding the caller, so an extreme legal shared
	// budget cannot overflow the requested worker count or increase allocations.
	old := parallel.SetBudget(math.MaxInt)
	defer parallel.SetBudget(old)
	grid := rankFixture([3]int{2, 70, 1}, false)
	out, err := MaskedRank(nil, grid, nil, MaskedRankOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.WorkerLimit != 64 {
		t.Fatal("captured default overflow", out.WorkerLimit)
	}
}
