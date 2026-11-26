package registration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

var (
	ErrSyN              = errors.New("registration: invalid symmetric normalization")
	ErrSyNResourceLimit = errors.New("registration: symmetric normalization resource limit")
	ErrSyNFolding       = errors.New("registration: symmetric normalization folding or unsupported gradient")
)

type SyNLevel struct {
	Shrink           int
	Iterations       int
	SmoothingSigmaMM float64
}

// SyNOptions controls greedy symmetric normalization with local correlation.
// Levels are coarsest first and must end at shrink 1. Zero Levels selects one
// full-resolution level with 50 iterations. Correlation requires an explicit
// voxel or physical neighborhood radius. Input images/masks remain borrowed.
type SyNOptions struct {
	Levels                []SyNLevel
	Correlation           NeighborhoodCorrelationOptions
	FixedMask, MovingMask *Image
	LearningRateMM        float64 // zero: half the smallest fixed spacing
	UpdateSigmaMM         float64 // zero: no update smoothing
	// Regularization weights the mean physical membrane energy of both half
	// fields. Zero disables the penalty; local intensity agreement alone does
	// not resolve the scalar registration aperture problem.
	Regularization      float64
	MaximumHalfGradient float64 // zero: 0.3; sufficient continuous-cell bound <1
	MinimumImprovement  float64 // zero: 1e-8
	MaxBacktracks       int     // zero: 12
	MaxVoxels           int     // zero: 1,000,000 per source
	MaxWorkingBytes     int64   // zero: 256 MiB of live owned payload, excludes runtime
	MaxWork             int64   // zero: 10,000,000,000 neighborhood/parameter visit charge
	Inverse             spatialreg.InverseOptions
	Progress            func(SyNProgress)
}

// SyNProgress describes a committed iteration, not an anatomical acceptance.
type SyNProgress struct {
	Level, Iteration  int
	Value             float64
	Energy            float64
	EligibleCenters   int
	StepMM            float64
	HalfGradientBound float64
	Stop              string
}

// SyNResult is a reviewable proposal. MiddleToFixed/MiddleToMoving share the
// fixed image's virtual grid. FixedToMoving = MiddleToMoving o inverse(MiddleToFixed)
// and MovingToFixed uses the opposite composition, on their respective grids.
// Undefined vectors are all-NaN, which the existing DICOM grid writer retains;
// validity masks identify them without pretending identity outside the domain.
type SyNResult struct {
	MiddleToFixed, MiddleToMoving                *DisplacementField
	FixedToMoving, MovingToFixed                 *DisplacementField
	ForwardValid, InverseValid                   []bool
	Levels                                       []SyNProgress
	EstimatedWorkingBytes                        int64
	MaximumInverseResidualMM                     float64
	ForwardGradientBound, InverseGradientBound   float64
	ForwardCertifiedCells, InverseCertifiedCells int
}

// SyN is an independent greedy halfway registration, using both images'
// complete local-correlation derivatives, physical Gaussian update smoothing,
// exponential composition, descent backtracking and conservative cell-gradient
// admission. It does not reproduce ANTs' center-only gradient or trajectory.
// Failure/cancellation returns no proposal and joins reused field helpers.
func SyN(ctx context.Context, fixed, moving *Image, options SyNOptions) (SyNResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SyNResult{}, err
	}
	o, estimated, err := prepareSyN(ctx, fixed, moving, options)
	if err != nil {
		return SyNResult{}, err
	}
	var hf, hm *DisplacementField
	result := SyNResult{EstimatedWorkingBytes: estimated}
	for level, l := range o.Levels {
		if err := ctx.Err(); err != nil {
			return SyNResult{}, err
		}
		lf, lm, err := (Method{Fixed: fixed, Moving: moving}).smooth(ctx, l.SmoothingSigmaMM)
		if err != nil {
			return SyNResult{}, err
		}
		factors := func(im *Image) [3]int {
			return [3]int{min(l.Shrink, im.size[0]), min(l.Shrink, im.size[1]), min(l.Shrink, im.size[2])}
		}
		lf, err = lf.Downsample(factors(lf))
		if err != nil {
			return SyNResult{}, err
		}
		lm, err = lm.Downsample(factors(lm))
		if err != nil {
			return SyNResult{}, err
		}
		hf, err = synRefine(ctx, hf, lf)
		if err != nil {
			return SyNResult{}, err
		}
		hm, err = synRefine(ctx, hm, lf)
		if err != nil {
			return SyNResult{}, err
		}
		config, err := NewNeighborhoodCorrelation(lf, lm, MetricDomain{}, o.Correlation)
		if err != nil {
			return SyNResult{}, err
		}
		last, err := synLevel(ctx, lf, lm, hf, hm, config, level, l.Iterations, o)
		if err != nil {
			return SyNResult{}, err
		}
		result.Levels = append(result.Levels, last)
	}
	if err := ctx.Err(); err != nil {
		return SyNResult{}, err
	}
	result.MiddleToFixed, result.MiddleToMoving = hf, hm
	result.FixedToMoving, result.ForwardValid, result.MaximumInverseResidualMM, err = synComposeInverse(ctx, hf, hm, fixed, moving, o.Inverse)
	if err != nil {
		return SyNResult{}, err
	}
	var residual float64
	result.MovingToFixed, result.InverseValid, residual, err = synComposeInverse(ctx, hm, hf, moving, fixed, o.Inverse)
	if err != nil {
		return SyNResult{}, err
	}
	result.MaximumInverseResidualMM = math.Max(result.MaximumInverseResidualMM, residual)
	result.ForwardGradientBound, result.ForwardCertifiedCells, err = synGradientCertificate(ctx, result.FixedToMoving)
	if err != nil {
		return SyNResult{}, err
	}
	result.InverseGradientBound, result.InverseCertifiedCells, err = synGradientCertificate(ctx, result.MovingToFixed)
	if err != nil {
		return SyNResult{}, err
	}
	if result.ForwardCertifiedCells == 0 || result.InverseCertifiedCells == 0 {
		return SyNResult{}, ErrTooFewSamples
	}
	if !finite(result.ForwardGradientBound) || !finite(result.InverseGradientBound) || result.ForwardGradientBound >= 1 || result.InverseGradientBound >= 1 {
		return SyNResult{}, ErrSyNFolding
	}
	if err := ctx.Err(); err != nil {
		return SyNResult{}, err
	}
	return result, nil
}

func prepareSyN(ctx context.Context, fixed, moving *Image, o SyNOptions) (SyNOptions, int64, error) {
	if fixed == nil || moving == nil {
		return o, 0, ErrSyN
	}
	if o.MaxVoxels < 0 || o.MaxWorkingBytes < 0 || o.MaxWork < 0 || o.MaxBacktracks < 0 {
		return o, 0, ErrSyN
	}
	if o.MaxVoxels == 0 {
		o.MaxVoxels = 1000000
	}
	if o.MaxWorkingBytes == 0 {
		o.MaxWorkingBytes = 256 << 20
	}
	if o.MaxWork == 0 {
		o.MaxWork = 10000000000
	}
	if o.MaxBacktracks == 0 {
		o.MaxBacktracks = 12
	}
	if o.MaxBacktracks > 20 || len(o.Levels) > 16 {
		return o, 0, ErrSyNResourceLimit
	}
	if len(o.Levels) == 0 {
		o.Levels = []SyNLevel{{Shrink: 1, Iterations: 50}}
	} else {
		o.Levels = append([]SyNLevel(nil), o.Levels...)
	}
	if o.LearningRateMM == 0 {
		o.LearningRateMM = .5 * math.Min(fixed.spacing.X, math.Min(fixed.spacing.Y, fixed.spacing.Z))
	}
	if o.MaximumHalfGradient == 0 {
		o.MaximumHalfGradient = .3
	}
	if o.MinimumImprovement == 0 {
		o.MinimumImprovement = 1e-8
	}
	if !finite(o.LearningRateMM) || o.LearningRateMM <= 0 || !finite(o.UpdateSigmaMM) || o.UpdateSigmaMM < 0 || !finite(o.Regularization) || o.Regularization < 0 || !finite(o.MaximumHalfGradient) || o.MaximumHalfGradient <= 0 || o.MaximumHalfGradient >= 1 || !finite(o.MinimumImprovement) || o.MinimumImprovement < 0 {
		return o, 0, ErrSyN
	}
	if o.Inverse.MaximumIterations < 0 || !finite(o.Inverse.Tolerance) || o.Inverse.Tolerance < 0 || !finite(o.Inverse.MaxResidual) || o.Inverse.MaxResidual < 0 {
		return o, 0, ErrSyN
	}
	if o.Inverse.MaximumIterations == 0 {
		o.Inverse.MaximumIterations = 50
	}
	if o.Inverse.MaximumIterations > 1000 {
		return o, 0, ErrSyNResourceLimit
	}
	if o.Inverse.Tolerance == 0 {
		o.Inverse.Tolerance = 1e-5
	}
	if o.Inverse.MaxResidual == 0 {
		o.Inverse.MaxResidual = .001
	}
	if o.Inverse.Tolerance > o.Inverse.MaxResidual {
		return o, 0, ErrSyN
	}
	for _, sp := range []float64{fixed.spacing.X, fixed.spacing.Y, fixed.spacing.Z} {
		if !finite(o.UpdateSigmaMM/sp) || o.UpdateSigmaMM/sp > 10 {
			return o, 0, ErrSyNResourceLimit
		}
	}
	var biggest int64
	for _, im := range []*Image{fixed, moving, o.FixedMask, o.MovingMask} {
		if im == nil {
			continue
		}
		if len(im.data) > o.MaxVoxels {
			return o, 0, ErrSyNResourceLimit
		}
		if !im.direction.Finite() {
			return o, 0, ErrSyN
		}
		gram := im.direction.Transpose().Mul(im.direction)
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				want := 0.0
				if r == c {
					want = 1
				}
				if math.Abs(gram[r][c]-want) > 1e-8 {
					return o, 0, ErrSyN
				}
			}
		}
		if im.direction.Determinant() <= 0 {
			return o, 0, ErrSyN
		}
		biggest = max(biggest, int64(len(im.data)))
		for i, v := range im.data {
			if i%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return o, 0, err
				}
			}
			if !finite(float64(v)) {
				return o, 0, ErrSyN
			}
		}
	}
	config, err := NewNeighborhoodCorrelation(fixed, moving, MetricDomain{}, o.Correlation)
	if err != nil {
		return o, 0, err
	}
	if biggest > o.MaxWorkingBytes/640 {
		return o, 0, ErrSyNResourceLimit
	}
	estimated := biggest * 640
	if int64(config.neighborhood) > (o.MaxWorkingBytes-estimated)/80 {
		return o, 0, ErrSyNResourceLimit
	}
	estimated += int64(config.neighborhood) * 80
	var work int64
	previous := int(^uint(0) >> 1)
	for i, l := range o.Levels {
		if l.Shrink < 1 || l.Shrink > previous || l.Iterations < 1 || l.Iterations > 500 || !finite(l.SmoothingSigmaMM) || l.SmoothingSigmaMM < 0 {
			return o, 0, ErrSyN
		}
		previous = l.Shrink
		if i == len(o.Levels)-1 && l.Shrink != 1 {
			return o, 0, ErrSyN
		}
		count := int64(1)
		for _, size := range fixed.size {
			count *= int64((size + min(l.Shrink, size) - 1) / min(l.Shrink, size))
		}
		// Bound gradient, line searches, three-axis smoothing and field/inverse
		// work with a conservative fixed per-sample charge. No claim of CPU cycles.
		charge := int64(config.neighborhood)*(int64(o.MaxBacktracks)+2) + 512 + int64(o.Inverse.MaximumIterations)*64
		if count > o.MaxWork/charge || count*charge > (o.MaxWork-work)/int64(l.Iterations) {
			return o, 0, ErrSyNResourceLimit
		}
		work += count * charge * int64(l.Iterations)
	}
	return o, estimated, nil
}

func synRefine(ctx context.Context, previous *DisplacementField, grid *Image) (*DisplacementField, error) {
	next := newField(grid)
	if previous == nil {
		return next, nil
	}
	g, err := previous.grid()
	if err != nil {
		return nil, err
	}
	for i := range next.Vectors {
		if i%512 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		p := synPoint(grid, i)
		next.Vectors[i] = sampleVectors(previous.Vectors, previous.Size, g.ContinuousIndex(p))
	}
	synZeroBoundary(next.Vectors, next.Size)
	return next, nil
}

func synPoint(grid *Image, i int) render.Vec3 {
	return grid.Point(render.Vec3{X: float64(i % grid.size[0]), Y: float64(i / grid.size[0] % grid.size[1]), Z: float64(i / (grid.size[0] * grid.size[1]))})
}

type synWarped struct {
	fixed, moving                 []float64
	valid                         []bool
	fixedGradient, movingGradient []render.Vec3
}

func synWarp(ctx context.Context, fixed, moving *Image, hf, hm *DisplacementField, o SyNOptions, gradients bool) (synWarped, error) {
	grid, _ := hf.grid()
	n := len(hf.Vectors)
	w := synWarped{fixed: make([]float64, n), moving: make([]float64, n), valid: make([]bool, n)}
	if gradients {
		w.fixedGradient = make([]render.Vec3, n)
		w.movingGradient = make([]render.Vec3, n)
	}
	tf, tm := DisplacementFieldTransform{field: *hf, grid: grid}, DisplacementFieldTransform{field: *hm, grid: grid}
	var failure synFailure
	err := synVoxels(ctx, n, func(i int) {
		if failure.after(i) {
			return
		}
		p := synPoint(&grid, i)
		pf, pm := p.Add(hf.Vectors[i]), p.Add(hm.Vectors[i])
		fi, mi := fixed.ContinuousIndex(pf), moving.ContinuousIndex(pm)
		if !fixed.inside(fi) || !moving.inside(mi) {
			return
		}
		fv, err := neighborhoodMaskAt(o.FixedMask, pf)
		if err != nil {
			failure.record(i, err)
			return
		}
		mv, err := neighborhoodMaskAt(o.MovingMask, pm)
		if err != nil {
			failure.record(i, err)
			return
		}
		if !fv || !mv {
			return
		}
		w.fixed[i], w.moving[i], w.valid[i] = fixed.linearAt(fi), moving.linearAt(mi), true
		if gradients {
			w.fixedGradient[i] = tf.JacobianWithRespectToPosition(p).Transpose().MulVec(fixed.gradientAt(fi))
			w.movingGradient[i] = tm.JacobianWithRespectToPosition(p).Transpose().MulVec(moving.gradientAt(mi))
			if !finiteVec(w.fixedGradient[i]) || !finiteVec(w.movingGradient[i]) {
				failure.record(i, ErrSyN)
			}
		}
	})
	if err != nil {
		return synWarped{}, err
	}
	if err := failure.err(); err != nil {
		return synWarped{}, err
	}
	return w, nil
}

// synVoxels runs fn for every voxel index in [0, n) on the calling goroutine
// plus helpers from the module's shared concurrency budget, which claim
// contiguous runs of voxels. fn must write only its own voxel's outputs.
func synVoxels(ctx context.Context, n int, fn func(int)) error {
	return parallel.Run(ctx, n, 0, 0, func() func(int) { return fn })
}

// synFailure keeps the error of the lowest failing voxel, the one a serial
// scan would report, whatever order the workers reach the voxels in.
type synFailure struct {
	mu    sync.Mutex
	at    atomic.Int64 // lowest failing voxel + 1; zero when none failed
	first error
}

// after reports whether a lower voxel already failed, so i need not run.
func (f *synFailure) after(i int) bool {
	at := f.at.Load()
	return at != 0 && int64(i) >= at-1
}

func (f *synFailure) record(i int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at := f.at.Load(); at == 0 || int64(i) < at-1 {
		f.at.Store(int64(i) + 1)
		f.first = err
	}
}

func (f *synFailure) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.first
}

// synCorrelation is the mean negated squared local correlation over the
// eligible centres and, with derivatives, its derivatives with respect to
// the warped fixed and moving intensities. Centres are evaluated in parallel
// into per-centre arrays; the sum then runs serially in centre order, and
// each voxel gathers the derivative terms of the centres whose neighbourhood
// holds it in increasing centre order, the order in which a serial scatter
// from each centre adds them. The result does not depend on the workers.
func synCorrelation(ctx context.Context, w synWarped, size, radius [3]int, variance float64, derivatives bool) (float64, []float64, []float64, int, error) {
	n := len(w.fixed)
	nhood := (2*radius[0] + 1) * (2*radius[1] + 1) * (2*radius[2] + 1)
	// rho2 is an eligible centre's squared correlation.
	rho2, eligibleAt := make([]float64, n), make([]bool, n)
	var centres synCentres
	if derivatives {
		centres = newSynCentres(n)
	}
	err := parallel.Run(ctx, n, 0, 0, func() func(int) {
		indices := make([]int, 0, nhood)
		return func(i int) {
			if !w.valid[i] {
				return
			}
			x, y, z := i%size[0], i/size[0]%size[1], i/(size[0]*size[1])
			indices = indices[:0]
			var meanF, meanM float64
			for k := max(0, z-radius[2]); k <= min(size[2]-1, z+radius[2]); k++ {
				for j := max(0, y-radius[1]); j <= min(size[1]-1, y+radius[1]); j++ {
					for h := max(0, x-radius[0]); h <= min(size[0]-1, x+radius[0]); h++ {
						at := (k*size[1]+j)*size[0] + h
						if !w.valid[at] {
							continue
						}
						indices = append(indices, at)
						n := float64(len(indices))
						meanF += (w.fixed[at] - meanF) / n
						meanM += (w.moving[at] - meanM) / n
					}
				}
			}
			if len(indices) < 2 {
				return
			}
			var ff, mm, fm float64
			for _, at := range indices {
				f, m := w.fixed[at]-meanF, w.moving[at]-meanM
				ff += f * f
				mm += m * m
				fm += f * m
			}
			n := float64(len(indices))
			if ff/n <= variance || mm/n <= variance {
				return
			}
			rho := (fm / math.Sqrt(ff)) / math.Sqrt(mm)
			rho2[i], eligibleAt[i] = rho*rho, true
			if derivatives {
				centres.meanF[i], centres.meanM[i] = meanF, meanM
				centres.sf[i], centres.sm[i] = -2*(fm/mm)/ff, -2*(fm/ff)/mm
				centres.cf[i], centres.cm[i] = fm/ff, fm/mm
			}
		}
	})
	if err != nil {
		return 0, nil, nil, 0, err
	}
	var sum float64
	eligible := 0
	for i, r := range rho2 {
		if eligibleAt[i] {
			sum -= r
			eligible++
		}
	}
	if eligible == 0 {
		return 0, nil, nil, 0, ErrTooFewSamples
	}
	if !derivatives {
		return sum / float64(eligible), nil, nil, eligible, nil
	}
	df, dm := make([]float64, n), make([]float64, n)
	count := float64(eligible)
	err = synVoxels(ctx, n, func(at int) {
		if !w.valid[at] {
			return
		}
		x, y, z := at%size[0], at/size[0]%size[1], at/(size[0]*size[1])
		var sumF, sumM float64
		for k := max(0, z-radius[2]); k <= min(size[2]-1, z+radius[2]); k++ {
			for j := max(0, y-radius[1]); j <= min(size[1]-1, y+radius[1]); j++ {
				for h := max(0, x-radius[0]); h <= min(size[0]-1, x+radius[0]); h++ {
					c := (k*size[1]+j)*size[0] + h
					if !eligibleAt[c] {
						continue
					}
					f, m := w.fixed[at]-centres.meanF[c], w.moving[at]-centres.meanM[c]
					sumF += centres.sf[c] * (m - centres.cf[c]*f)
					sumM += centres.sm[c] * (f - centres.cm[c]*m)
				}
			}
		}
		df[at], dm[at] = sumF/count, sumM/count
	})
	if err != nil {
		return 0, nil, nil, 0, err
	}
	return sum / float64(eligible), df, dm, eligible, nil
}

// synCentres holds each eligible centre's local means and the coefficients
// of its derivative terms: sf·(m − cf·f) for the fixed and sm·(f − cm·m)
// for the moving intensity of every voxel f, m of its neighbourhood.
type synCentres struct {
	meanF, meanM, sf, sm, cf, cm []float64
}

func newSynCentres(n int) synCentres {
	block := make([]float64, 6*n)
	return synCentres{meanF: block[:n], meanM: block[n : 2*n], sf: block[2*n : 3*n], sm: block[3*n : 4*n], cf: block[4*n : 5*n], cm: block[5*n:]}
}

func synLevel(ctx context.Context, fixed, moving *Image, hf, hm *DisplacementField, config *NeighborhoodCorrelation, level, iterations int, o SyNOptions) (SyNProgress, error) {
	last := SyNProgress{Level: level, Iteration: -1}
	// candidateF and candidateM receive each backtracking step's fields; an
	// accepted step trades them with the current fields.
	var candidateF, candidateM []render.Vec3
	for iteration := 0; iteration < iterations; iteration++ {
		w, err := synWarp(ctx, fixed, moving, hf, hm, o, true)
		if err != nil {
			return last, err
		}
		value, df, dm, centers, err := synCorrelation(ctx, w, hf.Size, config.radius, config.options.MinimumVariance, true)
		if err != nil {
			return last, err
		}
		uf, um := make([]render.Vec3, len(df)), make([]render.Vec3, len(dm))
		// With no regularization weight the membrane term contributes
		// nothing (its energy and gradient are finite for finite fields),
		// so it is not computed (#2025).
		var pf, pm float64
		var gf, gm []render.Vec3
		if o.Regularization != 0 {
			if pf, gf, err = synMembrane(ctx, hf, true); err != nil {
				return last, err
			}
			if pm, gm, err = synMembrane(ctx, hm, true); err != nil {
				return last, err
			}
		}
		energy := value + o.Regularization*(pf+pm)
		grid, _ := hf.grid()
		tf, tm := DisplacementFieldTransform{field: *hf, grid: grid}, DisplacementFieldTransform{field: *hm, grid: grid}
		if err := synVoxels(ctx, len(uf), func(i int) {
			uf[i] = w.fixedGradient[i].Scale(-df[i])
			um[i] = w.movingGradient[i].Scale(-dm[i])
			if o.Regularization != 0 {
				p := synPoint(&grid, i)
				uf[i] = uf[i].Sub(tf.JacobianWithRespectToPosition(p).Transpose().MulVec(gf[i]).Scale(o.Regularization))
				um[i] = um[i].Sub(tm.JacobianWithRespectToPosition(p).Transpose().MulVec(gm[i]).Scale(o.Regularization))
			}
		}); err != nil {
			return last, err
		}
		if err := synSmooth(ctx, uf, hf.Size, hf.Spacing, o.UpdateSigmaMM); err != nil {
			return last, err
		}
		if err := synSmooth(ctx, um, hm.Size, hm.Spacing, o.UpdateSigmaMM); err != nil {
			return last, err
		}
		synZeroBoundary(uf, hf.Size)
		synZeroBoundary(um, hm.Size)
		largest, err := synLargestStep(ctx, uf, um)
		if err != nil {
			return last, err
		}
		if !finite(largest) || !finite(energy) {
			return last, ErrSyN
		}
		last = SyNProgress{Level: level, Iteration: iteration, Value: value, Energy: energy, EligibleCenters: centers, Stop: "stationary"}
		if largest < 1e-15 {
			if o.Progress != nil {
				o.Progress(last)
			}
			if err := ctx.Err(); err != nil {
				return last, err
			}
			return last, nil
		}
		accepted := false
		// The scaled updates and the candidate fields are reused across the
		// backtracking steps; candidates are composed from the current
		// fields without copying them first (#2025).
		sf, sm := make([]render.Vec3, len(uf)), make([]render.Vec3, len(um))
		if len(candidateF) != len(hf.Vectors) {
			candidateF, candidateM = make([]render.Vec3, len(hf.Vectors)), make([]render.Vec3, len(hm.Vectors))
		}
		for attempt := 0; attempt < o.MaxBacktracks; attempt++ {
			if err := ctx.Err(); err != nil {
				return last, err
			}
			step := o.LearningRateMM / math.Pow(2, float64(attempt))
			if err := synVoxels(ctx, len(uf), func(i int) {
				sf[i] = uf[i].Scale(step / largest)
				sm[i] = um[i].Scale(step / largest)
			}); err != nil {
				return last, err
			}
			cf, cm := *hf, *hm
			cf.Vectors, cm.Vectors = candidateF, candidateM
			ef := exponentiate(sf, fixed)
			composeFieldInto(cf.Vectors, hf, ef, fixed)
			if err := ctx.Err(); err != nil {
				return last, err
			}
			em := exponentiate(sm, fixed)
			composeFieldInto(cm.Vectors, hm, em, fixed)
			for i := range cf.Vectors {
				if !finiteVec(cf.Vectors[i]) || !finiteVec(cm.Vectors[i]) {
					return last, ErrSyN
				}
			}
			bf, _, err := synGradientCertificate(ctx, &cf)
			if err != nil {
				return last, err
			}
			bm, _, err := synGradientCertificate(ctx, &cm)
			if err != nil {
				return last, err
			}
			bound := math.Max(bf, bm)
			if !finite(bound) || bound >= o.MaximumHalfGradient {
				continue
			}
			candidate, err := synWarp(ctx, fixed, moving, &cf, &cm, o, false)
			if err != nil {
				return last, err
			}
			next, _, _, _, err := synCorrelation(ctx, candidate, cf.Size, config.radius, config.options.MinimumVariance, false)
			if err != nil {
				return last, err
			}
			var pf, pm float64
			if o.Regularization != 0 {
				if pf, _, err = synMembrane(ctx, &cf, false); err != nil {
					return last, err
				}
				if pm, _, err = synMembrane(ctx, &cm, false); err != nil {
					return last, err
				}
			}
			nextEnergy := next + o.Regularization*(pf+pm)
			if !finite(nextEnergy) || nextEnergy >= energy-o.MinimumImprovement {
				continue
			}
			hf.Vectors, candidateF = candidateF, hf.Vectors
			hm.Vectors, candidateM = candidateM, hm.Vectors
			last.Value, last.StepMM, last.HalfGradientBound, last.Stop = next, step, bound, "iteration-limit"
			last.Energy = nextEnergy
			accepted = true
			break
		}
		if !accepted {
			last.Stop = "no-admissible-descent"
		}
		if o.Progress != nil {
			o.Progress(last)
		}
		if err := ctx.Err(); err != nil {
			return last, err
		}
		if !accepted {
			return last, nil
		}
	}
	return last, nil
}

func synMembrane(ctx context.Context, field *DisplacementField, derivatives bool) (float64, []render.Vec3, error) {
	var gradient []render.Vec3
	if derivatives {
		gradient = make([]render.Vec3, len(field.Vectors))
	}
	s := field.Size
	strides := [3]int{1, s[0], s[0] * s[1]}
	sp := [3]float64{field.Spacing.X, field.Spacing.Y, field.Spacing.Z}
	var energy float64
	edges := 0
	for i, v := range field.Vectors {
		if i%512 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, nil, err
			}
		}
		for a, stride := range strides {
			if i/stride%s[a] == s[a]-1 {
				continue
			}
			j := i + stride
			delta := field.Vectors[j].Sub(v)
			weight := 1 / (sp[a] * sp[a])
			energy += delta.Dot(delta) * weight
			edges++
			if derivatives {
				g := delta.Scale(2 * weight)
				gradient[i] = gradient[i].Sub(g)
				gradient[j] = gradient[j].Add(g)
			}
		}
	}
	if edges == 0 {
		return 0, gradient, nil
	}
	for i := range gradient {
		gradient[i] = gradient[i].Scale(1 / float64(edges))
	}
	return energy / float64(edges), gradient, nil
}

func synZeroBoundary(v []render.Vec3, size [3]int) {
	for i := range v {
		x, y, z := i%size[0], i/size[0]%size[1], i/(size[0]*size[1])
		if size[0] > 1 && (x == 0 || x == size[0]-1) || size[1] > 1 && (y == 0 || y == size[1]-1) || size[2] > 1 && (z == 0 || z == size[2]-1) {
			v[i] = render.Vec3{}
		}
	}
}

func synSmooth(ctx context.Context, v []render.Vec3, size [3]int, spacing render.Vec3, sigma float64) error {
	if sigma == 0 {
		return ctx.Err()
	}
	scratch := make([]render.Vec3, len(v))
	sp := [3]float64{spacing.X, spacing.Y, spacing.Z}
	strides := [3]int{1, size[0], size[0] * size[1]}
	for axis := 0; axis < 3; axis++ {
		if size[axis] < 2 {
			continue
		}
		kernel := gaussianKernel(sigma/sp[axis], .001, 16)
		radius := len(kernel) / 2
		if err := synVoxels(ctx, len(v), func(i int) {
			at := i / strides[axis] % size[axis]
			start := i - at*strides[axis]
			var sum render.Vec3
			for k, weight := range kernel {
				j := max(0, min(size[axis]-1, at+k-radius))
				sum = sum.Add(v[start+j*strides[axis]].Scale(weight))
			}
			scratch[i] = sum
		}); err != nil {
			return err
		}
		copy(v, scratch)
	}
	return ctx.Err()
}

// synLargestStep is the largest vector length of both updates. Each worker
// keeps its own maximum; a maximum does not depend on the order in which
// they are combined.
func synLargestStep(ctx context.Context, uf, um []render.Vec3) (float64, error) {
	var mu sync.Mutex
	var partials []*float64
	err := parallel.Run(ctx, len(uf), 0, 0, func() func(int) {
		local := new(float64)
		mu.Lock()
		partials = append(partials, local)
		mu.Unlock()
		return func(i int) { *local = math.Max(*local, math.Max(uf[i].Length(), um[i].Length())) }
	})
	largest := 0.0
	for _, local := range partials {
		largest = math.Max(largest, *local)
	}
	return largest, err
}

// Componentwise edge bounds dominate every bilinear derivative in a trilinear
// cell. Their Frobenius norm bounds the physical displacement gradient for an
// orthonormal grid. A bound <1 guarantees det(I+grad u)>0 throughout each cell.
func synGradientBound(field *DisplacementField) float64 {
	bound, _, err := synGradientCertificate(context.Background(), field)
	if err != nil {
		return math.NaN()
	}
	return bound
}

func synGradientCertificate(ctx context.Context, field *DisplacementField) (float64, int, error) {
	s := field.Size
	sp := [3]float64{field.Spacing.X, field.Spacing.Y, field.Spacing.Z}
	largest := 0.0
	cells := 0
	// Bound the inverse direction's spectral norm by the largest absolute row
	// sum of its Gram matrix, including tolerance-level nonorthogonality.
	inverse, err := field.Direction.Inverse()
	if err != nil {
		return 0, 0, err
	}
	gram := inverse.Transpose().Mul(inverse)
	directionBound := 0.0
	for _, row := range gram {
		sum := 0.0
		for _, v := range row {
			sum += math.Abs(v)
		}
		directionBound = math.Max(directionBound, sum)
	}
	directionBound = math.Sqrt(directionBound) * (1 + 1e-12)
	// Rows of cells run in parallel (#2025). Each worker keeps its own
	// largest bound and cell count; a maximum and a count do not depend on
	// the order they are combined in.
	type partial struct {
		largest float64
		cells   int
	}
	var mu sync.Mutex
	var partials []*partial
	rows := max(1, s[1]-1)
	err = parallel.Run(ctx, max(1, s[2]-1)*rows, 0, 0, func() func(int) {
		local := &partial{}
		mu.Lock()
		partials = append(partials, local)
		mu.Unlock()
		return func(row int) {
			z, y := row/rows, row%rows
			for x := 0; x < max(1, s[0]-1); x++ {
				valid := true
				for dz := 0; dz <= min(1, s[2]-1); dz++ {
					for dy := 0; dy <= min(1, s[1]-1); dy++ {
						for dx := 0; dx <= min(1, s[0]-1); dx++ {
							if !finiteVec(field.At(x+dx, y+dy, z+dz)) {
								valid = false
							}
						}
					}
				}
				if !valid {
					continue
				}
				local.cells++
				var bounds [3][3]float64
				for dz := 0; dz <= min(1, s[2]-1); dz++ {
					for dy := 0; dy <= min(1, s[1]-1); dy++ {
						for dx := 0; dx <= min(1, s[0]-1); dx++ {
							at := [3]int{x + dx, y + dy, z + dz}
							base := field.At(at[0], at[1], at[2])
							for a := 0; a < 3; a++ {
								if s[a] < 2 || at[a] != [3]int{x, y, z}[a] {
									continue
								}
								hi := at
								hi[a]++
								d := field.At(hi[0], hi[1], hi[2]).Sub(base).Scale(1 / sp[a])
								for c, v := range [3]float64{d.X, d.Y, d.Z} {
									bounds[c][a] = math.Max(bounds[c][a], math.Abs(v))
								}
							}
						}
					}
				}
				sum := 0.0
				for _, row := range bounds {
					for _, b := range row {
						sum += b * b
					}
				}
				local.largest = math.Max(local.largest, math.Sqrt(sum))
			}
		}
	})
	if err != nil {
		return 0, 0, err
	}
	for _, p := range partials {
		largest = math.Max(largest, p.largest)
		cells += p.cells
	}
	return largest * directionBound, cells, ctx.Err()
}

func synComposeInverse(ctx context.Context, half, other *DisplacementField, source, target *Image, options spatialreg.InverseOptions) (*DisplacementField, []bool, float64, error) {
	g, err := half.Grid()
	if err != nil {
		return nil, nil, 0, err
	}
	h, err := other.Grid()
	if err != nil {
		return nil, nil, 0, err
	}
	identity := spatialreg.Matrix{Values: render.GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}}
	r := spatialreg.Registration{Pre: identity, Post: identity, Grid: g}
	// The prepared inverter inverts the constant matrices once, not per
	// voxel, and serves concurrent queries (#2025).
	inverter, err := r.PrepareInverse(options)
	if err != nil {
		return nil, nil, 0, err
	}
	defer inverter.Close()
	field := newField(source)
	valid := make([]bool, len(field.Vectors))
	residual := make([]float64, len(field.Vectors))
	var failure synFailure
	err = synVoxels(ctx, len(field.Vectors), func(i int) {
		if failure.after(i) {
			return
		}
		p := synPoint(source, i)
		inverse, err := inverter.InvertPoint(ctx, p)
		if err != nil {
			if !errors.Is(err, spatialreg.ErrOutsideDomain) && !errors.Is(err, spatialreg.ErrInverseNotConverged) {
				failure.record(i, err)
				return
			}
			field.Vectors[i] = render.Vec3{X: math.NaN(), Y: math.NaN(), Z: math.NaN()}
			return
		}
		u, err := h.Sample(inverse.Registered)
		mapped := inverse.Registered.Add(u)
		if err != nil || !target.inside(target.ContinuousIndex(mapped)) {
			field.Vectors[i] = render.Vec3{X: math.NaN(), Y: math.NaN(), Z: math.NaN()}
			return
		}
		field.Vectors[i] = mapped.Sub(p)
		valid[i] = true
		residual[i] = inverse.Residual
	})
	if err != nil {
		return nil, nil, 0, err
	}
	if err := failure.err(); err != nil {
		return nil, nil, 0, err
	}
	maximum := 0.0
	for i, r := range residual {
		if valid[i] {
			maximum = math.Max(maximum, r)
		}
	}
	if !finite(maximum) {
		return nil, nil, 0, fmt.Errorf("%w: inverse residual", ErrSyN)
	}
	anyValid := false
	for _, v := range valid {
		anyValid = anyValid || v
	}
	if !anyValid {
		return nil, nil, 0, ErrTooFewSamples
	}
	return field, valid, maximum, ctx.Err()
}
