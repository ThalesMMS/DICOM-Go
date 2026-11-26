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
	ErrPSFDeconvolutionInput   = errors.New("imagefilter: invalid PSF deconvolution input")
	ErrPSFDeconvolutionLimit   = errors.New("imagefilter: PSF deconvolution resource limit")
	ErrPSFDeconvolutionNumeric = errors.New("imagefilter: unsupported or nonfinite PSF restoration arithmetic")
)

type PSFNormalization uint8

const (
	PSFRequireUnitMass PSFNormalization = iota
	PSFNormalizeMass
)

type PSFNegativePolicy uint8

const (
	PSFRejectNegative PSFNegativePolicy = iota
	PSFClampNegative
)

// PSFDeconvolutionLimits are logical payload/work admissions, not RSS. Zero
// selects finite defaults; negative limits are invalid. FFTWorkingBytes is
// reported zero because this profile uses direct convolution, never an FFT.
type PSFDeconvolutionLimits struct {
	MaxVoxels, MaxPSFSamples, MaxScratchBytes, MaxWorkingBytes, MaxKernelVisits int64
	MaxIterations                                                               int
	MaxSourceReferences, MaxReferenceBytes                                      int
}
type PSFDeconvolutionOptions struct {
	Iterations     int     // zero defaults20; positive and bounded by MaxIterations
	RelativeChange float64 // zero disables relative L1 early stopping
	Epsilon        float64 // zero-prediction fallback relative to observed maximum; zero=1e-12
	Boundary       PSFBoundary
	Normalization  PSFNormalization
	NegativePolicy PSFNegativePolicy
	Limits         PSFDeconvolutionLimits
	// Stable experiment/file/instance identifiers supplied by caller; mandatory
	// for restoration, preserved as owned strings, not interpreted as DICOM UIDs.
	SourceReferences []string
}
type PSFDerivedMetadata struct {
	Derived                                                       bool
	Method, NoiseModel, StoppingReason, PSFSHA256                 string
	SourceReferences                                              []string
	IterationsRequested, IterationsCompleted, NegativeClamped     int
	Boundary                                                      PSFBoundary
	Normalization                                                 PSFNormalization
	NegativePolicy                                                PSFNegativePolicy
	PSFDims                                                       [3]int
	SpacingMM                                                     [3]float64
	OriginalPSFMass, Epsilon, RelativeChange, IntensityScale      float64
	ScratchBytes, WorkingBytes, FFTWorkingBytes, KernelVisitBound int64
}
type PSFDeconvolutionResult struct {
	Restored                                                             *Volume
	Metadata                                                             PSFDerivedMetadata
	InitialRMSE, FinalRMSE, InitialPoissonDeviance, FinalPoissonDeviance float64
}

func psfOptions(o PSFDeconvolutionOptions) (PSFDeconvolutionOptions, error) {
	l := &o.Limits
	if o.Iterations < 0 || !psfFinite(o.RelativeChange) || o.RelativeChange < 0 || !psfFinite(o.Epsilon) || o.Epsilon < 0 || o.Epsilon > 1 || o.Boundary > PSFCircularBoundary || o.Normalization > PSFNormalizeMass || o.NegativePolicy > PSFClampNegative || l.MaxVoxels < 0 || l.MaxPSFSamples < 0 || l.MaxScratchBytes < 0 || l.MaxWorkingBytes < 0 || l.MaxKernelVisits < 0 || l.MaxIterations < 0 || l.MaxSourceReferences < 0 || l.MaxReferenceBytes < 0 {
		return o, ErrPSFDeconvolutionInput
	}
	if o.Iterations == 0 {
		o.Iterations = 20
	}
	if o.Epsilon == 0 {
		o.Epsilon = 1e-12
	}
	if l.MaxVoxels == 0 {
		l.MaxVoxels = 1 << 20
	}
	if l.MaxPSFSamples == 0 {
		l.MaxPSFSamples = 4096
	}
	if l.MaxScratchBytes == 0 {
		l.MaxScratchBytes = 64 << 20
	}
	if l.MaxWorkingBytes == 0 {
		l.MaxWorkingBytes = 96 << 20
	}
	if l.MaxKernelVisits == 0 {
		l.MaxKernelVisits = 100 << 20
	}
	if l.MaxIterations == 0 {
		l.MaxIterations = 128
	}
	if l.MaxSourceReferences == 0 {
		l.MaxSourceReferences = 32
	}
	if l.MaxReferenceBytes == 0 {
		l.MaxReferenceBytes = 4096
	}
	if l.MaxVoxels > 16<<20 || l.MaxPSFSamples > 65536 || l.MaxIterations > 4096 || l.MaxSourceReferences > 1024 || l.MaxReferenceBytes > 1<<20 {
		return o, ErrPSFDeconvolutionLimit
	}
	if o.Iterations > l.MaxIterations {
		return o, ErrPSFDeconvolutionLimit
	}
	return o, nil
}
func psfFinite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// psfPrepare admits all owned numeric/reference allocations before creating
// normalized snapshots. Inputs must remain stable for the duration of the call.
func psfPrepare(ctx context.Context, src, psf *Volume, o PSFDeconvolutionOptions, restore bool) (y, k []float64, meta PSFDerivedMetadata, opts PSFDeconvolutionOptions, err error) {
	if ctx == nil {
		err = ErrPSFDeconvolutionInput
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	opts, err = psfOptions(o)
	if err != nil {
		return
	}
	if src == nil || psf == nil {
		err = ErrPSFDeconvolutionInput
		return
	}
	n, ok := voxelCount(src.Dims)
	if !ok || len(src.Data) != n {
		err = ErrPSFDeconvolutionInput
		return
	}
	kn, ok := voxelCount(psf.Dims)
	if !ok || len(psf.Data) != kn {
		err = ErrPSFDeconvolutionInput
		return
	}
	if int64(n) > opts.Limits.MaxVoxels || int64(kn) > opts.Limits.MaxPSFSamples {
		err = ErrPSFDeconvolutionLimit
		return
	}
	for axis := 0; axis < 3; axis++ {
		if !psfFinite(src.SpacingMM[axis]) || !psfFinite(psf.SpacingMM[axis]) || src.SpacingMM[axis] < 0 || psf.SpacingMM[axis] < 0 {
			err = ErrPSFDeconvolutionInput
			return
		}
		if psf.Dims[axis]%2 != 1 || psf.Dims[axis] > src.Dims[axis] {
			err = fmt.Errorf("%w: odd PSF support must fit source axis %d", ErrPSFDeconvolutionInput, axis)
			return
		}
		if src.Dims[axis] > 1 && (!(src.SpacingMM[axis] > 0) || !psfFinite(src.SpacingMM[axis]) || psf.SpacingMM[axis] != src.SpacingMM[axis]) {
			err = fmt.Errorf("%w: PSF/source spacing axis %d", ErrPSFDeconvolutionInput, axis)
			return
		}
	}
	refbytes := int64(0)
	if len(opts.SourceReferences) > opts.Limits.MaxSourceReferences {
		err = ErrPSFDeconvolutionLimit
		return
	}
	if restore && len(opts.SourceReferences) == 0 {
		err = ErrPSFDeconvolutionInput
		return
	}
	for i, s := range opts.SourceReferences {
		if len(s) > opts.Limits.MaxReferenceBytes {
			err = ErrPSFDeconvolutionLimit
			return
		}
		if strings.TrimSpace(s) == "" {
			err = fmt.Errorf("%w: source reference %d", ErrPSFDeconvolutionInput, i)
			return
		}
		if refbytes > int64(opts.Limits.MaxReferenceBytes)-int64(len(s)) {
			err = ErrPSFDeconvolutionLimit
			return
		}
		refbytes += int64(len(s))
	}
	// Six float64 images(y,x,pred,ratio,back,sensitivity), normalized PSF,
	// four-byte owned output plus owned reference string bytes/header payload.
	scratch := int64(n)*48 + int64(kn)*8
	working := scratch + int64(n)*4 + refbytes + int64(len(opts.SourceReferences))*16
	convolutions := int64(1)
	if restore {
		convolutions = 2*int64(opts.Iterations) + 3
	}
	pairs := int64(n) * int64(kn) // finite hard admissions guarantee representability
	if pairs > opts.Limits.MaxKernelVisits/convolutions || scratch > opts.Limits.MaxScratchBytes || working > opts.Limits.MaxWorkingBytes {
		err = ErrPSFDeconvolutionLimit
		return
	}
	scale, mass := 0.0, 0.0
	clamped := 0
	for i, v := range src.Data {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return
			}
		}
		f := float64(v)
		if !psfFinite(f) || f < 0 && opts.NegativePolicy == PSFRejectNegative {
			err = fmt.Errorf("%w: source sample %d", ErrPSFDeconvolutionInput, i)
			return
		}
		if f < 0 {
			clamped++
		}
		scale = math.Max(scale, f)
	}
	for i, v := range psf.Data {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return
			}
		}
		f := float64(v)
		if !psfFinite(f) || f < 0 {
			err = fmt.Errorf("%w: PSF sample %d", ErrPSFDeconvolutionInput, i)
			return
		}
		mass += f
	}
	if !(mass > 0) || !psfFinite(mass) || opts.Normalization == PSFRequireUnitMass && math.Abs(mass-1) > 1e-6 {
		err = fmt.Errorf("%w: PSF mass", ErrPSFDeconvolutionInput)
		return
	}
	if scale == 0 {
		scale = 1
	}
	y = make([]float64, n)
	k = make([]float64, kn)
	for i, v := range src.Data {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return
			}
		}
		y[i] = math.Max(0, float64(v)) / scale
	}
	for i, v := range psf.Data {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return
			}
		}
		k[i] = float64(v) / mass
	}
	digest := sha256.New()
	var buf [4]byte
	for i, v := range psf.Data {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return
			}
		}
		binary.LittleEndian.PutUint32(buf[:], math.Float32bits(v))
		digest.Write(buf[:])
	}
	meta = PSFDerivedMetadata{Derived: true, Method: "Richardson-Lucy-direct", NoiseModel: "independent-Poisson", IterationsRequested: opts.Iterations, NegativeClamped: clamped, Boundary: opts.Boundary, Normalization: opts.Normalization, NegativePolicy: opts.NegativePolicy, PSFDims: psf.Dims, SpacingMM: src.SpacingMM, OriginalPSFMass: mass, Epsilon: opts.Epsilon, RelativeChange: opts.RelativeChange, IntensityScale: scale, ScratchBytes: scratch, WorkingBytes: working, KernelVisitBound: pairs * convolutions, PSFSHA256: hex.EncodeToString(digest.Sum(nil))}
	meta.SourceReferences = make([]string, len(opts.SourceReferences))
	for i, s := range opts.SourceReferences {
		meta.SourceReferences[i] = strings.Clone(s)
	}
	return
}

// RichardsonLucy restores a known-PSF nonnegative research image/volume. The
// finite cropped forward operator and exact adjoint include edge sensitivity.
// Any error/cancellation returns a zero result, never a partially restored grid.
func RichardsonLucy(ctx context.Context, observed, psf *Volume, o PSFDeconvolutionOptions) (PSFDeconvolutionResult, error) {
	y, k, meta, o, err := psfPrepare(ctx, observed, psf, o, true)
	if err != nil {
		return PSFDeconvolutionResult{}, err
	}
	n := len(y)
	x, pred, ratio, back, sensitivity := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	mean := 0.0
	for i, v := range y {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return PSFDeconvolutionResult{}, err
			}
		}
		mean += v / float64(n)
	}
	for i := range x {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return PSFDeconvolutionResult{}, err
			}
		}
		x[i] = mean
		ratio[i] = 1
	}
	if err = psfConvolve(ctx, ratio, sensitivity, k, observed.Dims, psf.Dims, o.Boundary, true); err != nil {
		return PSFDeconvolutionResult{}, err
	}
	for i, s := range sensitivity {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return PSFDeconvolutionResult{}, err
			}
		}
		if !(s > 0) || !psfFinite(s) {
			return PSFDeconvolutionResult{}, fmt.Errorf("%w: unobserved forward column", ErrPSFDeconvolutionNumeric)
		}
	}
	if err = psfConvolve(ctx, x, pred, k, observed.Dims, psf.Dims, o.Boundary, false); err != nil {
		return PSFDeconvolutionResult{}, err
	}
	initialR, initialD, err := psfResidual(ctx, y, pred, meta.IntensityScale, o.Epsilon)
	if err != nil {
		return PSFDeconvolutionResult{}, err
	}
	meta.StoppingReason = "iteration-limit"
	if mean == 0 {
		meta.StoppingReason = "zero-input"
	} else {
		for iteration := 0; iteration < o.Iterations; iteration++ {
			if err = ctx.Err(); err != nil {
				return PSFDeconvolutionResult{}, err
			}
			for i := range ratio {
				if i&1023 == 0 {
					if err = ctx.Err(); err != nil {
						return PSFDeconvolutionResult{}, err
					}
				}
				ratio[i] = y[i] / psfPositivePrediction(pred[i], o.Epsilon)
				if !psfFinite(ratio[i]) {
					return PSFDeconvolutionResult{}, ErrPSFDeconvolutionNumeric
				}
			}
			if err = psfConvolve(ctx, ratio, back, k, observed.Dims, psf.Dims, o.Boundary, true); err != nil {
				return PSFDeconvolutionResult{}, err
			}
			difference, total := 0.0, 0.0
			for i, old := range x {
				if i&1023 == 0 {
					if err = ctx.Err(); err != nil {
						return PSFDeconvolutionResult{}, err
					}
				}
				v := old * back[i] / sensitivity[i]
				if v < 0 || !psfFinite(v) {
					return PSFDeconvolutionResult{}, ErrPSFDeconvolutionNumeric
				}
				x[i] = v
				difference += math.Abs(v - old)
				total += old
			}
			if err = psfConvolve(ctx, x, pred, k, observed.Dims, psf.Dims, o.Boundary, false); err != nil {
				return PSFDeconvolutionResult{}, err
			}
			meta.IterationsCompleted = iteration + 1
			if o.RelativeChange > 0 && difference <= o.RelativeChange*math.Max(total, o.Epsilon) {
				meta.StoppingReason = "relative-L1"
				break
			}
		}
	}
	finalR, finalD, err := psfResidual(ctx, y, pred, meta.IntensityScale, o.Epsilon)
	if err != nil {
		return PSFDeconvolutionResult{}, err
	}
	if !psfFinite(initialR) || !psfFinite(initialD) || !psfFinite(finalR) || !psfFinite(finalD) {
		return PSFDeconvolutionResult{}, ErrPSFDeconvolutionNumeric
	}
	data := make([]float32, n)
	for i, v := range x {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return PSFDeconvolutionResult{}, err
			}
		}
		physical := v * meta.IntensityScale
		if !psfFinite(physical) || physical > math.MaxFloat32 {
			return PSFDeconvolutionResult{}, ErrPSFDeconvolutionNumeric
		}
		data[i] = float32(physical)
	}
	if err = ctx.Err(); err != nil {
		return PSFDeconvolutionResult{}, err
	}
	return PSFDeconvolutionResult{Restored: &Volume{Dims: observed.Dims, SpacingMM: observed.SpacingMM, Data: data}, Metadata: meta, InitialRMSE: initialR, FinalRMSE: finalR, InitialPoissonDeviance: initialD, FinalPoissonDeviance: finalD}, nil
}

// ConvolvePSFContext exposes the exact same known-PSF forward model. It returns
// an owned nonnegative grid; no source modification or implicit resampling.
func ConvolvePSFContext(ctx context.Context, source, psf *Volume, o PSFDeconvolutionOptions) (*Volume, error) {
	y, k, meta, o, err := psfPrepare(ctx, source, psf, o, false)
	if err != nil {
		return nil, err
	}
	pred := make([]float64, len(y))
	if err = psfConvolve(ctx, y, pred, k, source.Dims, psf.Dims, o.Boundary, false); err != nil {
		return nil, err
	}
	data := make([]float32, len(y))
	for i, v := range pred {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		value := v * meta.IntensityScale
		if !psfFinite(value) || value > math.MaxFloat32 {
			return nil, ErrPSFDeconvolutionNumeric
		}
		data[i] = float32(value)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &Volume{Dims: source.Dims, SpacingMM: source.SpacingMM, Data: data}, nil
}
func psfResidual(ctx context.Context, y, pred []float64, scale, epsilon float64) (float64, float64, error) {
	norm, deviance := 0.0, 0.0
	for i, v := range y {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
		}
		p := pred[i]
		norm = math.Hypot(norm, p-v)
		d := p - v
		if v > 0 {
			d += v * math.Log(v/psfPositivePrediction(p, epsilon))
		}
		deviance += math.Max(0, 2*d)
	}
	return norm / math.Sqrt(float64(len(y))) * scale, deviance * scale, ctx.Err()
}

// Every positive representable prediction is retained. A global intensity floor
// would erase valid small samples in high-dynamic-range data, including a delta
// PSF. Epsilon stabilizes only an exactly zero prediction.
func psfPositivePrediction(prediction, epsilon float64) float64 {
	if prediction > 0 {
		return prediction
	}
	return epsilon
}
