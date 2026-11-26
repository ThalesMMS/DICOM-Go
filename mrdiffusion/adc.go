package mrdiffusion

import (
	"errors"
	"fmt"
	"math"
)

// ErrADC reports inputs an apparent diffusion coefficient cannot be fitted
// to: fewer than two distinct b-values, or images of different sizes.
var ErrADC = errors.New("dicom/mrdiffusion: cannot fit ADC")

// ADCOptions select the voxels FitADC fits.
type ADCOptions struct {
	// Mask, when set, has one entry per voxel; voxels it leaves out are not
	// fitted.
	Mask []bool
	// MinSignal leaves out the voxels whose signal is at or below it at any
	// b-value. Signals at or below zero are always left out (the fit takes
	// their logarithm).
	MinSignal float64
}

// ADCMap is the result of FitADC.
type ADCMap struct {
	// Values is the apparent diffusion coefficient of each voxel in mm²/s,
	// in the order of the input; zero where the voxel was not fitted.
	Values []float64
	// Fitted counts the voxels fitted.
	Fitted int
	// Negative counts the fitted voxels whose signal rose with the b-value
	// (noise); their ADC is reported as zero.
	Negative int
}

// FitADC fits the mono-exponential model S(b) = S0·exp(−b·ADC) to every
// voxel by linear least squares on ln S, the usual definition of an ADC map:
// with two b-values it is ln(S1/S2)/(b2−b1). signals holds one image per
// b-value (bValues in s/mm², several images may share a b-value), all of
// the same length. Images of one b-value acquired along several directions
// can be passed separately or combined first (their geometric mean is the
// trace-weighted image, which gives the same fit).
func FitADC(bValues []float64, signals [][]float32, opts ADCOptions) (ADCMap, error) {
	if len(bValues) != len(signals) {
		return ADCMap{}, fmt.Errorf("%w: %d b-values for %d images", ErrADC, len(bValues), len(signals))
	}
	if len(signals) < 2 {
		return ADCMap{}, fmt.Errorf("%w: %d images", ErrADC, len(signals))
	}
	voxels := len(signals[0])
	meanB := 0.0
	for i, b := range bValues {
		if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 {
			return ADCMap{}, fmt.Errorf("%w: b-value %v", ErrADC, b)
		}
		if len(signals[i]) != voxels {
			return ADCMap{}, fmt.Errorf("%w: image %d has %d voxels, want %d", ErrADC, i, len(signals[i]), voxels)
		}
		meanB += b
	}
	meanB /= float64(len(bValues))
	varianceB := 0.0
	for _, b := range bValues {
		varianceB += (b - meanB) * (b - meanB)
	}
	if varianceB < 1e-9 {
		return ADCMap{}, fmt.Errorf("%w: the b-values are not distinct", ErrADC)
	}
	if opts.Mask != nil && len(opts.Mask) != voxels {
		return ADCMap{}, fmt.Errorf("%w: mask has %d voxels, want %d", ErrADC, len(opts.Mask), voxels)
	}
	minSignal := math.Max(opts.MinSignal, 0)
	out := ADCMap{Values: make([]float64, voxels)}
	logs := make([]float64, len(signals))
voxel:
	for v := 0; v < voxels; v++ {
		if opts.Mask != nil && !opts.Mask[v] {
			continue
		}
		meanLog := 0.0
		for i, image := range signals {
			s := float64(image[v])
			if !(s > minSignal) || math.IsInf(s, 0) {
				continue voxel
			}
			logs[i] = math.Log(s)
			meanLog += logs[i]
		}
		meanLog /= float64(len(logs))
		covariance := 0.0
		for i, b := range bValues {
			covariance += (b - meanB) * (logs[i] - meanLog)
		}
		adc := -covariance / varianceB
		out.Fitted++
		if adc < 0 {
			out.Negative++
			continue
		}
		out.Values[v] = adc
	}
	return out, nil
}
