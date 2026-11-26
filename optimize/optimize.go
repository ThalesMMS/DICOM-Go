// Package optimize minimizes functions of a parameter vector, for image
// registration: regular-step gradient descent, L-BFGS, Nelder–Mead
// (amoeba), Powell's direction-set method and the (1+1) evolution
// strategy, with
// per-parameter scales and a windowed convergence monitor. It follows the
// behaviour of ITK's Optimizersv4 module (Apache-2.0) at commit 35b196e1,
// written independently; registration.PhysicalShiftScales estimates the
// scales for a transform.
//
// Unlike ITK, which maximizes the negated metric "derivative", every
// optimizer here minimizes a Function and uses its true gradient.
package optimize

import (
	"errors"
	"fmt"
	"math"
)

// Function is a function to minimize.
type Function interface {
	Value(x []float64) (float64, error)
}

// GradientFunction also returns its gradient ∂f/∂x.
type GradientFunction interface {
	Function
	ValueAndGradient(x []float64) (float64, []float64, error)
}

// Func adapts a plain function.
type Func func(x []float64) (float64, error)

// Value calls f.
func (f Func) Value(x []float64) (float64, error) { return f(x) }

// StopReason tells why an optimizer stopped.
type StopReason string

const (
	StopMaximumIterations  StopReason = "maximum-iterations"
	StopMinimumStepLength  StopReason = "minimum-step-length"
	StopGradientMagnitude  StopReason = "gradient-magnitude"
	StopConvergenceWindow  StopReason = "convergence-window"
	StopParameterTolerance StopReason = "parameter-tolerance"
	StopValueTolerance     StopReason = "value-tolerance"
	StopRadius             StopReason = "radius"
)

// Result is where an optimizer stopped.
type Result struct {
	X           []float64
	Value       float64
	Iterations  int
	Evaluations int
	Stop        StopReason
}

// ErrOptions reports inconsistent optimizer settings.
var ErrOptions = errors.New("optimize: invalid options")

// scalesFor checks scales (nil means all ones) against n parameters.
func scalesFor(scales []float64, n int) ([]float64, error) {
	if scales == nil {
		out := make([]float64, n)
		for i := range out {
			out[i] = 1
		}
		return out, nil
	}
	if len(scales) != n {
		return nil, fmt.Errorf("%w: %d scales for %d parameters", ErrOptions, len(scales), n)
	}
	for _, s := range scales {
		if !(s > 0) || math.IsInf(s, 0) {
			return nil, fmt.Errorf("%w: scale %v", ErrOptions, s)
		}
	}
	return scales, nil
}

// counter counts the evaluations of a function.
type counter struct {
	f     Function
	count int
}

func (c *counter) value(x []float64) (float64, error) {
	c.count++
	return c.f.Value(x)
}

func clone(x []float64) []float64 { return append([]float64(nil), x...) }
