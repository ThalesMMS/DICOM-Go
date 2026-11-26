package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/fft"
)

var (
	ErrPhase2DInput   = errors.New("imagefilter: invalid phase/wavelet 2D input")
	ErrPhase2DLimit   = errors.New("imagefilter: phase/wavelet 2D resource limit")
	ErrPhase2DNumeric = errors.New("imagefilter: unrepresentable phase/wavelet 2D result")
)

// Wavelet2DBoundary declares the extension before the periodic FFT.
type Wavelet2DBoundary uint8

const (
	// WaveletReflect pads by six largest-scale sigmas on each side, reflects
	// without duplicating the endpoint, and rounds the domain up to radix-2.
	// Riesz filters have nonlocal support: this does not remove all seam effects.
	WaveletReflect Wavelet2DBoundary = iota
	// WaveletPeriodic uses the source domain without padding. Both dimensions
	// must be powers of two. Opposite source edges are adjacent by contract.
	WaveletPeriodic
)

// Phase2DLimits bounds owned numeric payload, not caller input or process RSS.
// Zero selects the hard ceilings: 2^20 source pixels, 2^22 padded pixels,
// eight bands and 256 MiB. Nonzero values may only lower these ceilings.
type Phase2DLimits struct {
	MaxPixels, MaxPaddedPixels, MaxScales int
	MaxWorkingBytes                       int64
}

// Wavelet2DOptions selects an undecimated isotropic Gaussian-difference bank.
// ScalesMM must contain one to eight strictly increasing physical Gaussian
// sigmas. The first band is I-G(sigma0), then G(previous)-G(current);
// Lowpass is G(last). The transfer functions sum to one at every frequency.
type Wavelet2DOptions struct {
	ScalesMM []float64
	Boundary Wavelet2DBoundary
	Limits   Phase2DLimits
	// Workers bounds the goroutines of the FFT line transforms (zero: the
	// module's shared concurrency budget); at most one works per 2^18 padded
	// pixels. The result does not depend on it.
	Workers int
}

// Wavelet2DMetadata records the mathematical and resource contract. Volume
// retains dimensions/spacing; origin, orientation and acquired scalar units
// remain with the caller, as for all imagefilter.Volume filters.
type Wavelet2DMetadata struct {
	Experimental               bool
	Family, Boundary           string
	Dims                       [3]int
	SpacingMM                  [3]float64
	ScalesMM                   []float64
	PaddedDims, CropOffset     [2]int
	Workers                    int
	ScratchBytes, WorkingBytes int64
}

type Wavelet2DResult struct {
	Bands    []*Volume // float32, same scalar units as the source; finest first
	Lowpass  *Volume
	Metadata Wavelet2DMetadata
}

const wavelet2DFamily = "undecimated-gaussian-difference-riesz-2d"

type wavelet2DPlan struct {
	w, h, n, pw, ph, pn, ox, oy, workers int
	scratch, working                     int64
}

// DecomposeWavelet2D computes a radial frequency partition on a regular 2D
// scalar image. It is a research feature, not an orthogonal/decimated wavelet,
// denoiser or clinical preset. All outputs are detached. Errors/cancellation
// return nil; the caller must not mutate src/options during the call.
func DecomposeWavelet2D(ctx context.Context, src *Volume, opts Wavelet2DOptions) (*Wavelet2DResult, error) {
	ctx = phase2DContext(ctx)
	p, err := admitWavelet2D(ctx, src, opts, false)
	if err != nil {
		return nil, err
	}
	spectrum, work, err := wavelet2DSpectrum(ctx, src, p)
	if err != nil {
		return nil, err
	}
	out := &Wavelet2DResult{Bands: make([]*Volume, len(opts.ScalesMM)), Metadata: p.metadata(src, opts)}
	previous := 0.0
	for j, sigma := range opts.ScalesMM {
		if err := wavelet2DFilter(ctx, spectrum, work, src, p, previous, sigma, 0); err != nil {
			return nil, err
		}
		out.Bands[j], err = wavelet2DCrop(ctx, work, src, p)
		if err != nil {
			return nil, err
		}
		previous = sigma
	}
	if err := wavelet2DFilter(ctx, spectrum, work, src, p, previous, previous, 0); err != nil {
		return nil, err
	}
	out.Lowpass, err = wavelet2DCrop(ctx, work, src, p)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ReconstructWavelet2D sums the owned lowpass and bands in float64, rounding
// only the final sample. Compatible geometry and finite coefficients are
// mandatory. Cropped bands still reconstruct the original crop exactly up to
// float32 storage error; no inverse extension is needed.
func ReconstructWavelet2D(ctx context.Context, in *Wavelet2DResult, limits Phase2DLimits) (*Volume, error) {
	ctx = phase2DContext(ctx)
	l, err := phase2DLimits(limits)
	if err != nil {
		return nil, err
	}
	if in == nil || in.Metadata.Family != wavelet2DFamily || len(in.Bands) < 1 {
		return nil, fmt.Errorf("%w: missing or incompatible wavelet bank", ErrPhase2DInput)
	}
	if len(in.Bands) > l.MaxScales {
		return nil, ErrPhase2DLimit
	}
	n, err := validatePhase2DVolume(ctx, in.Lowpass, l.MaxPixels)
	if err != nil {
		return nil, err
	}
	if in.Metadata.Dims != in.Lowpass.Dims || in.Metadata.SpacingMM != in.Lowpass.SpacingMM || len(in.Metadata.ScalesMM) != len(in.Bands) {
		return nil, fmt.Errorf("%w: incompatible bank metadata", ErrPhase2DInput)
	}
	previous := 0.0
	for _, sigma := range in.Metadata.ScalesMM {
		if !(sigma >= 1e-9 && sigma <= 1e9 && sigma > previous) {
			return nil, ErrPhase2DInput
		}
		previous = sigma
	}
	if int64(n)*4 > l.MaxWorkingBytes {
		return nil, ErrPhase2DLimit
	}
	for _, band := range in.Bands {
		if _, err := validatePhase2DVolume(ctx, band, l.MaxPixels); err != nil {
			return nil, err
		}
		if band.Dims != in.Lowpass.Dims || band.SpacingMM != in.Lowpass.SpacingMM {
			return nil, fmt.Errorf("%w: incompatible coefficient geometry", ErrPhase2DInput)
		}
	}
	out := &Volume{Dims: in.Lowpass.Dims, SpacingMM: in.Lowpass.SpacingMM, Data: make([]float32, n)}
	for i, v := range in.Lowpass.Data {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		sum := float64(v)
		for _, band := range in.Bands {
			sum += float64(band.Data[i])
		}
		if !phase2DRepresentable(sum) {
			return nil, ErrPhase2DNumeric
		}
		out.Data[i] = float32(sum)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func phase2DContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func phase2DLimits(l Phase2DLimits) (Phase2DLimits, error) {
	if l.MaxPixels < 0 || l.MaxPixels > 1<<20 || l.MaxPaddedPixels < 0 || l.MaxPaddedPixels > 1<<22 || l.MaxScales < 0 || l.MaxScales > 8 || l.MaxWorkingBytes < 0 || l.MaxWorkingBytes > 256<<20 {
		return l, ErrPhase2DInput
	}
	if l.MaxPixels == 0 {
		l.MaxPixels = 1 << 20
	}
	if l.MaxPaddedPixels == 0 {
		l.MaxPaddedPixels = 1 << 22
	}
	if l.MaxScales == 0 {
		l.MaxScales = 8
	}
	if l.MaxWorkingBytes == 0 {
		l.MaxWorkingBytes = 256 << 20
	}
	return l, nil
}

func validatePhase2DVolume(ctx context.Context, src *Volume, maxPixels int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if src == nil || src.Dims[2] != 1 || src.Dims[0] < 2 || src.Dims[1] < 2 {
		return 0, fmt.Errorf("%w: a regular 2D image with both axes >= 2 is required", ErrPhase2DInput)
	}
	n, ok := voxelCount(src.Dims)
	if !ok || n > maxPixels {
		return 0, ErrPhase2DLimit
	}
	if len(src.Data) != n {
		return 0, ErrPhase2DInput
	}
	for _, s := range src.SpacingMM[:2] {
		if !(s >= 1e-9 && s <= 1e9) {
			return 0, fmt.Errorf("%w: physical spacing outside [1e-9,1e9] mm", ErrPhase2DInput)
		}
	}
	if math.IsNaN(src.SpacingMM[2]) || math.IsInf(src.SpacingMM[2], 0) {
		return 0, ErrPhase2DInput
	}
	for i, v := range src.Data {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return 0, ErrPhase2DInput
		}
	}
	return n, nil
}

func admitWavelet2D(ctx context.Context, src *Volume, o Wavelet2DOptions, phase bool) (wavelet2DPlan, error) {
	var p wavelet2DPlan
	l, err := phase2DLimits(o.Limits)
	if err != nil {
		return p, err
	}
	if o.Boundary > WaveletPeriodic || len(o.ScalesMM) < 1 || o.Workers < 0 {
		return p, ErrPhase2DInput
	}
	if len(o.ScalesMM) > l.MaxScales {
		return p, ErrPhase2DLimit
	}
	previous := 0.0
	for _, sigma := range o.ScalesMM {
		if !(sigma >= 1e-9 && sigma <= 1e9 && sigma > previous) {
			return p, fmt.Errorf("%w: increasing physical sigmas in [1e-9,1e9] mm required", ErrPhase2DInput)
		}
		previous = sigma
	}
	p.n, err = validatePhase2DVolume(ctx, src, l.MaxPixels)
	if err != nil {
		return p, err
	}
	p.w, p.h = src.Dims[0], src.Dims[1]
	p.pw, p.ph = p.w, p.h
	if o.Boundary == WaveletPeriodic {
		if p.w&(p.w-1) != 0 || p.h&(p.h-1) != 0 {
			return p, fmt.Errorf("%w: periodic dimensions must be radix-2", ErrPhase2DInput)
		}
	} else {
		var domain [2]int
		var offset [2]int
		for axis, n := range src.Dims[:2] {
			margin := math.Ceil(6 * previous / src.SpacingMM[axis])
			if margin > float64(l.MaxPaddedPixels) || float64(n)+2*margin > float64(l.MaxPaddedPixels) {
				return p, ErrPhase2DLimit
			}
			offset[axis] = int(margin)
			domain[axis] = fft.NextPowerOfTwo(n + 2*offset[axis])
		}
		p.pw, p.ph, p.ox, p.oy = domain[0], domain[1], offset[0], offset[1]
	}
	if int64(p.pw)*int64(p.ph) > int64(l.MaxPaddedPixels) {
		return p, ErrPhase2DLimit
	}
	p.pn = p.pw * p.ph
	// Two complex planes and one complex column per FFT worker.
	p.workers = fft.AxisWorkers(p.pn, o.Workers)
	p.scratch = 32*int64(p.pn) + 16*int64(max(p.pw, p.ph))*int64(p.workers)
	output := int64(4 * p.n * (len(o.ScalesMM) + 1))
	if phase {
		p.scratch += 40 * int64(p.n) // even, Riesz-x, energy, amplitude, dominant amplitude
		output = 17 * int64(p.n)     // four float32 maps and one validity mask
	}
	p.working = p.scratch + output + 8*int64(len(o.ScalesMM))
	if p.working > l.MaxWorkingBytes {
		return p, ErrPhase2DLimit
	}
	return p, ctx.Err()
}

func (p wavelet2DPlan) metadata(src *Volume, o Wavelet2DOptions) Wavelet2DMetadata {
	boundary := "reflect-six-sigma-radix2-periodic-extension"
	if o.Boundary == WaveletPeriodic {
		boundary = "source-periodic"
	}
	return Wavelet2DMetadata{Experimental: true, Family: wavelet2DFamily, Boundary: boundary, Dims: src.Dims, SpacingMM: src.SpacingMM, ScalesMM: append([]float64(nil), o.ScalesMM...), PaddedDims: [2]int{p.pw, p.ph}, CropOffset: [2]int{p.ox, p.oy}, Workers: p.workers, ScratchBytes: p.scratch, WorkingBytes: p.working}
}

func wavelet2DSpectrum(ctx context.Context, src *Volume, p wavelet2DPlan) ([]complex128, []complex128, error) {
	spectrum := make([]complex128, p.pn)
	work := make([]complex128, p.pn)
	for y := 0; y < p.ph; y++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		sy := phase2DReflect(y-p.oy, p.h)
		for x := 0; x < p.pw; x++ {
			spectrum[y*p.pw+x] = complex(float64(src.Data[sy*p.w+phase2DReflect(x-p.ox, p.w)]), 0)
		}
	}
	if err := wavelet2DFFT(ctx, spectrum, p, false); err != nil {
		return nil, nil, err
	}
	return spectrum, work, nil
}

func phase2DReflect(i, n int) int {
	period := 2 * (n - 1)
	i %= period
	if i < 0 {
		i += period
	}
	if i >= n {
		i = period - i
	}
	return i
}

// wavelet2DFFT transforms rows, then columns, on up to p.workers workers.
func wavelet2DFFT(ctx context.Context, a []complex128, p wavelet2DPlan, inverse bool) error {
	if err := fft.TransformAxes(ctx, a, [3]int{p.pw, p.ph, 1}, inverse, p.workers); err != nil {
		return err
	}
	if inverse {
		scale := complex(1/float64(p.pn), 0)
		for i := range a {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			a[i] *= scale
		}
	}
	return ctx.Err()
}

// component 0 is the even band; 1/2 are Riesz x/y. Equal sigmas select lowpass.
// The Riesz multiplier vanishes on its axis's Nyquist line to retain Hermitian
// symmetry of this sampled real-domain transform; DC also vanishes.
func wavelet2DFilter(ctx context.Context, spectrum, work []complex128, src *Volume, p wavelet2DPlan, previous, sigma float64, component int) error {
	for y := 0; y < p.ph; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ky := y
		if ky > p.ph/2 {
			ky -= p.ph
		}
		wy := 2 * math.Pi * float64(ky) / (float64(p.ph) * src.SpacingMM[1])
		for x := 0; x < p.pw; x++ {
			kx := x
			if kx > p.pw/2 {
				kx -= p.pw
			}
			wx := 2 * math.Pi * float64(kx) / (float64(p.pw) * src.SpacingMM[0])
			r2 := wx*wx + wy*wy
			weight := math.Exp(-.5 * sigma * sigma * r2)
			if previous != sigma {
				weight = math.Exp(-.5*previous*previous*r2) - weight
			}
			factor := complex(weight, 0)
			if component != 0 {
				k := wx
				nyquist := x == p.pw/2
				if component == 2 {
					k = wy
					nyquist = y == p.ph/2
				}
				factor = 0
				if r2 > 0 && !nyquist {
					factor = complex(0, -weight*k/math.Sqrt(r2))
				}
			}
			work[y*p.pw+x] = spectrum[y*p.pw+x] * factor
		}
	}
	return wavelet2DFFT(ctx, work, p, true)
}

func wavelet2DCrop(ctx context.Context, a []complex128, src *Volume, p wavelet2DPlan) (*Volume, error) {
	out := &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, p.n)}
	for y := 0; y < p.h; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < p.w; x++ {
			v := real(a[(y+p.oy)*p.pw+x+p.ox])
			if !phase2DRepresentable(v) {
				return nil, ErrPhase2DNumeric
			}
			out.Data[y*p.w+x] = float32(v)
		}
	}
	return out, nil
}

func phase2DRepresentable(v float64) bool { return !math.IsNaN(v) && math.Abs(v) <= math.MaxFloat32 }
