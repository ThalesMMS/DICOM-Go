package optimize

import (
	"context"
	"fmt"
	"math"
)

type lbfgsbSearch struct {
	ctx                                  context.Context
	f                                    GradientFunction
	config                               lbfgsbConfig
	evaluations                          int
	lo, hi, trialX, trialG, bestX, bestG []float64
}

func (s *lbfgsbSearch) exhausted() bool { return s.evaluations >= s.config.evaluations }
func (s *lbfgsbSearch) evaluate(x, g []float64) (float64, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if s.exhausted() {
		return 0, fmt.Errorf("%w: exhausted L-BFGS-B evaluator", ErrOptions)
	}
	s.evaluations++
	value, gradient, err := s.f.ValueAndGradient(x)
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if err != nil {
		return 0, err
	}
	if math.IsNaN(value) || math.IsInf(value, -1) {
		return 0, fmt.Errorf("%w: non-finite L-BFGS-B value", ErrOptions)
	}
	if math.IsInf(value, 1) {
		return value, nil
	}
	if len(gradient) != len(g) {
		return 0, fmt.Errorf("%w: L-BFGS-B gradient length", ErrOptions)
	}
	for i, v := range gradient {
		if i&1023 == 0 {
			if err := s.ctx.Err(); err != nil {
				return 0, err
			}
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("%w: non-finite L-BFGS-B gradient", ErrOptions)
		}
		g[i] = v
	}
	return value, nil
}

// run prefers strong Wolfe points, admits a sufficient-decrease bound endpoint,
// and keeps the best Armijo point when the finite search budget is exhausted.
// Scalar bracket records never retain trial vectors; one best pair owns copies.
func (s *lbfgsbSearch) run(x []float64, f0, slope0 float64, d []float64, initial float64) (float64, bool, error) {
	maximum := math.Inf(1)
	for i, di := range d {
		if i&1023 == 0 {
			if err := s.ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		if di > 0 {
			maximum = math.Min(maximum, (s.hi[i]-x[i])/di)
		} else if di < 0 {
			maximum = math.Min(maximum, (s.lo[i]-x[i])/di)
		}
	}
	alpha := math.Min(initial, maximum)
	if !(alpha > 0) || math.IsNaN(alpha) {
		return 0, false, nil
	}
	bestValue := math.Inf(1)
	haveBest := false
	at := func(step float64) (trial, error) {
		t := trial{step: step, value: math.Inf(1)}
		for i := range x {
			if i&1023 == 0 {
				if err := s.ctx.Err(); err != nil {
					return t, err
				}
			}
			// alpha is bounded analytically; clamp only its endpoint roundoff.
			s.trialX[i] = lbfgsbClamp(x[i]+step*d[i], s.lo[i], s.hi[i])
			if math.IsNaN(s.trialX[i]) || math.IsInf(s.trialX[i], 0) {
				return t, nil
			}
		}
		v, err := s.evaluate(s.trialX, s.trialG)
		if err != nil {
			return t, err
		}
		t.value = v
		if !math.IsInf(v, 1) {
			t.slope, err = lbfgsbDot(s.ctx, s.trialG, d)
			if err != nil {
				return t, err
			}
		}
		return t, nil
	}
	sufficient := func(t trial) bool { return !math.IsInf(t.value, 1) && t.value <= f0+wolfeDecrease*t.step*slope0 }
	keep := func(t trial) {
		if sufficient(t) && t.value < f0 && t.value < bestValue {
			copy(s.bestX, s.trialX)
			copy(s.bestG, s.trialG)
			bestValue = t.value
			haveBest = true
		}
	}
	good := func(t trial) bool {
		return sufficient(t) && (math.Abs(t.slope) <= -wolfeCurvature*slope0 || t.step == maximum)
	}
	previous := trial{step: 0, value: f0, slope: slope0}
	var low, high trial
	zoom := false
	used := 0
	for used < s.config.lineEvaluations && !s.exhausted() {
		if err := s.ctx.Err(); err != nil {
			return 0, false, err
		}
		t, err := at(alpha)
		used++
		if err != nil {
			return 0, false, err
		}
		keep(t)
		if !sufficient(t) || (used > 1 && t.value >= previous.value) {
			low, high, zoom = previous, t, true
			break
		}
		if good(t) {
			// Return the accepted trial even when objective-value rounding
			// made keep's strict improvement test fail. Armijo history is a
			// fallback only; it is not the accepted Wolfe/endpoint state.
			copy(s.bestX, s.trialX)
			copy(s.bestG, s.trialG)
			return t.value, true, nil
		}
		if t.slope >= 0 {
			low, high, zoom = t, previous, true
			break
		}
		previous = t
		if alpha == maximum {
			break
		}
		next := math.Min(2*alpha, maximum)
		if !(next > alpha) || math.IsInf(next, 0) {
			break
		}
		alpha = next
	}
	for zoom && used < s.config.lineEvaluations && !s.exhausted() {
		if err := s.ctx.Err(); err != nil {
			return 0, false, err
		}
		alpha = interpolateStep(low, high)
		if !(alpha > 0) || alpha == low.step || alpha == high.step {
			break
		}
		t, err := at(alpha)
		used++
		if err != nil {
			return 0, false, err
		}
		keep(t)
		if !sufficient(t) || t.value >= low.value {
			high = t
			continue
		}
		if good(t) {
			copy(s.bestX, s.trialX)
			copy(s.bestG, s.trialG)
			return t.value, true, nil
		}
		if t.slope*(high.step-low.step) >= 0 {
			high = low
		}
		low = t
		if math.Abs(high.step-low.step) <= 1e-14*math.Max(math.Abs(low.step), 1e-300) {
			break
		}
	}
	return bestValue, haveBest, nil
}
