package roi

import (
	"context"
	"fmt"
	"math"
)

// ChanVeseOptions configures a scalar, two-phase regularized regional model.
// Phi is positive inside. No input mask or image callback is retained or changed.
type ChanVeseOptions struct {
	// Region is a fixed index box. Zero RegionSize selects the entire volume.
	// Statistics never read outside this box. Output masks use source coordinates.
	RegionOrigin, RegionSize [3]int
	Limits                   LevelSetLimits
	// Mu weights physical total variation of the smoothed membership, Nu its
	// physical area/volume; LambdaInside/Outside weight squared intensity errors.
	Mu, Nu, LambdaInside, LambdaOutside float64
	// EpsilonMM is atan-Heaviside smoothing in phi millimetres; GradientEpsilon
	// regularizes membership gradient magnitude in inverse millimetres.
	EpsilonMM, GradientEpsilon          float64
	MaxIterations, StableIterations     int
	MaximumRMSChangeMM, MaximumTimeStep float64
	// MaxWork bounds eight voxel passes per iteration/preparation plus twice
	// the supplied mask count and twice the relevant input runs plus crop rows.
	MaxWork  uint64
	Progress func(ChanVeseIteration)
}

func DefaultChanVeseOptions() ChanVeseOptions {
	return ChanVeseOptions{Mu: 0.2, LambdaInside: 1, LambdaOutside: 1, MaxIterations: 200, StableIterations: 10, MaximumRMSChangeMM: 1e-4, MaximumTimeStep: 1}
}

type ChanVeseIteration struct {
	Iteration                                              int
	Energy, InsideMean, OutsideMean, RMSChangeMM, TimeStep float64
	InsideVoxels, AvailableVoxels                          int
}

type ChanVeseResult struct {
	Masks                                        map[int]*RasterMask
	RegionOrigin, RegionSize                     [3]int
	History                                      []ChanVeseIteration
	Iterations                                   int
	StopReason                                   string // rms, stable-mask, empty-inside, empty-outside, iterations
	Converged                                    bool   // numerical criterion only, never clinical adequacy
	EstimatedPeakBytes                           uint64 // logical owned allocation ledger, not process RSS
	TouchesRegionBoundary, TouchesVolumeBoundary bool
}

type cvState struct {
	n                    int
	size, origin, stride [3]int
	spacing              [3]float64
	measure              float64
	image, phi, h, speed []float64
	flux                 [3][]float64
	available            []bool
}

func cvFinite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
func cvCheck(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return fmt.Errorf("%w: %w", ErrLevelSetCancelled, e)
	}
	return nil
}
func cvBoundAdd(a, b uint64) (uint64, bool) {
	if a > ^uint64(0)-b {
		return 0, false
	}
	return a + b, true
}
func cvBoundMul(a, b uint64) (uint64, bool) {
	if b != 0 && a > ^uint64(0)/b {
		return 0, false
	}
	return a * b, true
}

// ChanVese returns a detached research segmentation proposal. The available
// data and initial masks must remain stable during this call. Callback execution
// is synchronous; progress is invoked on the caller and may cancel the context.
// The discrete energy is not guaranteed to decrease for every explicit step.
func ChanVese(ctx context.Context, v LevelSetVolume, initial map[int]*RasterMask, o ChanVeseOptions) (*ChanVeseResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := cvCheck(ctx); e != nil {
		return nil, e
	}
	invalid := func(s string) (*ChanVeseResult, error) {
		return nil, fmt.Errorf("%w: Chan-Vese %s", ErrLevelSetInvalidInput, s)
	}
	sizes := [3]int{v.Columns, v.Rows, v.Slices}
	if v.ValueAt == nil {
		return invalid("missing image")
	}
	for _, n := range sizes {
		if n <= 0 {
			return invalid("size")
		}
	}
	for a, h := range v.SpacingMM {
		if a == 2 && v.Slices == 1 {
			continue
		}
		if !cvFinite(h) || h <= 0 {
			return invalid("spacing")
		}
	}
	if o.RegionSize == [3]int{} {
		if o.RegionOrigin != [3]int{} {
			return invalid("region origin without size")
		}
		o.RegionSize = sizes
	}
	for a, n := range o.RegionSize {
		if n <= 0 || o.RegionOrigin[a] < 0 || o.RegionOrigin[a] > sizes[a]-n {
			return invalid("region")
		}
	}
	if o.MaxIterations == 0 {
		o.MaxIterations = 200
	}
	if o.StableIterations == 0 {
		o.StableIterations = 10
	}
	if o.MaximumTimeStep == 0 {
		o.MaximumTimeStep = 1
	}
	if o.MaximumRMSChangeMM == 0 {
		o.MaximumRMSChangeMM = 1e-4
	}
	hmin := math.Min(v.SpacingMM[0], v.SpacingMM[1])
	if v.Slices > 1 {
		hmin = math.Min(hmin, v.SpacingMM[2])
	}
	if o.EpsilonMM == 0 {
		o.EpsilonMM = hmin
	}
	if o.GradientEpsilon == 0 {
		o.GradientEpsilon = 1e-6 / hmin
	}
	if o.LambdaInside == 0 && o.LambdaOutside == 0 {
		o.LambdaInside = 1
		o.LambdaOutside = 1
	}
	for _, x := range []float64{o.Mu, o.Nu, o.LambdaInside, o.LambdaOutside, o.EpsilonMM, o.GradientEpsilon, o.MaximumTimeStep, o.MaximumRMSChangeMM} {
		if !cvFinite(x) {
			return invalid("nonfinite parameter")
		}
	}
	if o.Mu < 0 || o.LambdaInside < 0 || o.LambdaOutside < 0 || o.EpsilonMM <= 0 || o.GradientEpsilon <= 0 || o.MaximumTimeStep <= 0 || o.MaximumRMSChangeMM < 0 || o.MaxIterations < 1 || o.MaxIterations > 100000 || o.StableIterations < 1 || o.StableIterations > 100000 {
		return invalid("parameter range")
	}
	limits, e := o.Limits.normalize()
	if e != nil {
		return nil, e
	}
	n := uint64(1)
	for _, x := range o.RegionSize {
		var ok bool
		n, ok = cvBoundMul(n, uint64(x))
		if !ok {
			return nil, ErrLevelSetResourceLimit
		}
	}
	if n > uint64(limits.MaxVoxels) {
		return nil, ErrLevelSetResourceLimit
	}
	work, ok := cvBoundMul(n, uint64(o.MaxIterations+1)*8)
	if o.MaxWork == 0 {
		o.MaxWork = 2_000_000_000
	}
	if !ok || work > o.MaxWork {
		return nil, ErrLevelSetResourceLimit
	}
	bytes, ok := cvBoundMul(n, 112)
	if !ok {
		return nil, ErrLevelSetResourceLimit
	}
	rowbytes, ok := cvBoundMul(uint64(v.Rows), uint64(o.RegionSize[2])*24)
	if !ok {
		return nil, ErrLevelSetResourceLimit
	}
	for _, extra := range []uint64{rowbytes, uint64(o.RegionSize[2]) * 128, uint64(o.MaxIterations+1) * 96, 4096} {
		bytes, ok = cvBoundAdd(bytes, extra)
		if !ok {
			return nil, ErrLevelSetResourceLimit
		}
	}
	if bytes > uint64(limits.MaxBytes) {
		return nil, ErrLevelSetResourceLimit
	}
	// Admission includes input run traversal, even for a narrow crop in a
	// much wider source row. Length accounting precedes scanning/allocation.
	inputWork, ok := cvBoundMul(uint64(len(initial)), 2)
	if !ok {
		return nil, ErrLevelSetResourceLimit
	}
	for z, m := range initial {
		if e := cvCheck(ctx); e != nil {
			return nil, e
		}
		if z < 0 || z >= v.Slices || m == nil || m.Columns != v.Columns || m.Rows != v.Rows || len(m.runs) != m.Rows {
			return invalid("mask grid/storage")
		}
		if z < o.RegionOrigin[2] || z >= o.RegionOrigin[2]+o.RegionSize[2] {
			continue
		}
		for y := o.RegionOrigin[1]; y < o.RegionOrigin[1]+o.RegionSize[1]; y++ {
			if e := cvCheck(ctx); e != nil {
				return nil, e
			}
			var added bool
			runWork, valid := cvBoundMul(uint64(len(m.runs[y])), 2)
			if !valid {
				return nil, ErrLevelSetResourceLimit
			}
			inputWork, added = cvBoundAdd(inputWork, runWork+1)
			if !added || inputWork > o.MaxWork {
				return nil, ErrLevelSetResourceLimit
			}
		}
	}
	work, ok = cvBoundAdd(work, inputWork)
	if !ok || work > o.MaxWork {
		return nil, ErrLevelSetResourceLimit
	}
	for z, m := range initial {
		if e := cvCheck(ctx); e != nil {
			return nil, e
		}
		if z < o.RegionOrigin[2] || z >= o.RegionOrigin[2]+o.RegionSize[2] {
			continue
		}
		for y := o.RegionOrigin[1]; y < o.RegionOrigin[1]+o.RegionSize[1]; y++ {
			end := 0
			for k, run := range m.runs[y] {
				if k%256 == 0 {
					if e := cvCheck(ctx); e != nil {
						return nil, e
					}
				}
				if run.Start < end || run.Start < 0 || run.End <= run.Start || run.End > v.Columns {
					return invalid("mask runs")
				}
				end = run.End
			}
		}
	}
	s := cvState{n: int(n), size: o.RegionSize, origin: o.RegionOrigin, spacing: v.SpacingMM}
	if v.Slices == 1 {
		s.spacing[2] = 1
	}
	s.measure = s.spacing[0] * s.spacing[1]
	if v.Slices > 1 {
		s.measure *= s.spacing[2]
	}
	if !cvFinite(s.measure) || s.measure <= 0 {
		return invalid("voxel measure")
	}
	s.stride = [3]int{1, s.size[0], s.size[0] * s.size[1]}
	s.image = make([]float64, s.n)
	s.phi = make([]float64, s.n)
	s.h = make([]float64, s.n)
	s.speed = make([]float64, s.n)
	s.available = make([]bool, s.n)
	for a := range s.flux {
		s.flux[a] = make([]float64, s.n)
	}
	inside, total := 0, 0
	for z := 0; z < s.size[2]; z++ {
		for y := 0; y < s.size[1]; y++ {
			var runs []MaskRun
			if m := initial[z+s.origin[2]]; m != nil {
				runs = m.runs[y+s.origin[1]]
			}
			cursor := 0
			for x := 0; x < s.size[0]; x++ {
				i := (z*s.size[1]+y)*s.size[0] + x
				if i%256 == 0 {
					if e := cvCheck(ctx); e != nil {
						return nil, e
					}
				}
				xx, yy, zz := x+s.origin[0], y+s.origin[1], z+s.origin[2]
				value, available := v.ValueAt(xx, yy, zz)
				if e := cvCheck(ctx); e != nil {
					return nil, e
				}
				if !available {
					continue
				}
				if !cvFinite(value) || math.Abs(value) > math.MaxFloat32 {
					return invalid("available value nonfinite/overflow")
				}
				s.available[i] = true
				s.image[i] = value
				s.phi[i] = -2 * o.EpsilonMM
				total++
				for cursor < len(runs) && runs[cursor].End <= xx {
					if cursor%256 == 0 {
						if e := cvCheck(ctx); e != nil {
							return nil, e
						}
					}
					cursor++
				}
				if cursor < len(runs) && runs[cursor].Start <= xx {
					s.phi[i] = 2 * o.EpsilonMM
					inside++
				}
			}
		}
	}
	if inside == 0 {
		return nil, fmt.Errorf("%w: Chan-Vese inside empty", ErrLevelSetEmpty)
	}
	if inside == total {
		return invalid("outside empty")
	}
	r := &ChanVeseResult{RegionOrigin: s.origin, RegionSize: s.size, EstimatedPeakBytes: bytes, History: make([]ChanVeseIteration, 0, o.MaxIterations+1), StopReason: "iterations"}
	stat, e := s.evaluate(ctx, o, true)
	if e != nil {
		return nil, e
	}
	r.History = append(r.History, stat)
	stable := 0
	for it := 1; it <= o.MaxIterations; it++ {
		maxspeed := 0.0
		for i := range s.phi {
			if i%256 == 0 {
				if e := cvCheck(ctx); e != nil {
					return nil, e
				}
			}
			if !s.available[i] {
				continue
			}
			d := s.phi[i] / o.EpsilonMM
			delta := 1 / (math.Pi * o.EpsilonMM * (1 + d*d))
			s.speed[i] *= delta
			if !cvFinite(s.speed[i]) {
				return invalid("evolution overflow")
			}
			maxspeed = math.Max(maxspeed, math.Abs(s.speed[i]))
		}
		dt := o.MaximumTimeStep
		if maxspeed > 0 {
			dt = math.Min(dt, .45*hmin/maxspeed)
		}
		change, changed := 0.0, 0
		for i := range s.phi {
			if i%256 == 0 {
				if e := cvCheck(ctx); e != nil {
					return nil, e
				}
			}
			if !s.available[i] {
				continue
			}
			old := s.phi[i]
			step := dt * s.speed[i]
			s.phi[i] += step
			if !cvFinite(s.phi[i]) {
				return invalid("level-set overflow")
			}
			change += step * step
			if (old >= 0) != (s.phi[i] >= 0) {
				changed++
			}
		}
		stat, e = s.evaluate(ctx, o, true)
		if e != nil {
			return nil, e
		}
		stat.Iteration = it
		stat.TimeStep = dt
		stat.RMSChangeMM = math.Sqrt(change / float64(total))
		r.History = append(r.History, stat)
		r.Iterations = it
		if o.Progress != nil {
			o.Progress(stat)
		}
		if e := cvCheck(ctx); e != nil {
			return nil, e
		}
		if changed == 0 {
			stable++
		} else {
			stable = 0
		}
		switch {
		case stat.InsideVoxels == 0:
			r.StopReason = "empty-inside"
		case stat.InsideVoxels == total:
			r.StopReason = "empty-outside"
		case stat.RMSChangeMM <= o.MaximumRMSChangeMM:
			r.StopReason = "rms"
			r.Converged = true
		case stable >= o.StableIterations && stat.RMSChangeMM <= 10*o.MaximumRMSChangeMM:
			r.StopReason = "stable-mask"
			r.Converged = true
		}
		if r.StopReason != "iterations" {
			break
		}
	}
	r.Masks = make(map[int]*RasterMask, s.size[2])
	for z := 0; z < s.size[2]; z++ {
		if e := cvCheck(ctx); e != nil {
			return nil, e
		}
		m := NewRasterMask(v.Columns, v.Rows)
		for y := 0; y < s.size[1]; y++ {
			if e := cvCheck(ctx); e != nil {
				return nil, e
			}
			start := -1
			for x := 0; x < s.size[0]; x++ {
				if x%256 == 0 {
					if e := cvCheck(ctx); e != nil {
						return nil, e
					}
				}
				i := (z*s.size[1]+y)*s.size[0] + x
				on := s.available[i] && s.phi[i] >= 0
				if on {
					if start < 0 {
						start = x + s.origin[0]
					}
					for a, c := range [3]int{x, y, z} {
						if !(a == 2 && v.Slices == 1) && (c == 0 || c == s.size[a]-1) {
							r.TouchesRegionBoundary = true
							if c+s.origin[a] == 0 || c+s.origin[a] == sizes[a]-1 {
								r.TouchesVolumeBoundary = true
							}
						}
					}
				}
				if start >= 0 && (!on || x+1 == s.size[0]) {
					end := x + s.origin[0]
					if on {
						end++
					}
					m.runs[y+s.origin[1]] = append(m.runs[y+s.origin[1]], MaskRun{Start: start, End: end})
					start = -1
				}
			}
		}
		r.Masks[z+s.origin[2]] = m
	}
	if e := cvCheck(ctx); e != nil {
		return nil, e
	}
	return r, nil
}
