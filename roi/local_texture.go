package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

type LocalTextureFeature string

const (
	LocalContrast         LocalTextureFeature = "Contrast"
	LocalEntropy          LocalTextureFeature = "Joint entropy"
	LocalEnergy           LocalTextureFeature = "Angular second moment"
	LocalHomogeneity      LocalTextureFeature = "Inverse difference moment"
	LocalTextureMaxVoxels                     = 1 << 20
	LocalTextureMaxBytes  int64               = 256 << 20
	LocalTextureMaxVisits int64               = 200_000_000
)

var ErrLocalTextureLimit = errors.New("roi: local texture resource limit")

type LocalTextureQuantization string

const (
	LocalTextureGlobal LocalTextureQuantization = "global minimum"
	LocalTextureWindow LocalTextureQuantization = "window minimum"
)

// LocalTextureRegion is a half-open source-grid box. Nil requests the image.
type LocalTextureRegion struct{ Min, Max [3]int }

// LocalTextureOptions describes a clipped rectangular physical neighborhood.
// Pairs must lie inside the window, mask and finite intensity domain. Singleton
// axes have no neighbors; their radius is ignored. Quantization is fixed width,
// with either the whole-mask or each window's minimum as origin. LowerBound is
// an optional resegmentation threshold, and also the global origin when set.
// Limits admit input mask, axes, halo cache, output, both histograms per worker
// and worst-case pair/histogram visits before allocation. They are not RSS.
type LocalTextureOptions struct {
	Feature                    LocalTextureFeature
	RadiusMM                   [3]float64
	BinWidth                   float64
	LowerBound                 *float64
	Quantization               LocalTextureQuantization
	Aggregation                TextureAggregation
	Directions                 [][3]int
	MaxGreyLevels, Workers     int
	MaxWorkingBytes, MaxVisits int64
}

type LocalTexturePlan struct {
	Region, Halo                   LocalTextureRegion
	Dimensions, RadiusVoxels       [3]int
	Workers                        int
	ReservedBytes, WorstCaseVisits int64
	Options                        LocalTextureOptions
}

// LocalTextureMap owns only requested-region float32 values. NaN means outside
// the mask, non-finite/resegmented centre or no pairs, never a quantitative zero.
type LocalTextureMap struct {
	Plan          LocalTexturePlan
	Values        []float32
	GlobalMinimum float64
}

func (o LocalTextureOptions) normalized() (LocalTextureOptions, error) {
	if o.Feature == "" {
		o.Feature = LocalContrast
	}
	if o.Quantization == "" {
		o.Quantization = LocalTextureGlobal
	}
	if o.MaxGreyLevels == 0 {
		o.MaxGreyLevels = 16
	}
	if o.Workers == 0 {
		o.Workers = min(16, parallel.DefaultWorkers())
	}
	if o.MaxWorkingBytes == 0 {
		o.MaxWorkingBytes = LocalTextureMaxBytes
	}
	if o.MaxVisits == 0 {
		o.MaxVisits = LocalTextureMaxVisits
	}
	if o.Feature != LocalContrast && o.Feature != LocalEntropy && o.Feature != LocalEnergy && o.Feature != LocalHomogeneity ||
		!finite(o.BinWidth) || o.BinWidth <= 0 || o.LowerBound != nil && !finite(*o.LowerBound) ||
		o.Quantization != LocalTextureGlobal && o.Quantization != LocalTextureWindow ||
		o.Aggregation != TextureAveraged && o.Aggregation != TextureMerged {
		return o, ErrInvalidTexture
	}
	if o.MaxGreyLevels < 1 || o.MaxGreyLevels > 256 || o.Workers < 1 || o.Workers > 16 || o.MaxWorkingBytes < 1 || o.MaxWorkingBytes > LocalTextureMaxBytes || o.MaxVisits < 1 || o.MaxVisits > LocalTextureMaxVisits {
		return o, ErrLocalTextureLimit
	}
	if o.LowerBound != nil {
		v := *o.LowerBound
		o.LowerBound = &v
	}
	dirs := o.Directions
	if len(dirs) == 0 {
		dirs = textureDirections[:]
	}
	if len(dirs) > 13 {
		return o, ErrLocalTextureLimit
	}
	o.Directions = make([][3]int, 0, len(dirs))
	seen := map[[3]int]bool{}
	for _, d := range dirs {
		last := -1
		for a, v := range d {
			if v < -32 || v > 32 {
				return o, ErrInvalidTexture
			}
			if v != 0 {
				last = a
			}
		}
		if last < 0 {
			return o, ErrInvalidTexture
		}
		if d[last] < 0 {
			for a := range d {
				d[a] = -d[a]
			}
		}
		if seen[d] {
			return o, fmt.Errorf("%w: duplicate symmetric direction", ErrInvalidTexture)
		}
		seen[d] = true
		o.Directions = append(o.Directions, d)
	}
	return o, nil
}

// PlanLocalTexture admits the request without invoking the intensity accessor
// or allocating input/output images. Axes must be finite, regular, increasing.
func (m *VolumeMask) PlanLocalTexture(region *LocalTextureRegion, options LocalTextureOptions) (LocalTexturePlan, error) {
	o, err := options.normalized()
	if err != nil {
		return LocalTexturePlan{}, err
	}
	if m == nil {
		return LocalTexturePlan{}, ErrInvalidVolumeMask
	}
	p := LocalTexturePlan{Options: o, Dimensions: [3]int{m.Columns, m.Rows, m.Slices}, Workers: o.Workers}
	n := int64(1)
	for _, d := range p.Dimensions {
		if d < 1 || d > LocalTextureMaxVoxels || n > LocalTextureMaxVoxels/int64(d) {
			return p, ErrLocalTextureLimit
		}
		n *= int64(d)
	}
	if int64(len(m.Voxels)) != n {
		return p, ErrInvalidVolumeMask
	}
	p.Region.Max = p.Dimensions
	if region != nil {
		p.Region = *region
	}
	window, halo, out := int64(1), int64(1), int64(1)
	for a, axis := range [][]float64{m.X, m.Y, m.Z} {
		d := p.Dimensions[a]
		if len(axis) != d || !finite(o.RadiusMM[a]) || o.RadiusMM[a] < 0 || o.RadiusMM[a] > 32 {
			return p, ErrInvalidTexture
		}
		step := 1.0
		if d > 1 {
			step = axis[1] - axis[0]
			if !finite(step) || step <= 0 {
				return p, ErrInvalidVolumeMask
			}
		}
		for i, v := range axis {
			if !finite(v) || i > 0 && math.Abs(v-axis[i-1]-step) > 1e-6*step {
				return p, fmt.Errorf("%w: irregular axis", ErrInvalidVolumeMask)
			}
		}
		if d > 1 {
			r := math.Floor(o.RadiusMM[a]/step + 1e-10)
			if !finite(r) || r > 32 {
				return p, ErrLocalTextureLimit
			}
			p.RadiusVoxels[a] = int(r)
		}
		lo, hi := p.Region.Min[a], p.Region.Max[a]
		if lo < 0 || hi <= lo || hi > d {
			return p, ErrInvalidTexture
		}
		p.Halo.Min[a] = max(0, lo-p.RadiusVoxels[a])
		p.Halo.Max[a] = min(d, hi+p.RadiusVoxels[a])
		window *= int64(min(d, 2*p.RadiusVoxels[a]+1))
		halo *= int64(p.Halo.Max[a] - p.Halo.Min[a])
		out *= int64(hi - lo)
	}
	p.Workers = min(p.Workers, int(out))
	bins := int64(o.MaxGreyLevels)
	p.ReservedBytes = n + 8*int64(m.Columns+m.Rows+m.Slices) + 8*halo + 4*out + int64(p.Workers)*(16*bins*bins+4096) + 65536
	// Includes clearing and reducing all directional histograms, even empty ones.
	p.WorstCaseVisits = 2*n + out*(window+int64(len(o.Directions))*(window+3*bins*bins))
	if p.ReservedBytes > o.MaxWorkingBytes || p.WorstCaseVisits > o.MaxVisits {
		return p, ErrLocalTextureLimit
	}
	return p, nil
}

// LocalTexture computes regional or whole-image GLCM values with identical
// source-window semantics. The accessor is invoked serially while snapshotting
// the admitted halo. Global quantization scans the entire mask for its origin
// even on a regional request. Caller keeps input/accessor stable until return.
// Every error returns nil; no incomplete map or mutation is published.
func (m *VolumeMask) LocalTexture(ctx context.Context, valueAt VoxelValueFunc, region *LocalTextureRegion, options LocalTextureOptions) (*LocalTextureMap, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if valueAt == nil {
		return nil, ErrInvalidTexture
	}
	p, err := m.PlanLocalTexture(region, options)
	if err != nil {
		return nil, err
	}
	o := p.Options
	hd := [3]int{p.Halo.Max[0] - p.Halo.Min[0], p.Halo.Max[1] - p.Halo.Min[1], p.Halo.Max[2] - p.Halo.Min[2]}
	cache := make([]float64, hd[0]*hd[1]*hd[2])
	for i := range cache {
		cache[i] = math.NaN()
	}
	minimum, maximum := math.Inf(1), math.Inf(-1)
	for z := 0; z < m.Slices; z++ {
		for y := 0; y < m.Rows; y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := 0; x < m.Columns; x++ {
				inHalo := x >= p.Halo.Min[0] && x < p.Halo.Max[0] && y >= p.Halo.Min[1] && y < p.Halo.Max[1] && z >= p.Halo.Min[2] && z < p.Halo.Max[2]
				if !m.Voxels[(z*m.Rows+y)*m.Columns+x] || !inHalo && o.Quantization == LocalTextureWindow {
					continue
				}
				v, ok := valueAt(x, y, z)
				if !ok || !finite(v) || o.LowerBound != nil && v < *o.LowerBound {
					continue
				}
				minimum = math.Min(minimum, v)
				maximum = math.Max(maximum, v)
				if inHalo {
					cache[((z-p.Halo.Min[2])*hd[1]+y-p.Halo.Min[1])*hd[0]+x-p.Halo.Min[0]] = v
				}
			}
		}
	}
	base := minimum
	if o.Quantization == LocalTextureGlobal && o.LowerBound != nil {
		base = *o.LowerBound
	}
	if o.Quantization == LocalTextureGlobal && finite(maximum) && math.Floor((maximum-base)/o.BinWidth) >= float64(o.MaxGreyLevels) {
		return nil, fmt.Errorf("%w: increase bin width", ErrLocalTextureLimit)
	}
	d := [3]int{p.Region.Max[0] - p.Region.Min[0], p.Region.Max[1] - p.Region.Min[1], p.Region.Max[2] - p.Region.Min[2]}
	r := &LocalTextureMap{Plan: p, Values: make([]float32, d[0]*d[1]*d[2]), GlobalMinimum: base}
	if o.Quantization == LocalTextureWindow {
		r.GlobalMinimum = math.NaN()
	}
	for i := range r.Values {
		r.Values[i] = float32(math.NaN())
	}
	at := func(x, y, z int) float64 {
		return cache[((z-p.Halo.Min[2])*hd[1]+y-p.Halo.Min[1])*hd[0]+x-p.Halo.Min[0]]
	}
	workctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var failure error
	fail := func(e error) {
		mu.Lock()
		if failure == nil {
			failure = e
		}
		mu.Unlock()
		cancel()
	}
	err = parallel.Do(workctx, p.Workers, p.Workers, func(worker int) {
		bins := o.MaxGreyLevels
		hist, merged := make([]float64, bins*bins), make([]float64, bins*bins)
		for index := worker; index < len(r.Values); index += p.Workers {
			if workctx.Err() != nil {
				return
			}
			x, y, z := index%d[0]+p.Region.Min[0], index/d[0]%d[1]+p.Region.Min[1], index/(d[0]*d[1])+p.Region.Min[2]
			if !finite(at(x, y, z)) {
				continue
			}
			lo, hi := [3]int{}, [3]int{}
			for a, c := range [3]int{x, y, z} {
				lo[a] = max(0, c-p.RadiusVoxels[a])
				hi[a] = min(p.Dimensions[a], c+p.RadiusVoxels[a]+1)
			}
			origin := base
			if o.Quantization == LocalTextureWindow {
				origin = math.Inf(1)
				windowMaximum := math.Inf(-1)
				for k := lo[2]; k < hi[2]; k++ {
					for j := lo[1]; j < hi[1]; j++ {
						for i := lo[0]; i < hi[0]; i++ {
							if v := at(i, j, k); finite(v) {
								origin = math.Min(origin, v)
								windowMaximum = math.Max(windowMaximum, v)
							}
						}
					}
				}
				if math.Floor((windowMaximum-origin)/o.BinWidth) >= float64(bins) {
					fail(fmt.Errorf("%w: increase window bin width", ErrLocalTextureLimit))
					return
				}
			}
			level := func(v float64) int {
				if !finite(v) {
					return -1
				}
				b := math.Floor((v - origin) / o.BinWidth)
				if !finite(b) || b < 0 || b >= float64(bins) {
					fail(fmt.Errorf("%w: window grey levels exceed limit", ErrLocalTextureLimit))
					return -1
				}
				return int(b)
			}
			clear(merged)
			total, directions, sum := 0., 0, 0.
			for _, dir := range o.Directions {
				clear(hist)
				pairs := 0.
				for k := lo[2]; k < hi[2]; k++ {
					for j := lo[1]; j < hi[1]; j++ {
						if workctx.Err() != nil {
							return
						}
						for i := lo[0]; i < hi[0]; i++ {
							u, v, w := i+dir[0], j+dir[1], k+dir[2]
							if u < lo[0] || u >= hi[0] || v < lo[1] || v >= hi[1] || w < lo[2] || w >= hi[2] {
								continue
							}
							a, b := level(at(i, j, k)), level(at(u, v, w))
							if a < 0 || b < 0 {
								continue
							}
							hist[a*bins+b]++
							hist[b*bins+a]++
							pairs += 2
						}
					}
				}
				if pairs == 0 {
					continue
				}
				directions++
				if o.Aggregation == TextureAveraged {
					sum += localTextureFeature(hist, pairs, bins, o.Feature)
				} else {
					total += pairs
					for i, v := range hist {
						merged[i] += v
					}
				}
			}
			if directions > 0 {
				if o.Aggregation == TextureAveraged {
					r.Values[index] = float32(sum / float64(directions))
				} else {
					r.Values[index] = float32(localTextureFeature(merged, total, bins, o.Feature))
				}
			}
		}
	})
	if failure != nil {
		return nil, failure
	}
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return r, nil
}

func localTextureFeature(hist []float64, total float64, bins int, feature LocalTextureFeature) float64 {
	sum := 0.
	for i, c := range hist {
		if c == 0 {
			continue
		}
		p := c / total
		delta := float64(i/bins - i%bins)
		switch feature {
		case LocalContrast:
			sum += p * delta * delta
		case LocalEntropy:
			sum -= p * math.Log2(p)
		case LocalEnergy:
			sum += p * p
		case LocalHomogeneity:
			sum += p / (1 + delta*delta)
		}
	}
	return sum
}
