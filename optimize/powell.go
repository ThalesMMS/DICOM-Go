package optimize

import (
	"fmt"
	"math"
)

// Powell is Powell's direction-set method (ITK's PowellOptimizerv4): line
// minimizations along a set of directions, starting with the scaled
// parameter axes, each bracketed and refined by Brent's method, replacing
// the direction of largest decrease with the overall displacement of an
// iteration. It needs values only.
type Powell struct {
	// StepLength is the initial bracketing step along a direction, in
	// scaled units (0: 1).
	StepLength float64
	// StepTolerance ends a line search once the bracket is narrower
	// (0: 1e-6); ValueTolerance ends the optimization once an iteration
	// improves the value by less (0: 1e-6).
	StepTolerance, ValueTolerance float64
	// MaximumIterations and MaximumLineIterations bound the iterations and
	// the Brent steps of one line search (0: 100 each).
	MaximumIterations, MaximumLineIterations int
	// Scales define search coordinates u_i = x_i * scale_i: one search
	// unit changes parameter i by 1/scale_i (nil: all ones).
	Scales []float64
}

// Minimize runs from x0.
func (o Powell) Minimize(f Function, x0 []float64) (Result, error) {
	n := len(x0)
	if n == 0 {
		return Result{}, fmt.Errorf("%w: no parameters", ErrOptions)
	}
	scales, err := scalesFor(o.Scales, n)
	if err != nil {
		return Result{}, err
	}
	step := defaultTo(o.StepLength, 1)
	stepTolerance := defaultTo(o.StepTolerance, 1e-6)
	valueTolerance := defaultTo(o.ValueTolerance, 1e-6)
	maxIterations := defaultToInt(o.MaximumIterations, 100)
	maxLine := defaultToInt(o.MaximumLineIterations, 100)

	c := &counter{f: f}
	// The search runs in scaled coordinates u = x*scale, so one search
	// unit changes parameter i by 1/scale_i.
	toX := func(u []float64) []float64 {
		x := make([]float64, n)
		for i := range x {
			x[i] = u[i] / scales[i]
		}
		return x
	}
	value := func(u []float64) (float64, error) { return c.value(toX(u)) }
	u := make([]float64, n)
	for i := range u {
		u[i] = x0[i] * scales[i]
	}
	current, err := value(u)
	if err != nil {
		return Result{}, err
	}
	directions := make([][]float64, n)
	for i := range directions {
		directions[i] = make([]float64, n)
		directions[i][i] = 1
	}
	result := Result{Stop: StopMaximumIterations}
	for iteration := 0; iteration < maxIterations; iteration++ {
		result.Iterations = iteration + 1
		start, startValue := clone(u), current
		largestDecrease, largestIndex := 0.0, 0
		for i, direction := range directions {
			before := current
			u, current, err = lineMinimize(value, u, current, direction, step, stepTolerance, maxLine)
			if err != nil {
				return Result{}, err
			}
			if decrease := before - current; decrease > largestDecrease {
				largestDecrease, largestIndex = decrease, i
			}
		}
		if 2*(startValue-current) <= valueTolerance*(math.Abs(startValue)+math.Abs(current))+1e-20 {
			result.Stop = StopValueTolerance
			break
		}
		// Replace the direction of largest decrease with the displacement,
		// when the extrapolated point suggests it helps (Powell's test).
		displacement := make([]float64, n)
		extrapolated := make([]float64, n)
		for i := range displacement {
			displacement[i] = u[i] - start[i]
			extrapolated[i] = 2*u[i] - start[i]
		}
		extrapolatedValue, err := value(extrapolated)
		if err != nil {
			return Result{}, err
		}
		if extrapolatedValue < startValue {
			t := 2 * (startValue - 2*current + extrapolatedValue) * sq(startValue-current-largestDecrease)
			t -= largestDecrease * sq(startValue-extrapolatedValue)
			if t < 0 {
				u, current, err = lineMinimize(value, u, current, displacement, step, stepTolerance, maxLine)
				if err != nil {
					return Result{}, err
				}
				directions[largestIndex] = directions[n-1]
				directions[n-1] = displacement
			}
		}
	}
	result.X, result.Value, result.Evaluations = toX(u), current, c.count
	return result, nil
}

func sq(v float64) float64 { return v * v }

func defaultTo(v, fallback float64) float64 {
	if v > 0 {
		return v
	}
	return fallback
}

func defaultToInt(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

// lineMinimize minimizes value along u + t·direction: it brackets a
// minimum from t = 0 with steps of step, then refines it with Brent's
// method until the bracket is narrower than tolerance.
func lineMinimize(value func([]float64) (float64, error), u []float64, current float64, direction []float64, step, tolerance float64, maxIterations int) ([]float64, float64, error) {
	at := func(t float64) (float64, error) {
		point := make([]float64, len(u))
		for i := range point {
			point[i] = u[i] + t*direction[i]
		}
		return value(point)
	}
	a, b, c, fb, err := bracket(at, current, step)
	if err != nil {
		return nil, 0, err
	}
	t, ft, err := brent(at, a, b, c, fb, tolerance, maxIterations)
	if err != nil {
		return nil, 0, err
	}
	if ft >= current {
		return u, current, nil
	}
	moved := make([]float64, len(u))
	for i := range moved {
		moved[i] = u[i] + t*direction[i]
	}
	return moved, ft, nil
}

const golden = 1.618033988749895

// bracket finds a < b < c (or c < b < a) with f(b) ≤ f(a), f(c), starting
// from 0 and step, by golden-ratio expansion.
func bracket(f func(float64) (float64, error), f0, step float64) (a, b, c, fb float64, err error) {
	var fa, fc float64
	a, fa = 0, f0
	b = step
	if fb, err = f(b); err != nil {
		return
	}
	if fb > fa {
		a, b, fa, fb = b, a, fb, fa
	}
	c = b + golden*(b-a)
	if fc, err = f(c); err != nil {
		return
	}
	for i := 0; fc < fb && i < 100; i++ {
		a, b, fa, fb = b, c, fb, fc
		c = b + golden*(b-a)
		if fc, err = f(c); err != nil {
			return
		}
	}
	return
}

// brent refines a bracketed minimum (Brent, Algorithms for Minimization
// without Derivatives, 1973): parabolic steps where they behave, golden
// sections otherwise.
func brent(f func(float64) (float64, error), a, b, c, fb, tolerance float64, maxIterations int) (float64, float64, error) {
	const cgold = 0.3819660112501051
	low, high := math.Min(a, c), math.Max(a, c)
	x, w, v := b, b, b
	fx, fw, fv := fb, fb, fb
	var d, e float64
	for i := 0; i < maxIterations; i++ {
		middle := (low + high) / 2
		tol1 := tolerance*math.Abs(x) + 1e-12
		tol2 := 2 * tol1
		if math.Abs(x-middle) <= tol2-(high-low)/2 {
			break
		}
		parabolic := false
		if math.Abs(e) > tol1 {
			r := (x - w) * (fx - fv)
			q := (x - v) * (fx - fw)
			p := (x-v)*q - (x-w)*r
			q = 2 * (q - r)
			if q > 0 {
				p = -p
			}
			q = math.Abs(q)
			if math.Abs(p) < math.Abs(q*e/2) && p > q*(low-x) && p < q*(high-x) {
				e, d = d, p/q
				if u := x + d; u-low < tol2 || high-u < tol2 {
					d = math.Copysign(tol1, middle-x)
				}
				parabolic = true
			}
		}
		if !parabolic {
			if x >= middle {
				e = low - x
			} else {
				e = high - x
			}
			d = cgold * e
		}
		u := x + d
		if math.Abs(d) < tol1 {
			u = x + math.Copysign(tol1, d)
		}
		fu, err := f(u)
		if err != nil {
			return 0, 0, err
		}
		if fu <= fx {
			if u >= x {
				low = x
			} else {
				high = x
			}
			v, w, x = w, x, u
			fv, fw, fx = fw, fx, fu
		} else {
			if u < x {
				low = u
			} else {
				high = u
			}
			if fu <= fw || w == x {
				v, w, fv, fw = w, u, fw, fu
			} else if fu <= fv || v == x || v == w {
				v, fv = u, fu
			}
		}
	}
	return x, fx, nil
}
