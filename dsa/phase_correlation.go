package dsa

import (
	"errors"
	"fmt"
	"math"
	"math/cmplx"

	"github.com/ThalesMMS/dicom-go/internal/fft"
)

// Automatic pixel shift (#1072): the mask-to-contrast translation estimated
// by phase correlation (the normalised cross-power spectrum, whose inverse
// transform peaks at the displacement), refined to a fraction of a pixel by
// one of the peak interpolations of ITK's PhaseCorrelationOptimizer. Written
// from their documented behaviour; no ITK or VTK code is copied.

// PeakInterpolation selects how the correlation peak is refined below one
// pixel.
type PeakInterpolation int

const (
	// PeakParabolic fits a parabola through the peak and its two neighbours
	// on each axis.
	PeakParabolic PeakInterpolation = iota
	// PeakCosine fits a cosine through the same three samples, which models
	// the band-limited peak better than a parabola.
	PeakCosine
	// PeakWeightedMeanPhase takes the residual below one pixel from the
	// phase slope of the cross-power spectrum around the integer peak,
	// weighted by spectral power.
	PeakWeightedMeanPhase
	// PeakNone keeps the integer peak.
	PeakNone
)

// String names the interpolation.
func (p PeakInterpolation) String() string {
	switch p {
	case PeakParabolic:
		return "parabolic"
	case PeakCosine:
		return "cosine"
	case PeakWeightedMeanPhase:
		return "weighted-mean-phase"
	case PeakNone:
		return "none"
	default:
		return fmt.Sprintf("PeakInterpolation(%d)", int(p))
	}
}

// ErrFrameTooSmall reports frames too small to register.
var ErrFrameTooSmall = errors.New("dsa: frame too small for phase correlation")

// minPhaseCorrelationSize is the smallest side PhaseCorrelate registers.
const minPhaseCorrelationSize = 8

// PhaseCorrelationOptions tunes PhaseCorrelate.
type PhaseCorrelationOptions struct {
	// Peak is the sub-pixel refinement (parabolic by default).
	Peak PeakInterpolation
	// MaxShift bounds the searched displacement on each axis, in pixels; 0
	// searches up to a quarter of each side.
	MaxShift float64
}

// Registration is an estimated mask translation.
type Registration struct {
	// Shift is the translation in the Mask Sub-pixel Shift convention, to be
	// passed to ApplyShift for the moving frame.
	Shift Shift
	// DX and DY are the same translation as an image displacement, x to the
	// right and y down (Shift.Displacement).
	DX, DY float64
	// Peak is the height of the correlation peak: about 1 for a pure
	// translation, lower as the frames differ otherwise.
	Peak float64
}

// PhaseCorrelate estimates the translation that aligns moving with reference,
// both columns x rows row-major frames in the same units (modality values for
// DSA: the contrast frame as reference and the mask as moving). ApplyShift
// with the result's Shift moves moving onto reference.
//
// Both frames lose their mean and are tapered by a Hann window, then are
// zero-padded to powers of two; the peak of the inverse transform of their
// normalised cross-power spectrum is searched within MaxShift and refined by
// opts.Peak. Unless opts.Peak is PeakNone, moving is then shifted by the
// estimate (ApplyShift) and re-correlated a few times, each pass adding the
// residual peak, which removes most of the pull toward zero of the fixed
// window.
func PhaseCorrelate(reference, moving []float64, columns, rows int, opts PhaseCorrelationOptions) (Registration, error) {
	if !frameSizeMatches(columns, rows, reference, moving) {
		return Registration{}, fmt.Errorf("%w: %d and %d values for %dx%d", ErrFrameSizeMismatch, len(reference), len(moving), columns, rows)
	}
	if columns < minPhaseCorrelationSize || rows < minPhaseCorrelationSize {
		return Registration{}, fmt.Errorf("%w: %dx%d", ErrFrameTooSmall, columns, rows)
	}
	if opts.Peak < PeakParabolic || opts.Peak > PeakNone {
		return Registration{}, fmt.Errorf("dsa: unknown peak interpolation %d", int(opts.Peak))
	}
	width, height := fft.NextPowerOfTwo(columns), fft.NextPowerOfTwo(rows)
	const complex128Bytes = 16
	if width > math.MaxInt/height/complex128Bytes {
		return Registration{}, fmt.Errorf("%w: padded spectrum of %dx%d exceeds addressable size", ErrFrameSizeMismatch, width, height)
	}
	a, err := taperedSpectrum(reference, columns, rows, width, height)
	if err != nil {
		return Registration{}, err
	}
	b, err := taperedSpectrum(moving, columns, rows, width, height)
	if err != nil {
		return Registration{}, err
	}
	limitX, limitY := searchLimit(opts.MaxShift, width, columns), searchLimit(opts.MaxShift, height, rows)
	dx, dy, peak := correlationPeak(a, b, width, height, limitX, limitY, opts.Peak)
	// The window stays put while the content moves, which pulls a sub-pixel
	// estimate toward zero in proportion to the displacement. Re-correlating
	// the mask moved by the running estimate shrinks that residual.
	if opts.Peak != PeakNone {
		for iteration := 0; iteration < phaseRefinementIterations; iteration++ {
			shifted, err := ApplyShift(moving, columns, rows, ShiftFromDisplacement(dx, dy))
			if err != nil {
				return Registration{}, err
			}
			if b, err = taperedSpectrum(shifted, columns, rows, width, height); err != nil {
				return Registration{}, err
			}
			rx, ry, _ := correlationPeak(a, b, width, height, 1, 1, opts.Peak)
			dx, dy = dx+rx, dy+ry
			if math.Abs(rx) < 0.005 && math.Abs(ry) < 0.005 {
				break
			}
		}
	}
	return Registration{Shift: ShiftFromDisplacement(dx, dy), DX: dx, DY: dy, Peak: peak}, nil
}

// phaseRefinementIterations bounds the re-correlations of the moving frame.
const phaseRefinementIterations = 4

// correlationPeak returns the displacement of b onto a, two tapered spectra,
// searched within +-limitX, +-limitY pixels and refined by method, and the
// peak height.
func correlationPeak(a, b []complex128, width, height, limitX, limitY int, method PeakInterpolation) (float64, float64, float64) {
	// cross keeps the unnormalised cross-power for the phase weights; the
	// surface is the inverse transform of its normalised phase.
	cross := make([]complex128, len(a))
	surface := make([]complex128, len(a))
	for i := range a {
		c := a[i] * cmplx.Conj(b[i])
		cross[i] = c
		if magnitude := cmplx.Abs(c); magnitude > 1e-12 {
			surface[i] = c / complex(magnitude, 0)
		}
	}
	fft.Transform2D(surface, width, height, true)
	peakX, peakY, peak := 0, 0, math.Inf(-1)
	for y := -limitY; y <= limitY; y++ {
		row := wrapIndex(y, height) * width
		for x := -limitX; x <= limitX; x++ {
			if value := real(surface[row+wrapIndex(x, width)]); value > peak {
				peakX, peakY, peak = x, y, value
			}
		}
	}
	at := func(x, y int) float64 {
		return real(surface[wrapIndex(y, height)*width+wrapIndex(x, width)])
	}
	dx, dy := float64(peakX), float64(peakY)
	switch method {
	case PeakParabolic:
		dx += parabolicOffset(at(peakX-1, peakY), peak, at(peakX+1, peakY))
		dy += parabolicOffset(at(peakX, peakY-1), peak, at(peakX, peakY+1))
	case PeakCosine:
		dx += cosineOffset(at(peakX-1, peakY), peak, at(peakX+1, peakY))
		dy += cosineOffset(at(peakX, peakY-1), peak, at(peakX, peakY+1))
	case PeakWeightedMeanPhase:
		rx, ry := phaseSlopeOffset(cross, width, height, peakX, peakY)
		dx += rx
		dy += ry
	}
	return dx, dy, peak
}

// taperedSpectrum removes the frame's least-squares plane (a mean and a
// gradient would otherwise correlate with themselves at zero shift through
// the window), applies a separable Hann window and returns the transform of
// the zero-padded result.
func taperedSpectrum(frame []float64, columns, rows, width, height int) ([]complex128, error) {
	var mean, gx, gy float64
	cx, cy := float64(columns-1)/2, float64(rows-1)/2
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			value := frame[y*columns+x]
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, errors.New("dsa: phase correlation frame has a non-finite value")
			}
			mean += value
			gx += value * (float64(x) - cx)
			gy += value * (float64(y) - cy)
		}
	}
	count := float64(len(frame))
	mean /= count
	// Sum of (x - cx)^2 over the frame is rows * columns (columns^2 - 1) / 12.
	gx /= count * (float64(columns)*float64(columns) - 1) / 12
	gy /= count * (float64(rows)*float64(rows) - 1) / 12
	windowX, windowY := hannWindow(columns), hannWindow(rows)
	out := make([]complex128, width*height)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			plane := mean + gx*(float64(x)-cx) + gy*(float64(y)-cy)
			out[y*width+x] = complex((frame[y*columns+x]-plane)*windowX[x]*windowY[y], 0)
		}
	}
	fft.Transform2D(out, width, height, false)
	return out, nil
}

func hannWindow(n int) []float64 {
	window := make([]float64, n)
	for i := range window {
		window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*(float64(i)+0.5)/float64(n))
	}
	return window
}

func searchLimit(maxShift float64, padded, size int) int {
	limit := size / 4
	if maxShift > 0 && maxShift < float64(limit) {
		limit = int(math.Ceil(maxShift))
	}
	return min(max(limit, 1), padded/2-1)
}

// parabolicOffset is the vertex of the parabola through (-1, y0), (0, y1)
// and (1, y2).
func parabolicOffset(y0, y1, y2 float64) float64 {
	denominator := y0 - 2*y1 + y2
	if denominator >= 0 {
		return 0
	}
	return clampHalf((y0 - y2) / (2 * denominator))
}

// cosineOffset is the maximum of the cosine y = A cos(w (x - d)) through the
// same three samples: cos w = (y0 + y2) / (2 y1) and
// tan(w d) = (y2 - y0) / (2 y1 sin w).
func cosineOffset(y0, y1, y2 float64) float64 {
	if y1 <= 0 {
		return 0
	}
	ratio := (y0 + y2) / (2 * y1)
	const edge = 1 - 1e-9
	ratio = min(max(ratio, -edge), edge)
	omega := math.Acos(ratio)
	return clampHalf(math.Atan((y2-y0)/(2*y1*math.Sin(omega))) / omega)
}

// phaseSlopeOffset is the sub-pixel residual of the displacement whose
// integer part is (peakX, peakY): the cross-power phase after removing the
// integer ramp is -2 pi (kx rx / width + ky ry / height), and (rx, ry) is its
// power-weighted least-squares slope over the frequencies up to a quarter of
// each band, where a residual below one pixel cannot wrap the phase.
func phaseSlopeOffset(cross []complex128, width, height, peakX, peakY int) (float64, float64) {
	var sxx, sxy, syy, sxp, syp float64
	for ky := -height / 4; ky <= height/4; ky++ {
		row := wrapIndex(ky, height) * width
		for kx := -width / 4; kx <= width/4; kx++ {
			if kx == 0 && ky == 0 {
				continue
			}
			c := cross[row+wrapIndex(kx, width)]
			weight := cmplx.Abs(c)
			if weight == 0 {
				continue
			}
			fx := 2 * math.Pi * float64(kx) / float64(width)
			fy := 2 * math.Pi * float64(ky) / float64(height)
			// Removing the integer ramp: multiply by exp(+i (fx px + fy py)).
			phase := cmplx.Phase(c * cmplx.Exp(complex(0, fx*float64(peakX)+fy*float64(peakY))))
			// phase ~ -(fx rx + fy ry)
			sxx += weight * fx * fx
			sxy += weight * fx * fy
			syy += weight * fy * fy
			sxp -= weight * fx * phase
			syp -= weight * fy * phase
		}
	}
	determinant := sxx*syy - sxy*sxy
	if determinant <= 0 {
		return 0, 0
	}
	rx := (sxp*syy - syp*sxy) / determinant
	ry := (syp*sxx - sxp*sxy) / determinant
	return min(max(rx, -1), 1), min(max(ry, -1), 1)
}

func clampHalf(value float64) float64 {
	if math.IsNaN(value) {
		return 0
	}
	return min(max(value, -0.5), 0.5)
}

func wrapIndex(i, n int) int {
	i %= n
	if i < 0 {
		i += n
	}
	return i
}
