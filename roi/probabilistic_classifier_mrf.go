package roi

import (
	"context"
	"math"
)

func probabilityMRF(ctx context.Context, w probabilityWork, r *ProbabilisticResult, unary []float64) error {
	// Unary is already normalized in the log domain, before exponentiation.
	// Only an actual zero prior is -Inf; positive-prior underflow is recoverable.
	next := make([]float64, len(r.Posteriors))
	scores := make([]float64, w.k)
	nx, ny := w.in.Dimensions[0], w.in.Dimensions[1]
	plane := nx * ny
	for iteration := 0; iteration < w.o.MRF.Iterations; iteration++ {
		change := 0.0
		for i := 0; i < w.n; i++ {
			if i%128 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if !w.valid[i] {
				continue
			}
			copy(scores, unary[i*w.k:(i+1)*w.k])
			x, y, z := i%nx, (i/nx)%ny, i/plane
			for axis, step := range [3]int{1, nx, plane} {
				coord := [3]int{x, y, z}[axis]
				for _, direction := range [2]int{-1, 1} {
					if coord+direction < 0 || coord+direction >= w.in.Dimensions[axis] {
						continue
					}
					neighbor := i + direction*step
					if !w.valid[neighbor] {
						continue
					}
					weight := w.o.MRF.Beta / w.in.SpacingMM[axis]
					if w.o.MRF.ContrastSigma > 0 {
						d := (w.in.Values[i] - w.in.Values[neighbor]) / w.o.MRF.ContrastSigma
						weight *= math.Exp(-.5 * d * d)
					}
					for j := range scores {
						scores[j] += weight * r.Posteriors[neighbor*w.k+j]
					}
				}
			}
			mx := math.Inf(-1)
			for _, v := range scores {
				mx = math.Max(mx, v)
			}
			sum := 0.0
			for j, v := range scores {
				scores[j] = math.Exp(v - mx)
				sum += scores[j]
			}
			for j := range scores {
				index := i*w.k + j
				p := (1-w.o.MRF.Damping)*r.Posteriors[index] + w.o.MRF.Damping*scores[j]/sum
				next[index] = p
				change = math.Max(change, math.Abs(p-r.Posteriors[index]))
			}
		}
		r.Posteriors, next = next, r.Posteriors
		r.MRFIterations = iteration + 1
		if change <= w.o.MRF.Tolerance {
			r.MRFConverged = true
			break
		}
	}
	return ctx.Err()
}
