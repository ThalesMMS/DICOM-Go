package optimize

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// NelderMead is the downhill simplex method (Nelder and Mead, 1965; ITK's
// AmoebaOptimizerv4 over vnl_amoeba): it needs values only, which suits
// metrics without gradients or with noisy ones.
type NelderMead struct {
	// InitialSimplexDelta offsets the starting point along each (scaled)
	// parameter to build the first simplex; nil uses 5% of each parameter
	// whose magnitude exceeds 0.00025, and 0.00025 otherwise.
	InitialSimplexDelta []float64
	// ParametersTolerance and FunctionTolerance stop once the simplex is
	// narrower than the first in every parameter (between consecutive
	// vertices by value) and its values span less than the second (0: 1e-8
	// and 1e-4; negative is an error).
	ParametersTolerance, FunctionTolerance float64
	// MaximumIterations bounds the evaluations of the search (0: 500).
	MaximumIterations int
	// Scales multiply the parameters, so the simplex works on x·scale
	// (nil: all ones).
	Scales []float64
	// Restarts restarts the search from its solution, with the simplex
	// halved each time and flipped at random (seeded by Seed), until two
	// successive solutions agree within the tolerances or the evaluations
	// run out, which escapes a local minimum next to the start.
	Restarts bool
	Seed     int64
}

// Minimize runs from x0.
func (o NelderMead) Minimize(f Function, x0 []float64) (Result, error) {
	n := len(x0)
	if n == 0 {
		return Result{}, fmt.Errorf("%w: no parameters", ErrOptions)
	}
	if o.InitialSimplexDelta != nil && len(o.InitialSimplexDelta) != n {
		return Result{}, fmt.Errorf("%w: %d simplex deltas for %d parameters", ErrOptions, len(o.InitialSimplexDelta), n)
	}
	if o.ParametersTolerance < 0 || o.FunctionTolerance < 0 {
		return Result{}, fmt.Errorf("%w: negative tolerance", ErrOptions)
	}
	scales, err := scalesFor(o.Scales, n)
	if err != nil {
		return Result{}, err
	}
	xTolerance := defaultTo(o.ParametersTolerance, 1e-8)
	fTolerance := defaultTo(o.FunctionTolerance, 1e-4)
	maxIterations := defaultToInt(o.MaximumIterations, 500)

	c := &counter{f: Func(func(u []float64) (float64, error) {
		x := make([]float64, n)
		for i := range x {
			x[i] = u[i] / scales[i]
		}
		return f.Value(x)
	})}
	u := make([]float64, n)
	delta := make([]float64, n)
	for i := range u {
		u[i] = x0[i] * scales[i]
		switch {
		case o.InitialSimplexDelta != nil:
			delta[i] = o.InitialSimplexDelta[i]
		case math.Abs(u[i]) > 0.00025:
			delta[i] = 0.05 * u[i]
		default:
			delta[i] = 0.00025
		}
	}
	best, err := o.search(c, u, delta, xTolerance, fTolerance, maxIterations)
	if err != nil {
		return Result{}, err
	}
	if o.Restarts {
		random := rand.New(rand.NewSource(o.Seed))
		for round := 1; c.count < maxIterations; round++ {
			sign := 1.0
			if random.Intn(2) == 0 {
				sign = -1
			}
			for i := range delta {
				delta[i] *= sign / math.Pow(2, float64(round))
			}
			next, err := o.search(c, best.x, delta, xTolerance, fTolerance, maxIterations-c.count)
			if err != nil {
				return Result{}, err
			}
			converged := math.Abs(best.value-next.value) < fTolerance
			for i := range next.x {
				converged = converged && math.Abs(best.x[i]-next.x[i]) < xTolerance
			}
			if next.value < best.value {
				best = next
			}
			if converged {
				break
			}
		}
	}
	x := make([]float64, n)
	for i := range x {
		x[i] = best.x[i] / scales[i]
	}
	stop := StopMaximumIterations
	if best.converged {
		stop = StopParameterTolerance
	}
	return Result{X: x, Value: best.value, Iterations: c.count, Evaluations: c.count, Stop: stop}, nil
}

type vertex struct {
	x     []float64
	value float64
}

type searchResult struct {
	vertex
	converged bool
}

// search runs one simplex search from u with the given offsets, for at
// most budget evaluations.
func (o NelderMead) search(c *counter, u, delta []float64, xTolerance, fTolerance float64, budget int) (searchResult, error) {
	n := len(u)
	start := c.count
	simplex := make([]vertex, n+1)
	evaluate := func(x []float64) (vertex, error) {
		v, err := c.value(x)
		return vertex{x: x, value: v}, err
	}
	var err error
	if simplex[0], err = evaluate(clone(u)); err != nil {
		return searchResult{}, err
	}
	for i := 0; i < n; i++ {
		x := clone(u)
		x[i] += delta[i]
		if simplex[i+1], err = evaluate(x); err != nil {
			return searchResult{}, err
		}
	}
	point := func(centroid, toward []float64, t float64) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = centroid[i] + t*(toward[i]-centroid[i])
		}
		return out
	}
	sortSimplex := func() {
		sort.SliceStable(simplex, func(a, b int) bool { return simplex[a].value < simplex[b].value })
	}
	sortSimplex()
	for c.count-start < budget {
		if diameter(simplex) < xTolerance && simplex[n].value-simplex[0].value < fTolerance {
			return searchResult{vertex: simplex[0], converged: true}, nil
		}
		centroid := make([]float64, n)
		for _, v := range simplex[:n] {
			for i := range centroid {
				centroid[i] += v.x[i] / float64(n)
			}
		}
		worst := simplex[n]
		reflected, err := evaluate(point(centroid, worst.x, -1))
		if err != nil {
			return searchResult{}, err
		}
		switch {
		case reflected.value < simplex[0].value:
			expanded, err := evaluate(point(centroid, worst.x, -2))
			if err != nil {
				return searchResult{}, err
			}
			if expanded.value < reflected.value {
				simplex[n] = expanded
			} else {
				simplex[n] = reflected
			}
		case reflected.value < simplex[n-1].value:
			simplex[n] = reflected
		default:
			// Contract toward the better of the worst and reflected points.
			toward := worst
			if reflected.value < worst.value {
				toward = reflected
			}
			contracted, err := evaluate(point(centroid, toward.x, 0.5))
			if err != nil {
				return searchResult{}, err
			}
			if contracted.value < toward.value {
				simplex[n] = contracted
				break
			}
			// Shrink every vertex toward the best.
			for i := 1; i <= n; i++ {
				if simplex[i], err = evaluate(point(simplex[0].x, simplex[i].x, 0.5)); err != nil {
					return searchResult{}, err
				}
			}
		}
		sortSimplex()
	}
	return searchResult{vertex: simplex[0]}, nil
}

// diameter is vnl_amoeba's simplex size: the largest parameter difference
// between vertices adjacent in value order.
func diameter(simplex []vertex) float64 {
	var out float64
	for i := 0; i+1 < len(simplex); i++ {
		for k := range simplex[i].x {
			out = math.Max(out, math.Abs(simplex[i].x[k]-simplex[i+1].x[k]))
		}
	}
	return out
}
