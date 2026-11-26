package registration

import (
	"math"
	"sync"
)

// evaluateLocal computes V and its gradient for a transform with local
// support (a B-spline deformation, #1184) in two passes, as ITK's Mattes
// metric does for such transforms: the first builds the joint histogram,
// the second weighs every sample's moving-intensity derivative by
//
//	w(x) = Σ_j ln(p(i,j)/p_m(j))·β3′(j − t(x))
//
// over the four moving bins j of its window (i its fixed bin), and adds
// w(x)·∇m·∂T/∂p to the few parameters the sample's support touches. The
// result equals the dense formula of ValueAndDerivative without a
// bins × bins × parameters accumulator, so the cost per sample does not
// grow with the number of control points.
func (m *MattesMutualInformation) evaluateLocal(transform Transform, local localSupportTransform) (float64, []float64, error) {
	parameters := transform.NumberOfParameters()
	if err := checkMattesArraySize(1, parameters); err != nil {
		return 0, nil, err
	}
	bins := m.bins
	total := mattesPartial{joint: make([]float64, bins*bins), fixed: make([]float64, bins)}
	// Chunk partials go back to a free list once merged, so each pass
	// allocates only as many as the budget lets it hold in flight.
	histograms := mattesFreeList[*mattesPartial]{allocate: func() *mattesPartial {
		return &mattesPartial{joint: make([]float64, bins*bins), fixed: make([]float64, bins)}
	}}
	histogramFloats := bins*bins + bins
	reduceChunks(m.evaluator, transform, false, mattesChunksInFlight(m.workspaceBudget(), histogramFloats, histogramFloats), func(samples []sample) *mattesPartial {
		partial := histograms.get()
		for _, s := range samples {
			if s.moving < m.movingMin || s.moving > m.movingMax {
				continue
			}
			partial.count++
			movingTerm := s.moving/m.movingBinSize - m.movingNormMin
			movingBin := clampBin(movingTerm, bins)
			fixedBin := clampBin(s.fixed/m.fixedBinSize-m.fixedNormalMin, bins)
			partial.fixed[fixedBin]++
			for j := movingBin - 1; j <= movingBin+2; j++ {
				partial.joint[fixedBin*bins+j] += cubicBSpline(float64(j) - movingTerm)
			}
		}
		return partial
	}, func(partial *mattesPartial) {
		total.count += partial.count
		for i, v := range partial.joint {
			total.joint[i] += v
		}
		for i, v := range partial.fixed {
			total.fixed[i] += v
		}
		partial.count = 0
		clear(partial.joint)
		clear(partial.fixed)
		histograms.put(partial)
	})
	if total.count == 0 {
		return 0, nil, ErrTooFewSamples
	}
	var sum float64
	for _, v := range total.joint {
		sum += v
	}
	if sum < epsilon {
		return 0, nil, ErrTooFewSamples
	}
	movingMarginal := make([]float64, bins)
	for i := 0; i < bins; i++ {
		for j := 0; j < bins; j++ {
			movingMarginal[j] += total.joint[i*bins+j] / sum
		}
	}
	// ratio holds ln(p(i,j)/p_m(j)) for the cells that count, zero for the
	// others, which the dense formula skips.
	ratio := make([]float64, bins*bins)
	var information float64
	for i := 0; i < bins; i++ {
		fixedProbability := total.fixed[i] / sum
		if fixedProbability <= epsilon {
			continue
		}
		logFixed := math.Log(fixedProbability)
		for j := 0; j < bins; j++ {
			joint := total.joint[i*bins+j] / sum
			if movingMarginal[j] <= epsilon || joint <= epsilon {
				continue
			}
			r := math.Log(joint / movingMarginal[j])
			ratio[i*bins+j] = r
			information += joint * (r - logFixed)
		}
	}
	derivative := make([]float64, parameters)
	partials := mattesFreeList[[]float64]{allocate: func() []float64 { return make([]float64, parameters) }}
	// The merged derivative, the ratio table and the histograms stay live.
	held := parameters + 2*bins*bins + bins
	reduceChunks(m.evaluator, transform, true, mattesChunksInFlight(m.workspaceBudget(), held, parameters), func(samples []sample) []float64 {
		partial := partials.get()
		var sparse sparseJacobian
		for _, s := range samples {
			if s.moving < m.movingMin || s.moving > m.movingMax {
				continue
			}
			movingTerm := s.moving/m.movingBinSize - m.movingNormMin
			movingBin := clampBin(movingTerm, bins)
			fixedBin := clampBin(s.fixed/m.fixedBinSize-m.fixedNormalMin, bins)
			var weight float64
			for j := movingBin - 1; j <= movingBin+2; j++ {
				if r := ratio[fixedBin*bins+j]; r != 0 {
					weight += r * cubicBSplineDerivative(float64(j)-movingTerm)
				}
			}
			if weight == 0 {
				continue
			}
			local.sparseJacobianAt(s.fixedPoint, &sparse)
			if sparse.n == 0 {
				continue
			}
			// g_d = ∇m · (column d of post).
			var g [3]float64
			for d := 0; d < 3; d++ {
				g[d] = weight * (s.gradient.X*sparse.post[0][d] + s.gradient.Y*sparse.post[1][d] + s.gradient.Z*sparse.post[2][d])
			}
			for k := 0; k < sparse.n; k++ {
				at, w := sparse.indices[k], sparse.weights[k]
				partial[at] += w * g[0]
				partial[sparse.stride+at] += w * g[1]
				partial[2*sparse.stride+at] += w * g[2]
			}
		}
		return partial
	}, func(partial []float64) {
		for i, v := range partial {
			derivative[i] += v
		}
		clear(partial)
		partials.put(partial)
	})
	for p := range derivative {
		derivative[p] /= m.movingBinSize * sum
	}
	return -information, derivative, nil
}

// mattesFreeList recycles chunk partials between the workers that fill them
// and the ordered merge that empties them.
type mattesFreeList[T any] struct {
	mu       sync.Mutex
	free     []T
	allocate func() T
}

func (l *mattesFreeList[T]) get() T {
	l.mu.Lock()
	if last := len(l.free) - 1; last >= 0 {
		item := l.free[last]
		l.free = l.free[:last]
		l.mu.Unlock()
		return item
	}
	l.mu.Unlock()
	return l.allocate()
}

func (l *mattesFreeList[T]) put(item T) {
	l.mu.Lock()
	l.free = append(l.free, item)
	l.mu.Unlock()
}
