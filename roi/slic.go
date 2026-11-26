package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
)

var ErrSLICInput = errors.New("roi: invalid SLIC input")
var ErrSLICLimit = errors.New("roi: SLIC resource limit")

type SLICConnectivity uint8

const (
	SLICFaceConnectivity SLICConnectivity = iota // 4 in 2D, 6 in 3D.
	SLICFullConnectivity                         // 8 in 2D, 26 in 3D.
)

// SLICGrid contains scalar values in x-fastest order. Spacing is millimetres.
// Values and optional Available are borrowed and must remain stable until return.
// A 2D image has Size[2]=1. No channels or units are mixed implicitly.
type SLICGrid struct {
	Size      [3]int
	Spacing   [3]float64
	Values    []float64
	Available []bool
}

// SLICOptions defines a physical-space SLIC partition, not clinical classes.
// Distance squared is ((value-center)/IntensityScale)^2 plus Compactness^2
// times physical squared distance/TargetStepMM^2. Initialization selects the
// nearest available cell-center voxel deterministically, without random seeds
// or gradient perturbation. Iterations bounds Lloyd steps; connectivity is
// enforced afterwards by splitting disconnected labels, never merging them.
type SLICOptions struct {
	TargetStepMM, Compactness, IntensityScale float64
	Iterations                                int
	Connectivity                              SLICConnectivity
	RejectNonFinite                           bool // Otherwise nonfinite values are unavailable.
	MaxVoxels, MaxClusters, MaxBytes, MaxWork int64
	MaxIterations                             int
}

// SLICResult owns Labels (zero means unavailable) and records its parameters.
// Converged describes assignment stability before connectivity splitting.
// Reaching the iteration cap is reported explicitly, not as convergence.
type SLICResult struct {
	Size                                  [3]int
	Spacing                               [3]float64
	Labels                                []uint32
	Options                               SLICOptions
	SeedStepVoxels                        [3]int
	SeedStepMM                            [3]float64
	InitialClusters, Clusters, Iterations int
	AvailableVoxels, UnavailableVoxels    int
	Converged, IterationLimitReached      bool
	ConnectivitySplits                    int
	Objective                             float64 // Last assignment objective, before connectivity splitting.
	ReservedBytes, Work                   int64   // Logical admitted storage/work, not RSS.
}
type slicCenter struct{ x, y, z, value float64 }
type slicRun struct {
	ctx         context.Context
	work, limit int64
}

func (r *slicRun) tick() error {
	if r.work >= r.limit {
		return fmt.Errorf("%w: work", ErrSLICLimit)
	}
	r.work++
	if r.work&1023 == 0 {
		return r.ctx.Err()
	}
	return nil
}
func slicFinite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
func slicOptions(o SLICOptions) (SLICOptions, error) {
	if !slicFinite(o.TargetStepMM) || o.TargetStepMM <= 0 || !slicFinite(o.IntensityScale) || o.IntensityScale <= 0 || !slicFinite(o.Compactness) || o.Compactness < 0 || o.Iterations <= 0 || o.Connectivity > SLICFullConnectivity {
		return o, ErrSLICInput
	}
	defaults := []int64{16 << 20, 65536, 1 << 30, 100000000}
	ps := []*int64{&o.MaxVoxels, &o.MaxClusters, &o.MaxBytes, &o.MaxWork}
	for i, p := range ps {
		if *p < 0 {
			return o, ErrSLICInput
		}
		if *p == 0 {
			*p = defaults[i]
		}
	}
	if o.MaxIterations < 0 {
		return o, ErrSLICInput
	}
	if o.MaxIterations == 0 {
		o.MaxIterations = 100
	}
	if o.Iterations > o.MaxIterations {
		return o, fmt.Errorf("%w: iterations", ErrSLICLimit)
	}
	if o.MaxClusters > math.MaxUint32 {
		return o, ErrSLICInput
	}
	return o, nil
}
func slicIndex(s [3]int, x, y, z int) int { return (z*s[1]+y)*s[0] + x }
func slicCoords(s [3]int, i int) [3]int {
	return [3]int{i % s[0], (i / s[0]) % s[1], i / (s[0] * s[1])}
}

// SLIC partitions a borrowed scalar grid into deterministic connected units.
// Limits/context errors return nil, never a partially qualified result.
func SLIC(ctx context.Context, g SLICGrid, o SLICOptions) (*SLICResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	var e error
	o, e = slicOptions(o)
	if e != nil {
		return nil, e
	}
	n := int64(1)
	var step, cells [3]int
	for a, d := range g.Size {
		if d <= 0 || !slicFinite(g.Spacing[a]) || g.Spacing[a] <= 0 {
			return nil, ErrSLICInput
		}
		if int64(d) > o.MaxVoxels/n {
			return nil, fmt.Errorf("%w: voxels", ErrSLICLimit)
		}
		n *= int64(d)
		ratio := o.TargetStepMM / g.Spacing[a]
		if ratio >= float64(d) || math.IsInf(ratio, 1) {
			step[a] = d
		} else {
			step[a] = int(math.Floor(ratio + .5))
			if step[a] < 1 {
				step[a] = 1
			}
		}
		cells[a] = 1 + (d-1)/step[a]
		// Admit normalized physical coordinates before working allocation.
		bound := float64(d) * g.Spacing[a] / o.TargetStepMM
		if !slicFinite(bound) || math.Abs(bound) > math.Sqrt(math.MaxFloat64/64) || !slicFinite(bound*o.Compactness) || math.Abs(bound*o.Compactness) > math.Sqrt(math.MaxFloat64/64) {
			return nil, ErrSLICInput
		}
	}
	if n > int64(int(^uint(0)>>1)) || int64(len(g.Values)) != n || (g.Available != nil && int64(len(g.Available)) != n) {
		return nil, ErrSLICInput
	}
	k := int64(1)
	for _, c := range cells {
		if int64(c) > o.MaxClusters/k {
			return nil, fmt.Errorf("%w: initial clusters", ErrSLICLimit)
		}
		k *= int64(c)
	}
	// Includes borrowed input accounting, validity, distances, old/final labels,
	// full flood queue, centers/means and conservative allocator-independent slots.
	if n > (math.MaxInt64-32768)/64 || k > (math.MaxInt64-32768-64*n)/256 {
		return nil, ErrSLICLimit
	}
	reserved := 32768 + 64*n + 256*k
	if reserved > o.MaxBytes {
		return nil, fmt.Errorf("%w: bytes", ErrSLICLimit)
	}
	run := slicRun{ctx: ctx, limit: o.MaxWork}
	// Validate the numeric domain before allocating work arrays.
	avail := 0
	for i, v := range g.Values {
		if e = run.tick(); e != nil {
			return nil, e
		}
		if g.Available != nil && !g.Available[i] {
			continue
		}
		if !slicFinite(v) {
			if o.RejectNonFinite {
				return nil, ErrSLICInput
			}
			continue
		}
		normalized := v / o.IntensityScale
		if !slicFinite(normalized) || math.Abs(normalized) > math.Sqrt(math.MaxFloat64/64) {
			return nil, ErrSLICInput
		}
		avail++
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	result := &SLICResult{Size: g.Size, Spacing: g.Spacing, Options: o, AvailableVoxels: avail, UnavailableVoxels: int(n) - avail, ReservedBytes: reserved}
	result.SeedStepVoxels = step
	for a := range step {
		result.SeedStepMM[a] = float64(step[a]) * g.Spacing[a]
	}
	valid := make([]bool, int(n))
	for i, v := range g.Values {
		if e = run.tick(); e != nil {
			return nil, e
		}
		valid[i] = (g.Available == nil || g.Available[i]) && slicFinite(v)
	}
	labels := make([]uint32, int(n))
	dist := make([]float64, int(n))
	if avail == 0 {
		result.Labels = labels
		result.Converged = true
		result.Work = run.work
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		return result, nil
	}
	centers := make([]slicCenter, 0, int(k))
	for z := 0; z < g.Size[2]; z += step[2] {
		for y := 0; y < g.Size[1]; y += step[1] {
			for x := 0; x < g.Size[0]; x += step[0] {
				end := [3]int{x + min(step[0], g.Size[0]-x), y + min(step[1], g.Size[1]-y), z + min(step[2], g.Size[2]-z)}
				center := [3]float64{(float64(x) + float64(end[0]) - 1) / 2, (float64(y) + float64(end[1]) - 1) / 2, (float64(z) + float64(end[2]) - 1) / 2}
				best := math.Inf(1)
				chosen := -1
				for zz := z; zz < end[2]; zz++ {
					for yy := y; yy < end[1]; yy++ {
						for xx := x; xx < end[0]; xx++ {
							if e = run.tick(); e != nil {
								return nil, e
							}
							i := slicIndex(g.Size, xx, yy, zz)
							if !valid[i] {
								continue
							}
							d := 0.0
							for a, c := range [3]int{xx, yy, zz} {
								v := (float64(c) - center[a]) * g.Spacing[a] / o.TargetStepMM
								d += v * v
							}
							if d < best {
								best = d
								chosen = i
							}
						}
					}
				}
				if chosen >= 0 {
					c := slicCoords(g.Size, chosen)
					centers = append(centers, slicCenter{float64(c[0]), float64(c[1]), float64(c[2]), g.Values[chosen] / o.IntensityScale})
				}
			}
		}
	}
	result.InitialClusters = len(centers)
	score := func(i int, c slicCenter) float64 {
		coord := slicCoords(g.Size, i)
		v := g.Values[i]/o.IntensityScale - c.value
		d := v * v
		for a, p := range [3]float64{c.x, c.y, c.z} {
			delta := (float64(coord[a]) - p) * g.Spacing[a] / o.TargetStepMM * o.Compactness
			d += delta * delta
		}
		return d
	}
	previous := make([]uint32, int(n))
	sums := make([]slicCenter, len(centers))
	counts := make([]int, len(centers))
	for iteration := 0; iteration < o.Iterations; iteration++ {
		for i, id := range labels {
			if e = run.tick(); e != nil {
				return nil, e
			}
			previous[i] = id
		}
		for i := range dist {
			if e = run.tick(); e != nil {
				return nil, e
			}
			dist[i] = math.Inf(1)
			labels[i] = 0
		}
		for ci, c := range centers {
			cc := [3]float64{c.x, c.y, c.z}
			var lo, hi [3]int
			for a := 0; a < 3; a++ {
				lo[a] = int(max(int64(0), int64(math.Floor(cc[a]))-2*int64(step[a])))
				hi[a] = int(min(int64(g.Size[a]-1), int64(math.Ceil(cc[a]))+2*int64(step[a])))
			}
			for z := lo[2]; z <= hi[2]; z++ {
				for y := lo[1]; y <= hi[1]; y++ {
					for x := lo[0]; x <= hi[0]; x++ {
						if e = run.tick(); e != nil {
							return nil, e
						}
						i := slicIndex(g.Size, x, y, z)
						if !valid[i] {
							continue
						}
						d := score(i, c)
						if !slicFinite(d) {
							return nil, ErrSLICInput
						}
						if d < dist[i] {
							dist[i] = d
							labels[i] = uint32(ci + 1)
						}
					}
				}
			}
		}
		// Holes in the domain can leave a voxel outside all local windows. Search
		// every initialized center for that voxel rather than dropping it silently.
		for i := range labels {
			if !valid[i] || labels[i] != 0 {
				continue
			}
			for ci, c := range centers {
				if e = run.tick(); e != nil {
					return nil, e
				}
				d := score(i, c)
				if !slicFinite(d) {
					return nil, ErrSLICInput
				}
				if d < dist[i] {
					dist[i] = d
					labels[i] = uint32(ci + 1)
				}
			}
		}
		for i := range sums {
			if e = run.tick(); e != nil {
				return nil, e
			}
			sums[i] = slicCenter{}
			counts[i] = 0
		}
		changed := false
		objective := 0.0
		observed := 0
		for i, id := range labels {
			if e = run.tick(); e != nil {
				return nil, e
			}
			if !valid[i] {
				continue
			}
			if id == 0 {
				return nil, ErrSLICInput
			}
			if id != previous[i] {
				changed = true
			}
			ci := int(id) - 1
			counts[ci]++
			c := slicCoords(g.Size, i)
			a := sums[ci]
			weight := 1 / float64(counts[ci])
			a.x += (float64(c[0]) - a.x) * weight
			a.y += (float64(c[1]) - a.y) * weight
			a.z += (float64(c[2]) - a.z) * weight
			a.value += (g.Values[i]/o.IntensityScale - a.value) * weight
			sums[ci] = a
			// Mean per-voxel assignment energy avoids accumulation overflow.
			observed++
			objective += (dist[i] - objective) / float64(observed)
		}
		for i, c := range sums {
			if counts[i] > 0 {
				centers[i] = c
			}
		}
		result.Iterations = iteration + 1
		result.Objective = objective
		if !changed {
			result.Converged = true
			break
		}
	}
	result.IterationLimitReached = !result.Converged
	// Split disconnected pieces in x-fastest traversal order; final cluster IDs
	// are partition-local, not persistent DICOM Segment Numbers.
	connected := make([]uint32, int(n))
	queue := make([]int, 0, int(n))
	pieces := make([]int, len(centers))
	next := uint32(0)
	for start, old := range labels {
		if e = run.tick(); e != nil {
			return nil, e
		}
		if old == 0 || connected[start] != 0 {
			continue
		}
		if int64(next) >= o.MaxClusters {
			return nil, fmt.Errorf("%w: connected clusters", ErrSLICLimit)
		}
		next++
		pieces[int(old)-1]++
		connected[start] = next
		queue = queue[:0]
		queue = append(queue, start)
		for head := 0; head < len(queue); head++ {
			c := slicCoords(g.Size, queue[head])
			for dz := -1; dz <= 1; dz++ {
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if e = run.tick(); e != nil {
							return nil, e
						}
						norm := absSLIC(dx) + absSLIC(dy) + absSLIC(dz)
						if norm == 0 || (o.Connectivity == SLICFaceConnectivity && norm != 1) {
							continue
						}
						x, y, z := c[0]+dx, c[1]+dy, c[2]+dz
						if x < 0 || y < 0 || z < 0 || x >= g.Size[0] || y >= g.Size[1] || z >= g.Size[2] {
							continue
						}
						j := slicIndex(g.Size, x, y, z)
						if labels[j] == old && connected[j] == 0 {
							connected[j] = next
							queue = append(queue, j)
						}
					}
				}
			}
		}
	}
	for _, p := range pieces {
		if p > 1 {
			result.ConnectivitySplits += p - 1
		}
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	result.Labels = connected
	result.Clusters = int(next)
	result.Work = run.work
	return result, nil
}
func absSLIC(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
