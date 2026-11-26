package roi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
)

var (
	ErrProbabilisticInput      = errors.New("roi: invalid probabilistic classification input")
	ErrProbabilisticLimit      = errors.New("roi: probabilistic classification resource limit")
	ErrProbabilisticEmptyClass = errors.New("roi: empty Gaussian mixture class")
)

// ScalarClassificationInput is one physically aligned scalar channel, x-fastest.
// Values and Mask are immutable borrowed inputs for the duration of a call.
// Units and Normalization identify the calibration, not a requested transform.
// A nil mask selects all voxels. Excluded/nonfinite-skipped voxels get label 0
// and all-zero posteriors; labels of selected voxels are one-based.
type ScalarClassificationInput struct {
	Dimensions           [3]int
	SpacingMM            [3]float64
	Values               []float64
	Mask                 []bool
	Units, Normalization string
}

// GaussianClass uses the input's units (variance uses units squared).
type GaussianClass struct{ Mean, Variance, Weight float64 }

type ProbabilisticLimits struct {
	MaxVoxels, MaxClasses, MaxIterations int
	MaxWorkingBytes                      int64
}

func DefaultProbabilisticLimits() ProbabilisticLimits {
	return ProbabilisticLimits{1 << 20, 8, 256, 256 << 20}
}

// MRFOptions selects synchronous damped mean-field inference for a six-neighbor
// attractive Potts model. Edge weight is 1/distance in mm. Beta therefore has
// units mm. ContrastSigma=0 disables the intensity edge factor, otherwise the
// edge is multiplied by exp(-0.5*(difference/ContrastSigma)^2).
type MRFOptions struct {
	Beta, ContrastSigma, Tolerance, Damping float64
	Iterations                              int
}

type ProbabilisticOptions struct {
	Classes, EMIterations      int
	Seed                       int64
	VarianceFloor, EMTolerance float64
	// Initial may be supplied to FitGaussianMixture; otherwise seeded k-means++
	// chooses distinct data values. Components are canonicalized by increasing mean.
	Initial []GaussianClass
	// Priors, if present, replace learned mixture weights for classification only.
	// They refer to increasing-mean class order and are normalized by their sum.
	Priors               []float64
	SkipNonFinite        bool
	Units, Normalization string
	MRF                  MRFOptions
	Limits               ProbabilisticLimits
}

// ProbabilisticResult is detached and owned by the caller. Posteriors is
// voxel-major. Without MRF it contains exact Bayes probabilities; with MRF it
// contains approximate mean-field marginals, NOT exact joint probabilities.
// No result is returned after cancellation or failure, including a partial mask.
type ProbabilisticResult struct {
	Dimensions                                                   [3]int
	SpacingMM                                                    [3]float64
	Classes                                                      []GaussianClass
	Priors                                                       []float64
	Labels                                                       []uint16
	Posteriors                                                   []float64
	LogLikelihood                                                float64
	EMIterations, MRFIterations                                  int
	EMConverged, MRFConverged                                    bool
	SelectedVoxels, SkippedNonFinite                             int
	WorkingBytes                                                 int64
	SourceSHA256, ParametersSHA256, Method, Units, Normalization string
}

type probabilityWork struct {
	in                   ScalarClassificationInput
	o                    ProbabilisticOptions
	valid                []bool
	n, k, count, skipped int
	bytes                int64
}

func probabilityFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func probabilityCheck(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrProbabilisticInput)
	}
	return ctx.Err()
}
func prepareProbability(ctx context.Context, in ScalarClassificationInput, o ProbabilisticOptions) (probabilityWork, error) {
	var w probabilityWork
	if err := probabilityCheck(ctx); err != nil {
		return w, err
	}
	d := DefaultProbabilisticLimits()
	l := o.Limits
	if l.MaxVoxels < 0 || l.MaxClasses < 0 || l.MaxIterations < 0 || l.MaxWorkingBytes < 0 {
		return w, ErrProbabilisticInput
	}
	if l.MaxVoxels == 0 {
		l.MaxVoxels = d.MaxVoxels
	}
	if l.MaxClasses == 0 {
		l.MaxClasses = d.MaxClasses
	}
	if l.MaxIterations == 0 {
		l.MaxIterations = d.MaxIterations
	}
	if l.MaxWorkingBytes == 0 {
		l.MaxWorkingBytes = d.MaxWorkingBytes
	}
	o.Limits = l
	if l.MaxIterations > 4096 {
		return w, ErrProbabilisticLimit
	}
	n := 1
	for a, x := range in.Dimensions {
		if x <= 0 || x > l.MaxVoxels/n {
			return w, ErrProbabilisticLimit
		}
		n *= x
		if !probabilityFinite(in.SpacingMM[a]) || in.SpacingMM[a] < 1e-9 || in.SpacingMM[a] > 1e9 {
			return w, ErrProbabilisticInput
		}
	}
	if len(in.Values) != n || (in.Mask != nil && len(in.Mask) != n) || o.Classes < 1 || o.Classes > 64 {
		return w, ErrProbabilisticInput
	}
	if o.Classes > l.MaxClasses {
		return w, ErrProbabilisticLimit
	}
	if len(in.Units) > 256 || len(in.Normalization) > 256 || in.Units == "" || in.Normalization == "" || in.Units != o.Units || in.Normalization != o.Normalization {
		return w, fmt.Errorf("%w: units/normalization mismatch", ErrProbabilisticInput)
	}
	if o.EMIterations == 0 {
		o.EMIterations = 100
	}
	if o.VarianceFloor == 0 {
		o.VarianceFloor = 1e-6
	}
	if o.EMTolerance == 0 {
		o.EMTolerance = 1e-8
	}
	m := o.MRF
	if m.Beta < 0 || m.Beta > 1e9 || !probabilityFinite(m.Beta) || m.ContrastSigma < 0 || !probabilityFinite(m.ContrastSigma) || m.Iterations < 0 {
		return w, ErrProbabilisticInput
	}
	if m.Beta > 0 && m.Iterations == 0 {
		m.Iterations = 20
	}
	if m.Tolerance == 0 {
		m.Tolerance = 1e-6
	}
	if m.Damping == 0 {
		m.Damping = .5
	}
	o.MRF = m
	if o.EMIterations < 1 || o.EMIterations > l.MaxIterations || m.Iterations > l.MaxIterations {
		return w, ErrProbabilisticLimit
	}
	if !probabilityFinite(o.VarianceFloor) || o.VarianceFloor < 1e-100 || o.VarianceFloor > 1e200 || !probabilityFinite(o.EMTolerance) || o.EMTolerance <= 0 || !probabilityFinite(m.Tolerance) || m.Tolerance <= 0 || !probabilityFinite(m.Damping) || m.Damping <= 0 || m.Damping > 1 {
		return w, ErrProbabilisticInput
	}
	if len(o.Initial) != 0 && len(o.Initial) != o.Classes {
		return w, ErrProbabilisticInput
	}
	if len(o.Priors) != 0 && len(o.Priors) != o.Classes {
		return w, ErrProbabilisticInput
	}
	// Upper bound includes valid mask, uint16 labels, output, unary and next MRF
	// buffers (log unary is populated directly, not an additional fourth buffer),
	// detached model/options/provenance and EM O(K) scratch. Input excluded.
	if int64(n) > (math.MaxInt64-int64(o.Classes)*1024-16384)/(int64(o.Classes)*24+3) {
		return w, ErrProbabilisticLimit
	}
	if n > int(^uint(0)>>1)/o.Classes {
		return w, ErrProbabilisticLimit
	}
	bytes := int64(n)*(int64(o.Classes)*24+3) + int64(o.Classes)*1024 + 16384
	if bytes > l.MaxWorkingBytes {
		return w, ErrProbabilisticLimit
	}
	o.Initial = append([]GaussianClass(nil), o.Initial...)
	o.Priors = append([]float64(nil), o.Priors...)
	if len(o.Initial) > 0 {
		if err := probabilityModel(o.Initial, o.VarianceFloor); err != nil {
			return w, err
		}
	}
	if len(o.Priors) > 0 {
		sum := 0.0
		for _, p := range o.Priors {
			if !probabilityFinite(p) || p < 0 {
				return w, ErrProbabilisticInput
			}
			sum += p
		}
		if !probabilityFinite(sum) || sum <= 0 {
			return w, ErrProbabilisticInput
		}
	}
	w = probabilityWork{in: in, o: o, n: n, k: o.Classes, bytes: bytes, valid: make([]bool, n)}
	for i, x := range in.Values {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return probabilityWork{}, err
			}
		}
		if in.Mask != nil && !in.Mask[i] {
			continue
		}
		if !probabilityFinite(x) {
			if !o.SkipNonFinite {
				return probabilityWork{}, ErrProbabilisticInput
			}
			w.skipped++
			continue
		}
		if math.Abs(x) > 1e100 {
			return probabilityWork{}, fmt.Errorf("%w: scalar magnitude exceeds numeric domain", ErrProbabilisticInput)
		}
		w.valid[i] = true
		w.count++
	}
	if w.count == 0 {
		return probabilityWork{}, ErrProbabilisticEmptyClass
	}
	return w, nil
}
func probabilityModel(c []GaussianClass, floor float64) error {
	sum := 0.0
	for i := range c {
		v := &c[i]
		if !probabilityFinite(v.Mean) || math.Abs(v.Mean) > 1e100 || !probabilityFinite(v.Variance) || v.Variance < 0 || v.Variance > 1e201 || !probabilityFinite(v.Weight) || v.Weight <= 0 {
			return ErrProbabilisticInput
		}
		v.Variance = math.Max(v.Variance, floor)
		sum += v.Weight
	}
	if !probabilityFinite(sum) || sum <= 0 {
		return ErrProbabilisticInput
	}
	for i := range c {
		c[i].Weight /= sum
	}
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].Mean != c[j].Mean {
			return c[i].Mean < c[j].Mean
		}
		if c[i].Variance != c[j].Variance {
			return c[i].Variance < c[j].Variance
		}
		return c[i].Weight < c[j].Weight
	})
	return nil
}
func probabilityScores(x float64, c []GaussianClass, priors []float64, s []float64) float64 {
	return probabilityScoresLog(x, c, priors, s, nil)
}

// Preserve normalized log unary before exponentiation; finite positive-prior
// scores may underflow in public probabilities but remain recoverable by MRF.
func probabilityScoresLog(x float64, c []GaussianClass, priors, s, logPosterior []float64) float64 {
	mx := math.Inf(-1)
	for j, v := range c {
		p := v.Weight
		if priors != nil {
			p = priors[j]
		}
		delta := (x - v.Mean) / math.Sqrt(v.Variance)
		s[j] = math.Log(p) - .5*(math.Log(2*math.Pi)+math.Log(v.Variance)+delta*delta)
		mx = math.Max(mx, s[j])
	}
	sum := 0.0
	for _, v := range s {
		sum += math.Exp(v - mx)
	}
	logSum := math.Log(sum)
	for j, v := range s {
		if logPosterior != nil {
			logPosterior[j] = v - mx - logSum
		}
		s[j] = math.Exp(v-mx) / sum
	}
	return mx + logSum
}
func seedProbability(ctx context.Context, w probabilityWork) ([]GaussianClass, error) {
	c := make([]GaussianClass, w.k)
	r := rand.New(rand.NewSource(w.o.Seed))
	target := r.Intn(w.count)
	chosen := 0
	mean, m2 := 0.0, 0.0
	count := 0.0
	for i, x := range w.in.Values {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !w.valid[i] {
			continue
		}
		if chosen == target {
			c[0].Mean = x
		}
		chosen++
		count++
		delta := x - mean
		mean += delta / count
		m2 += delta * (x - mean)
	}
	variance := math.Max(m2/count, w.o.VarianceFloor)
	for j := 1; j < w.k; j++ {
		total := 0.0
		for i, x := range w.in.Values {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if !w.valid[i] {
				continue
			}
			dist := math.Inf(1)
			for a := 0; a < j; a++ {
				d := (x - c[a].Mean) / math.Sqrt(variance)
				dist = math.Min(dist, d*d)
			}
			total += dist
		}
		if !probabilityFinite(total) || total <= 0 {
			return nil, ErrProbabilisticEmptyClass
		}
		pick := r.Float64() * total
		for i, x := range w.in.Values {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if !w.valid[i] {
				continue
			}
			dist := math.Inf(1)
			for a := 0; a < j; a++ {
				d := (x - c[a].Mean) / math.Sqrt(variance)
				dist = math.Min(dist, d*d)
			}
			pick -= dist
			if dist > 0 {
				c[j].Mean = x
			}
			if dist > 0 && pick <= 0 {
				c[j].Mean = x
				break
			}
		}
	}
	for j := range c {
		c[j].Variance = variance
		c[j].Weight = 1 / float64(w.k)
	}
	return c, nil
}

// FitGaussianMixture performs scalar Gaussian EM, then Bayes classification and
// optional spatial MRF. It does not run STAPLE or threshold a histogram.
func FitGaussianMixture(ctx context.Context, in ScalarClassificationInput, o ProbabilisticOptions) (*ProbabilisticResult, error) {
	w, err := prepareProbability(ctx, in, o)
	if err != nil {
		return nil, err
	}
	if w.count < w.k {
		return nil, ErrProbabilisticEmptyClass
	}
	c := append([]GaussianClass(nil), w.o.Initial...)
	if len(c) == 0 {
		c, err = seedProbability(ctx, w)
		if err != nil {
			return nil, err
		}
	}
	if err = probabilityModel(c, w.o.VarianceFloor); err != nil {
		return nil, err
	}
	scores := make([]float64, w.k)
	mass := make([]float64, w.k)
	means := make([]float64, w.k)
	m2 := make([]float64, w.k)
	previous := math.Inf(-1)
	iterations := 0
	converged := false
	for it := 0; it < w.o.EMIterations; it++ {
		clear(mass)
		clear(means)
		clear(m2)
		ll := 0.0
		for i, x := range in.Values {
			if i%256 == 0 {
				if err = ctx.Err(); err != nil {
					return nil, err
				}
			}
			if !w.valid[i] {
				continue
			}
			ll += probabilityScores(x, c, nil, scores)
			for j, p := range scores {
				next := mass[j] + p
				if p == 0 {
					continue
				}
				delta := x - means[j]
				means[j] += p / next * delta
				m2[j] += p * delta * (x - means[j])
				mass[j] = next
			}
		}
		if !probabilityFinite(ll) {
			return nil, ErrProbabilisticInput
		}
		if it > 0 && math.Abs(ll-previous) <= w.o.EMTolerance*(1+math.Abs(previous)) {
			converged = true
			break
		}
		previous = ll
		for j := range c {
			if mass[j] < 1e-12 {
				return nil, ErrProbabilisticEmptyClass
			}
			c[j] = GaussianClass{means[j], math.Max(m2[j]/mass[j], w.o.VarianceFloor), mass[j] / float64(w.count)}
		}
		iterations++
	}
	if err = probabilityModel(c, w.o.VarianceFloor); err != nil {
		return nil, err
	}
	result, err := finishProbability(ctx, w, c)
	if err != nil {
		return nil, err
	}
	result.EMIterations = iterations
	result.EMConverged = converged
	return result, nil
}

// ClassifyGaussianBayes uses a caller supplied model. Component labels are
// canonical increasing-mean order; a permutation of components has no effect.
func ClassifyGaussianBayes(ctx context.Context, in ScalarClassificationInput, model []GaussianClass, o ProbabilisticOptions) (*ProbabilisticResult, error) {
	if o.Classes == 0 {
		o.Classes = len(model)
	}
	w, err := prepareProbability(ctx, in, o)
	if err != nil {
		return nil, err
	}
	if len(model) != w.k {
		return nil, ErrProbabilisticInput
	}
	c := append([]GaussianClass(nil), model...)
	if err = probabilityModel(c, w.o.VarianceFloor); err != nil {
		return nil, err
	}
	return finishProbability(ctx, w, c)
}
func finishProbability(ctx context.Context, w probabilityWork, c []GaussianClass) (*ProbabilisticResult, error) {
	priors := make([]float64, w.k)
	for j, v := range c {
		priors[j] = v.Weight
	}
	if len(w.o.Priors) > 0 {
		copy(priors, w.o.Priors)
		sum := 0.0
		for _, p := range priors {
			if !probabilityFinite(p) || p < 0 {
				return nil, ErrProbabilisticInput
			}
			sum += p
		}
		if !probabilityFinite(sum) || sum <= 0 {
			return nil, ErrProbabilisticInput
		}
		for j := range priors {
			priors[j] /= sum
		}
	}
	r := &ProbabilisticResult{Dimensions: w.in.Dimensions, SpacingMM: w.in.SpacingMM, Classes: c, Priors: priors, Labels: make([]uint16, w.n), Posteriors: make([]float64, w.n*w.k), SelectedVoxels: w.count, SkippedNonFinite: w.skipped, WorkingBytes: w.bytes, Method: "scalar Gaussian Bayes", Units: w.in.Units, Normalization: w.in.Normalization}
	scores := make([]float64, w.k)
	var logUnary []float64
	if w.o.MRF.Beta > 0 {
		logUnary = make([]float64, len(r.Posteriors))
	}
	for i, x := range w.in.Values {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !w.valid[i] {
			continue
		}
		var logRow []float64
		if logUnary != nil {
			logRow = logUnary[i*w.k : (i+1)*w.k]
		}
		r.LogLikelihood += probabilityScoresLog(x, c, priors, scores, logRow)
		copy(r.Posteriors[i*w.k:], scores)
	}
	if w.o.MRF.Beta > 0 {
		if err := probabilityMRF(ctx, w, r, logUnary); err != nil {
			return nil, err
		}
		r.Method = "scalar Gaussian Bayes + physical Potts mean-field"
	}
	for i := range r.Labels {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !w.valid[i] {
			continue
		}
		best := 0
		for j := 1; j < w.k; j++ {
			if r.Posteriors[i*w.k+j] > r.Posteriors[i*w.k+best] {
				best = j
			}
		}
		r.Labels[i] = uint16(best + 1)
	}
	sh := sha256.New()
	var b [8]byte
	for _, d := range w.in.Dimensions {
		binary.LittleEndian.PutUint64(b[:], uint64(d))
		sh.Write(b[:])
	}
	for _, s := range w.in.SpacingMM {
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(s))
		sh.Write(b[:])
	}
	for i, x := range w.in.Values {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(x))
		sh.Write(b[:])
		if w.in.Mask == nil || w.in.Mask[i] {
			sh.Write([]byte{1})
		} else {
			sh.Write([]byte{0})
		}
	}
	for _, identifier := range []string{w.in.Units, w.in.Normalization} {
		binary.LittleEndian.PutUint64(b[:], uint64(len(identifier)))
		sh.Write(b[:])
		sh.Write([]byte(identifier))
	}
	r.SourceSHA256 = hex.EncodeToString(sh.Sum(nil))
	payload, err := json.Marshal(struct {
		Options ProbabilisticOptions
		Model   []GaussianClass
	}{w.o, c})
	if err != nil {
		return nil, err
	}
	ph := sha256.Sum256(payload)
	r.ParametersSHA256 = hex.EncodeToString(ph[:])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r, nil
}
