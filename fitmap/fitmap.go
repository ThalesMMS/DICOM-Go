// Package fitmap computes quantitative maps by fitting a signal model to
// every voxel of a series acquired at several values of one parameter
// (#1149): T2 and T2* relaxation times from multi-echo series and the
// apparent diffusion coefficient from multi-b diffusion series.
//
// The model is the mono-exponential decay S(x) = S0·exp(−x·R), fitted per
// voxel by least squares on ln S (the usual log-linear method, as Horos's
// T2 fit map does; behaviour reference only, no code was used): x is the
// echo time in ms for T2 and T2* (R = 1/T2) and the b-value in s/mm² for
// the ADC (R = ADC in mm²/s). A background level can be subtracted from
// every signal first, voxels whose first signal is at or below a threshold
// are left out, and the fit can weight each point by its squared signal to
// counter the noise amplification of the logarithm. Maps are written as
// DICOM Parametric Maps with parametricmap.Write; Kind gives the quantity
// and unit codes.
package fitmap

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/codes/ucum"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// ErrFit reports inputs a map cannot be fitted from.
var ErrFit = errors.New("dicom/fitmap: cannot fit map")

// Kind is the quantity a map holds.
type Kind int

const (
	// T2 is the transverse relaxation time in ms, from spin-echo echoes.
	T2 Kind = iota
	// T2Star is the effective transverse relaxation time in ms, from
	// gradient-echo echoes.
	T2Star
	// ADC is the apparent diffusion coefficient in µm²/s (10⁻⁶ mm²/s),
	// from diffusion b-values in s/mm².
	ADC
)

func (k Kind) valid() bool { return k == T2 || k == T2Star || k == ADC }

// String names the quantity.
func (k Kind) String() string {
	switch k {
	case T2:
		return "T2"
	case T2Star:
		return "T2*"
	case ADC:
		return "ADC"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Quantity is the quantity's code in CID 7180 (T2, T2*) or CID 7203 (ADC),
// for the Quantity Definition and the Derivation Code of a Parametric Map.
func (k Kind) Quantity() codes.Code {
	switch k {
	case T2:
		return dcm.T2_113065
	case T2Star:
		return dcm.T2_113064
	case ADC:
		return dcm.ApparentDiffusionCoefficient
	}
	return codes.Code{}
}

// Units is the UCUM code of the map's values: ms for T2 and T2*, um2/s for
// the ADC.
func (k Kind) Units() codes.Code {
	if k == ADC {
		return ucum.Um2PerS
	}
	return ucum.Millisecond
}

// ParameterName names the fitted parameter and its unit.
func (k Kind) ParameterName() string {
	if k == ADC {
		return "b-value (s/mm²)"
	}
	return "echo time (ms)"
}

// Options configure Fit. The zero value fits every voxel by unweighted
// least squares.
type Options struct {
	// Mask, when set, has one entry per voxel; voxels it leaves out are not
	// fitted.
	Mask []bool
	// Background is subtracted from every signal before the fit (the noise
	// floor of magnitude images); signals at or below it are left out of a
	// voxel's fit.
	Background float64
	// Threshold leaves out the voxels whose signal at the lowest parameter
	// is at or below it (air and background).
	Threshold float64
	// Weighted weights each point by its squared signal, the variance
	// correction of the log transform, instead of fitting unweighted.
	Weighted bool
	// MaxValue, when positive, leaves out results above it (a slow decay
	// fitted to noise) as out of range.
	MaxValue float64
	// Workers bounds the goroutines (0: the module's shared concurrency
	// budget). The result does not depend on it.
	Workers int
}

// Result is a fitted map.
type Result struct {
	Kind Kind
	// Parameters are the distinct parameter values fitted, ascending (echo
	// times in ms or b-values in s/mm²).
	Parameters []float64
	// Values are the map in Kind's unit, one per voxel in input order; NaN
	// where the voxel was not fitted.
	Values []float64
	// S0 is the fitted signal at parameter zero; RSquared the coefficient
	// of determination of the (weighted) log-linear fit. Both are NaN where
	// the voxel was not fitted.
	S0, RSquared []float64
	// Fitted counts the voxels with a value; Skipped those masked out or at
	// or below the threshold; Insufficient those with fewer than two
	// distinct parameters above the background; NonDecaying those whose
	// signal did not decay; OutOfRange those above MaxValue.
	Fitted, Skipped, Insufficient, NonDecaying, OutOfRange int
}

// fitChunk is the number of voxels a worker claims at a time.
const fitChunk = 4096

// Fit fits the model to every voxel. parameters has one value per image
// (several images may share a value, such as the directions of one
// b-value); images are signal volumes of one length, in any order.
func Fit(ctx context.Context, kind Kind, parameters []float64, images [][]float32, opts Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !kind.valid() {
		return Result{}, fmt.Errorf("%w: unknown kind %d", ErrFit, int(kind))
	}
	if len(parameters) != len(images) {
		return Result{}, fmt.Errorf("%w: %d parameters for %d images", ErrFit, len(parameters), len(images))
	}
	if len(images) < 2 {
		return Result{}, fmt.Errorf("%w: %d images", ErrFit, len(images))
	}
	voxels := len(images[0])
	for i, x := range parameters {
		if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
			return Result{}, fmt.Errorf("%w: %s %v", ErrFit, kind.ParameterName(), x)
		}
		if len(images[i]) != voxels {
			return Result{}, fmt.Errorf("%w: image %d has %d voxels, want %d", ErrFit, i, len(images[i]), voxels)
		}
	}
	if opts.Mask != nil && len(opts.Mask) != voxels {
		return Result{}, fmt.Errorf("%w: mask has %d voxels, want %d", ErrFit, len(opts.Mask), voxels)
	}
	if !finite(opts.Background) || !finite(opts.Threshold) || !finite(opts.MaxValue) || opts.Workers < 0 {
		return Result{}, fmt.Errorf("%w: background, threshold and maximum must be finite and workers not negative", ErrFit)
	}
	order := make([]int, len(parameters))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return parameters[order[a]] < parameters[order[b]] })
	out := Result{Kind: kind}
	for _, i := range order {
		if n := len(out.Parameters); n == 0 || parameters[i] != out.Parameters[n-1] {
			out.Parameters = append(out.Parameters, parameters[i])
		}
	}
	if len(out.Parameters) < 2 {
		return Result{}, fmt.Errorf("%w: the %ss are not distinct", ErrFit, kind.ParameterName())
	}
	first := images[order[0]]
	out.Values = make([]float64, voxels)
	out.S0 = make([]float64, voxels)
	out.RSquared = make([]float64, voxels)
	var skipped, insufficient, nonDecaying, outOfRange, fitted atomic.Int64
	chunks := (voxels + fitChunk - 1) / fitChunk
	err := parallel.Run(ctx, chunks, opts.Workers, 1, func() func(int) {
		x := make([]float64, 0, len(images))
		y := make([]float64, 0, len(images))
		w := make([]float64, 0, len(images))
		return func(chunk int) {
			var counts [5]int64
			for v := chunk * fitChunk; v < min(voxels, (chunk+1)*fitChunk); v++ {
				out.Values[v], out.S0[v], out.RSquared[v] = math.NaN(), math.NaN(), math.NaN()
				if (opts.Mask != nil && !opts.Mask[v]) || !(float64(first[v]) > opts.Threshold) {
					counts[0]++
					continue
				}
				x, y, w = x[:0], y[:0], w[:0]
				distinct := false
				for _, i := range order {
					s := float64(images[i][v]) - opts.Background
					if !(s > 0) || math.IsInf(s, 0) {
						continue
					}
					if len(x) > 0 && parameters[i] != x[0] {
						distinct = true
					}
					x = append(x, parameters[i])
					y = append(y, math.Log(s))
					weight := 1.0
					if opts.Weighted {
						weight = s * s
					}
					w = append(w, weight)
				}
				if !distinct {
					counts[1]++
					continue
				}
				rate, intercept, r2 := logLinear(x, y, w)
				if !(rate > 0) {
					counts[2]++
					continue
				}
				value := 1 / rate
				if kind == ADC {
					value = rate * 1e6
				}
				if opts.MaxValue > 0 && value > opts.MaxValue {
					counts[3]++
					continue
				}
				out.Values[v], out.S0[v], out.RSquared[v] = value, math.Exp(intercept), r2
				counts[4]++
			}
			skipped.Add(counts[0])
			insufficient.Add(counts[1])
			nonDecaying.Add(counts[2])
			outOfRange.Add(counts[3])
			fitted.Add(counts[4])
		}
	})
	if err != nil {
		return Result{}, err
	}
	out.Skipped, out.Insufficient, out.NonDecaying = int(skipped.Load()), int(insufficient.Load()), int(nonDecaying.Load())
	out.OutOfRange, out.Fitted = int(outOfRange.Load()), int(fitted.Load())
	return out, nil
}

// logLinear fits y = a − rate·x by weighted least squares and returns the
// decay rate, the intercept a and the weighted coefficient of determination
// (1 when y is constant or the fit exact). x holds at least two distinct
// values.
func logLinear(x, y, w []float64) (rate, intercept, r2 float64) {
	var sw, sx, sy float64
	for i := range x {
		sw += w[i]
		sx += w[i] * x[i]
		sy += w[i] * y[i]
	}
	mx, my := sx/sw, sy/sw
	var sxx, sxy, syy float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		sxx += w[i] * dx * dx
		sxy += w[i] * dx * dy
		syy += w[i] * dy * dy
	}
	slope := sxy / sxx
	intercept = my - slope*mx
	r2 = 1.0
	if syy > 0 {
		var residual float64
		for i := range x {
			d := y[i] - (intercept + slope*x[i])
			residual += w[i] * d * d
		}
		r2 = 1 - residual/syy
	}
	return -slope, intercept, r2
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
