package optimize

import (
	"fmt"
	"math"
	"math/rand"
)

// OnePlusOne is the (1+1) evolution strategy with covariance adaptation of
// Styner et al., "Parametric estimate of intensity inhomogeneities applied
// to MRI" (IEEE TMI 19(3), 2000), ITK's OnePlusOneEvolutionaryOptimizerv4:
// each iteration tries the parent plus a normally distributed step shaped by
// a matrix A, keeps the better of the two, and grows A along the step after
// a success or shrinks it after a failure. It needs values only and escapes
// shallow local minima; the seed makes a run repeatable.
type OnePlusOne struct {
	// InitialRadius is the initial search radius, A = radius·I (0: 1.01).
	InitialRadius float64
	// GrowthFactor enlarges A after a success (0: 1.05); ShrinkFactor
	// reduces it after a failure (0: GrowthFactor^(−1/4)).
	GrowthFactor, ShrinkFactor float64
	// Epsilon stops once the Frobenius norm of A, in parameter units, is at
	// most this (0: 1.5e-4).
	Epsilon float64
	// MaximumIterations bounds the trials (0: 100).
	MaximumIterations int
	// Scales divide the steps: parameter i moves by step_i/scale_i (nil:
	// all ones).
	Scales []float64
	// Seed seeds the normal generator.
	Seed int64
}

// Minimize runs from x0.
func (o OnePlusOne) Minimize(f Function, x0 []float64) (Result, error) {
	n := len(x0)
	if n == 0 {
		return Result{}, fmt.Errorf("%w: no parameters", ErrOptions)
	}
	scales, err := scalesFor(o.Scales, n)
	if err != nil {
		return Result{}, err
	}
	growth := defaultTo(o.GrowthFactor, 1.05)
	shrink := defaultTo(o.ShrinkFactor, math.Pow(growth, -0.25))
	if growth <= 1 || shrink >= 1 {
		return Result{}, fmt.Errorf("%w: growth %v must exceed 1 and shrink %v stay below it", ErrOptions, growth, shrink)
	}
	epsilon := defaultTo(o.Epsilon, 1.5e-4)
	maxIterations := defaultToInt(o.MaximumIterations, 100)
	radius := defaultTo(o.InitialRadius, 1.01)
	random := rand.New(rand.NewSource(o.Seed))

	c := &counter{f: f}
	parent := clone(x0)
	parentValue, err := c.value(parent)
	if err != nil {
		return Result{}, err
	}
	a := make([]float64, n*n) // row-major
	for i := 0; i < n; i++ {
		a[i*n+i] = radius
	}
	normal := make([]float64, n)
	step := make([]float64, n)
	child := make([]float64, n)
	result := Result{Stop: StopMaximumIterations}
	for iteration := 0; iteration < maxIterations; iteration++ {
		result.Iterations = iteration + 1
		var norm2 float64
		for i := range normal {
			normal[i] = random.NormFloat64()
			norm2 += normal[i] * normal[i]
		}
		for r := 0; r < n; r++ {
			var sum float64
			for k := 0; k < n; k++ {
				sum += a[r*n+k] * normal[k]
			}
			step[r] = sum
			child[r] = parent[r] + sum/scales[r]
		}
		childValue, err := c.value(child)
		if err != nil {
			return Result{}, err
		}
		adjust := shrink
		if childValue < parentValue {
			parent, child = child, parent
			parentValue = childValue
			adjust = growth
		}
		// Stop once the search radius, the Frobenius norm of A in parameter
		// units, is below Epsilon (before this iteration's update, as ITK).
		var frobenius float64
		for r := 0; r < n; r++ {
			for k := 0; k < n; k++ {
				v := a[r*n+k] / scales[r]
				frobenius += v * v
			}
		}
		if math.Sqrt(frobenius) <= epsilon {
			result.Stop = StopRadius
			break
		}
		// A ← A + (adjust − 1)·(A·n)·nᵀ/|n|²: scale A along the tried step.
		if norm2 > 0 {
			alpha := (adjust - 1) / norm2
			for r := 0; r < n; r++ {
				for k := 0; k < n; k++ {
					a[r*n+k] += alpha * step[r] * normal[k]
				}
			}
		}
	}
	result.X, result.Value, result.Evaluations = parent, parentValue, c.count
	return result, nil
}
