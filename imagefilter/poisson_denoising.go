package imagefilter

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
)

var (
	ErrPoissonDenoisingInput   = errors.New("imagefilter: invalid Poisson denoising input")
	ErrPoissonDenoisingLimit   = errors.New("imagefilter: Poisson denoising resource limit")
	ErrPoissonDenoisingNumeric = errors.New("imagefilter: unsupported Poisson denoising arithmetic")
)

// PoissonDenoisingLimits bound admitted logical payload and work, not process RSS.
// Zero selects finite defaults. Callers may lower or raise limits up to hard ceilings.
type PoissonDenoisingLimits struct {
	MaxVoxels, MaxScratchBytes, MaxWorkingBytes, MaxNeighborVisits int64
	MaxIterations, MaxSourceReferences, MaxReferenceBytes          int
}

// PoissonDenoisingOptions selects a Poisson count-domain MAP estimator explicitly.
// No existing Gaussian-like NLM option or default is changed.
type PoissonDenoisingOptions struct {
	CountScale       float64  // mandatory counts per input unit, within [1e-12,1e12]
	Lambda           float64  // 0 is exact identity; otherwise [1e-12,1e6], mm²/count
	Iterations       int      // zero=20; finite maximum defaults128, hard4096
	RelativeChange   float64  // 0 disables stop; otherwise (0,1], relative max count update
	SourceReferences []string // at least one nonempty stable caller source identifier
	Limits           PoissonDenoisingLimits
}

type PoissonDenoisingMetadata struct {
	Derived                                                            bool
	Method, NoiseModel, Domain, Boundary, StoppingReason, SourceSHA256 string
	SourceReferences                                                   []string
	CountScale, Lambda, RelativeChange                                 float64
	IterationsRequested, IterationsCompleted, FractionalCountSamples   int
	SpacingMM                                                          [3]float64
	Dims                                                               [3]int
	ScratchBytes, WorkingBytes, NeighborVisitBound                     int64
	// Reports use pre-output-rounding estimates, counts, and the declared objective.
	InitialObjective, FinalObjective, FinalPoissonDeviance, FinalPenalty float64
	ObservedMeanCounts, EstimatedMeanCounts, MaximumChangeCounts         float64
}

type PoissonDenoisingResult struct {
	Volume   *Volume
	Metadata PoissonDenoisingMetadata
}

type poissonAdmission struct {
	n, iterations, refsBytes int
	weights                  [3]float64
	scratch, working, visits int64
}

// DenoisePoisson estimates a nonnegative independent-Poisson intensity by minimizing
// sum_i[x_i-y_i+y_i log(y_i/x_i)] + Lambda/2 sum_edges[(x_i-x_j)/spacing]^2,
// where y=src*CountScale. Fractional y uses the explicitly reported continuous
// quasi-likelihood extension, not a claim of literal discrete Poisson observations.
// Zero counts contribute x_i; positive counts require
// positive x_i. Edges join only existing axial neighbors (natural/no-flux border).
// Deterministic x-fastest Gauss-Seidel sweeps minimize each scalar coordinate exactly.
// This quadratic prior smooths edges, may bias intensities, and is a research estimate,
// not preserved acquisition measurement or Rician-magnitude correction. Errors and
// cancellation return nil; caller must not mutate src/references concurrently.
func DenoisePoisson(ctx context.Context, src *Volume, opts PoissonDenoisingOptions) (*PoissonDenoisingResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a, err := admitPoisson(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	x := make([]float64, a.n)
	var sum, maxCount, maxSource float64
	fractional := 0
	h := sha256.New()
	var word [4]byte
	for i, v := range src.Data {
		if i&255 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		x[i] = float64(v) * opts.CountScale
		if x[i] != math.Trunc(x[i]) {
			fractional++
		}
		sum += x[i]
		maxCount = math.Max(maxCount, x[i])
		maxSource = math.Max(maxSource, float64(v))
		binary.LittleEndian.PutUint32(word[:], math.Float32bits(v))
		_, _ = h.Write(word[:])
	}
	// Literal identity has x=y and lambda=0: every objective component is exactly
	// zero. Do not manufacture a residual through rounded logarithm/subtraction.
	var initial float64
	if opts.Lambda != 0 {
		initial, _, _, err = poissonObjective(ctx, src, x, opts.CountScale, opts.Lambda, a.weights)
		if err != nil {
			return nil, err
		}
	}
	completed := 0
	reason := "iteration-limit"
	change := 0.0
	if opts.Lambda == 0 {
		reason = "identity"
	} else if maxCount == 0 {
		reason = "zero-counts"
	} else {
		for iteration := 0; iteration < a.iterations; iteration++ {
			change = 0
			for i := range x {
				if i&255 == 0 {
					if err = ctx.Err(); err != nil {
						return nil, err
					}
				}
				neighbors, degree := poissonNeighbors(src.Dims, x, i, a.weights)
				y := float64(src.Data[i]) * opts.CountScale
				next := poissonCoordinate(y, opts.Lambda*degree, opts.Lambda*neighbors)
				if !(next >= 0) || math.IsInf(next, 0) || (y > 0 && next == 0) {
					return nil, ErrPoissonDenoisingNumeric
				}
				change = math.Max(change, math.Abs(next-x[i]))
				x[i] = next
			}
			completed++
			if opts.RelativeChange > 0 && change/maxCount <= opts.RelativeChange {
				reason = "relative-change"
				break
			}
		}
	}
	var final, deviance, penalty float64
	if opts.Lambda != 0 {
		final, deviance, penalty, err = poissonObjective(ctx, src, x, opts.CountScale, opts.Lambda, a.weights)
		if err != nil {
			return nil, err
		}
	}
	out := &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, a.n)}
	var estimated float64
	for i, value := range x {
		if i&255 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		estimated += value
		if opts.Lambda == 0 {
			// Preserve every input bit, including signed zero and gain roundoff at MaxFloat32.
			out.Data[i] = src.Data[i]
			continue
		}
		physical := value / opts.CountScale
		// Convex scalar minimizers obey the observed global maximum principle.
		// A last-bit positive overshoot is clipped only when below float64 roundoff.
		if physical > maxSource && physical <= maxSource*(1+1e-14) {
			physical = maxSource
		}
		if physical > math.MaxFloat32 || !(physical >= 0) || math.IsInf(physical, 0) {
			return nil, ErrPoissonDenoisingNumeric
		}
		out.Data[i] = float32(physical)
	}
	refs := make([]string, len(opts.SourceReferences))
	for i, s := range opts.SourceReferences {
		refs[i] = strings.Clone(s)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &PoissonDenoisingResult{Volume: out, Metadata: PoissonDenoisingMetadata{
		Derived: true, Method: "Poisson-quadratic-MAP-coordinate", NoiseModel: "independent-Poisson",
		Domain: "nonnegative-count-intensity-estimate", Boundary: "natural-axial-no-flux",
		StoppingReason: reason, SourceSHA256: hex.EncodeToString(h.Sum(nil)), SourceReferences: refs,
		CountScale: opts.CountScale, Lambda: opts.Lambda, RelativeChange: opts.RelativeChange,
		IterationsRequested: a.iterations, IterationsCompleted: completed, FractionalCountSamples: fractional, Dims: src.Dims, SpacingMM: src.SpacingMM,
		ScratchBytes: a.scratch, WorkingBytes: a.working, NeighborVisitBound: a.visits,
		InitialObjective: initial, FinalObjective: final, FinalPoissonDeviance: deviance, FinalPenalty: penalty,
		ObservedMeanCounts: sum / float64(a.n), EstimatedMeanCounts: estimated / float64(a.n), MaximumChangeCounts: change,
	}}, nil
}

func admitPoisson(ctx context.Context, src *Volume, o PoissonDenoisingOptions) (a poissonAdmission, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	bad := func(s string) error { return fmt.Errorf("%w: %s", ErrPoissonDenoisingInput, s) }
	if src == nil {
		return a, bad("nil volume")
	}
	n, ok := voxelCount(src.Dims)
	if !ok || n != len(src.Data) {
		return a, bad("dimensions/data")
	}
	a.n = n
	if !(o.CountScale >= 1e-12 && o.CountScale <= 1e12) {
		return a, bad("count scale")
	}
	if !(o.Lambda == 0 || (o.Lambda >= 1e-12 && o.Lambda <= 1e6)) {
		return a, bad("lambda")
	}
	if !(o.RelativeChange >= 0 && o.RelativeChange <= 1) {
		return a, bad("relative change")
	}
	a.iterations = o.Iterations
	if a.iterations == 0 {
		a.iterations = 20
	}
	if a.iterations < 1 || a.iterations > 4096 {
		return a, bad("iterations")
	}
	// Admission constants guarantee all products below int64 and arithmetic below float64.
	check := func(v, def, hard, need int64, label string) error {
		if v < 0 || v > hard {
			return bad(label)
		}
		if v == 0 {
			v = def
		}
		if need > v {
			return fmt.Errorf("%w: %s requires %d > %d", ErrPoissonDenoisingLimit, label, need, v)
		}
		return nil
	}
	if err = check(o.Limits.MaxVoxels, 1<<20, 1<<24, int64(n), "voxels"); err != nil {
		return
	}
	if err = check(int64(o.Limits.MaxIterations), 128, 4096, int64(a.iterations), "iterations"); err != nil {
		return
	}
	if len(o.SourceReferences) == 0 {
		return a, bad("source references required")
	}
	if err = check(int64(o.Limits.MaxSourceReferences), 32, 1024, int64(len(o.SourceReferences)), "references"); err != nil {
		return
	}
	for _, s := range o.SourceReferences {
		if len(s) == 0 {
			return a, bad("empty source reference")
		}
		if len(s) > 1<<20-a.refsBytes {
			return a, fmt.Errorf("%w: reference bytes", ErrPoissonDenoisingLimit)
		}
		a.refsBytes += len(s)
	}
	if err = check(int64(o.Limits.MaxReferenceBytes), 4096, 1<<20, int64(a.refsBytes), "reference bytes"); err != nil {
		return
	}
	a.scratch = 8 * int64(n)
	a.working = 12*int64(n) + int64(a.refsBytes) + 16*int64(len(o.SourceReferences))
	// Six neighbors per coordinate; four complete scans cover two objectives and setup/output.
	a.visits = int64(n) * (6*int64(a.iterations) + 24)
	if err = check(o.Limits.MaxScratchBytes, 16<<20, 128<<20, a.scratch, "scratch bytes"); err != nil {
		return
	}
	if err = check(o.Limits.MaxWorkingBytes, 32<<20, 256<<20, a.working, "working bytes"); err != nil {
		return
	}
	if err = check(o.Limits.MaxNeighborVisits, 100000000, 5000000000, a.visits, "neighbor visits"); err != nil {
		return
	}
	for axis, s := range src.SpacingMM {
		if src.Dims[axis] > 1 {
			w := 1 / (s * s)
			if !(s > 0 && w >= 1e-12 && w <= 1e12) {
				return a, bad("active spacing outside physical weight range")
			}
			a.weights[axis] = w
		} else if !(s >= 0 && s <= math.MaxFloat64) {
			return a, bad("inactive spacing")
		}
	}
	for i, v := range src.Data {
		if i&255 == 0 {
			if err = ctx.Err(); err != nil {
				return
			}
		}
		if !(v >= 0) || math.IsInf(float64(v), 0) {
			return a, bad("nonfinite/negative samples")
		}
	}
	return
}

// Stable positive quadratic root a*x*x+(1-neighborSum)*x-y=0.
// a=0 (isolated/identity coordinate) returns the exact observed count. A true
// positive count is never replaced by an epsilon floor or artificial background.
func poissonCoordinate(y, a, neighborSum float64) float64 {
	if a == 0 {
		return y
	}
	b := neighborSum - 1
	d := math.Hypot(b, 2*math.Sqrt(a)*math.Sqrt(y))
	if b >= 0 {
		return (b + d) / (2 * a)
	}
	return 2 * y / (d - b)
}

func poissonNeighbors(d [3]int, x []float64, i int, w [3]float64) (sum, degree float64) {
	strides := [3]int{1, d[0], d[0] * d[1]}
	coords := [3]int{i % d[0], (i / d[0]) % d[1], i / (d[0] * d[1])}
	for axis := 0; axis < 3; axis++ {
		if coords[axis] > 0 {
			sum += w[axis] * x[i-strides[axis]]
			degree += w[axis]
		}
		if coords[axis]+1 < d[axis] {
			sum += w[axis] * x[i+strides[axis]]
			degree += w[axis]
		}
	}
	return
}

func poissonObjective(ctx context.Context, src *Volume, x []float64, scale, lambda float64, w [3]float64) (total, deviance, penalty float64, err error) {
	strides := [3]int{1, src.Dims[0], src.Dims[0] * src.Dims[1]}
	var likelihood float64
	for i, v := range x {
		if i&255 == 0 {
			if err = ctx.Err(); err != nil {
				return
			}
		}
		y := float64(src.Data[i]) * scale
		term := v
		if y > 0 {
			if !(v > 0) {
				err = ErrPoissonDenoisingNumeric
				return
			}
			logratio := math.Log(y) - math.Log(v)
			if math.Abs(y-v) < v/2 {
				logratio = math.Log1p((y - v) / v)
			}
			term = v - y + y*logratio
			if term < 0 && term >= -1e-14*math.Max(y, v) {
				term = 0
			}
		}
		if term < 0 || math.IsInf(term, 0) || math.IsNaN(term) {
			err = ErrPoissonDenoisingNumeric
			return
		}
		likelihood += term
		coords := [3]int{i % src.Dims[0], (i / src.Dims[0]) % src.Dims[1], i / (src.Dims[0] * src.Dims[1])}
		for axis := 0; axis < 3; axis++ {
			if coords[axis]+1 < src.Dims[axis] {
				delta := v - x[i+strides[axis]]
				penalty += lambda * .5 * w[axis] * delta * delta
			}
		}
	}
	total = likelihood + penalty
	deviance = 2 * likelihood
	if !(total >= 0) || math.IsInf(total, 0) {
		err = ErrPoissonDenoisingNumeric
	}
	return
}
