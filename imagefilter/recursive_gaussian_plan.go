package imagefilter

import (
	"fmt"
	"math"
)

// VesselnessSlabMarginSigmas is the line extension used by Vesselness before
// evaluating the core of a z slab. Device backends must preserve this margin.
const VesselnessSlabMarginSigmas = vesselnessMarginSigmas

// RecursiveGaussianPole describes one complex recursion of a normalized
// Gaussian derivative. Values are copied metadata, without source samples or
// scratch buffers, for backends that evaluate the filter on another device.
type RecursiveGaussianPole struct {
	Pole, Causal, Anticausal           complex128
	CausalBoundary, AnticausalBoundary complex128
}

// RecursiveGaussianKernel is the two-pole line filter used by RecursiveGaussian
// and Vesselness. For input x[j], evaluate y[j] = Causal*x[j] + Pole*y[j-1]
// forwards and w[j] = Pole*(Anticausal*x[j+1] + w[j+1]) backwards. Initialize
// y[-1] = CausalBoundary*x[0] and w[last] = AnticausalBoundary*x[last],
// replicating the endpoint samples outside the line. The result is the sum of
// the real parts of both poles' y[j] and w[j]. Each completed axis pass rounds
// that sum to float32. The library uses float64 intermediate recursion states.
type RecursiveGaussianKernel struct {
	Poles [2]RecursiveGaussianPole
}

// PlanRecursiveGaussian returns filter coefficients without accessing a volume.
// Sigma is in samples; order is 0, 1 or 2, and scale multiplies the derivative.
// Vesselness uses scale=(sigmaMM/spacingMM)^order for its normalized Hessian.
// Zero and negative finite scales are accepted, as for a scaled linear filter.
func PlanRecursiveGaussian(sigmaSamples float64, order int, scale float64) (RecursiveGaussianKernel, error) {
	var result RecursiveGaussianKernel
	if !(sigmaSamples > 0) || math.IsInf(sigmaSamples, 0) || math.IsNaN(scale) || math.IsInf(scale, 0) {
		return result, fmt.Errorf("%w: recursive Gaussian parameters", ErrInvalidVolume)
	}
	k, err := newRecursiveKernel(sigmaSamples, order, scale)
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrInvalidVolume, err)
	}
	for i := range result.Poles {
		p := RecursiveGaussianPole{k.z[i], k.c[i], k.d[i], k.cInit[i], k.dInit[i]}
		for _, value := range [...]complex128{p.Pole, p.Causal, p.Anticausal, p.CausalBoundary, p.AnticausalBoundary} {
			if math.IsNaN(real(value)) || math.IsNaN(imag(value)) || math.IsInf(real(value), 0) || math.IsInf(imag(value), 0) {
				return RecursiveGaussianKernel{}, fmt.Errorf("%w: non-finite recursive Gaussian coefficients", ErrInvalidVolume)
			}
		}
		result.Poles[i] = p
	}
	return result, nil
}
