package roi

import (
	"fmt"
	"math"
)

// Independent Welford centered covariance and Cholesky solve. No reference code is
// transcribed. Numeric admission is an explicit finite float64 research profile.
type vectorConfidenceModel struct {
	c, samples                    int
	mean, m2, cov, applied, lower []float64
	delta, values, solve          []float64
}

func newVectorConfidenceModel(c int) vectorConfidenceModel {
	buffer := make([]float64, 4*c*c+4*c)
	take := func(n int) []float64 { out := buffer[:n:n]; buffer = buffer[n:]; return out }
	return vectorConfidenceModel{c: c, mean: take(c), m2: take(c * c), cov: take(c * c),
		applied: take(c * c), lower: take(c * c), delta: take(c), values: take(c), solve: take(c)}
}

func (m *vectorConfidenceModel) reset(g *vectorConfidenceGrower) error {
	if err := g.charge(uint64(m.c + m.c*m.c)); err != nil {
		return err
	}
	clear(m.mean)
	clear(m.m2)
	m.samples = 0
	return nil
}

func (m *vectorConfidenceModel) add(g *vectorConfidenceGrower, value []float64) error {
	if err := g.charge(uint64(4*m.c + 3*m.c*(m.c+1)/2)); err != nil {
		return err
	}
	m.samples++
	n := float64(m.samples)
	factor := (n - 1) / n
	for i := 0; i < m.c; i++ {
		m.delta[i] = value[i] - m.mean[i]
		m.mean[i] += m.delta[i] / n
	}
	// A single triangle avoids asymmetric roundoff; cross moments use the same
	// factor (n-1)/n as the stable univariate centered-square update.
	for i := 0; i < m.c; i++ {
		for j := i; j < m.c; j++ {
			m.m2[i*m.c+j] += m.delta[i] * m.delta[j] * factor
			if !vectorConfidenceFinite(m.m2[i*m.c+j]) {
				return ErrVectorConfidenceNumeric
			}
		}
	}
	return nil
}

func (m *vectorConfidenceModel) covariance(g *vectorConfidenceGrower) error {
	if m.samples < 2 {
		return fmt.Errorf("%w: fewer than two samples", ErrVectorConfidenceCovariance)
	}
	if err := g.charge(uint64(m.c * m.c)); err != nil {
		return err
	}
	denominator := float64(m.samples - 1)
	if g.params.CovarianceNormalization == VectorCovariancePopulation {
		denominator = float64(m.samples)
	}
	for i := 0; i < m.c; i++ {
		for j := i; j < m.c; j++ {
			v := m.m2[i*m.c+j] / denominator
			if !vectorConfidenceFinite(v) {
				return ErrVectorConfidenceNumeric
			}
			m.cov[i*m.c+j], m.cov[j*m.c+i] = v, v
		}
	}
	return nil
}

func (m *vectorConfidenceModel) factor(g *vectorConfidenceGrower) error {
	if err := g.charge(uint64(2*m.c*m.c + 2*m.c*m.c*m.c)); err != nil {
		return err
	}
	copy(m.applied, m.cov)
	clear(m.lower)
	maxDiagonal := 0.0
	for i := 0; i < m.c; i++ {
		if g.params.CovariancePolicy == VectorCovarianceRidge {
			m.applied[i*m.c+i] += g.params.Ridge
		}
		v := m.applied[i*m.c+i]
		if !vectorConfidenceFinite(v) {
			return ErrVectorConfidenceNumeric
		}
		maxDiagonal = math.Max(maxDiagonal, v)
	}
	if maxDiagonal <= 0 {
		return fmt.Errorf("%w: non-positive covariance scale", ErrVectorConfidenceCovariance)
	}
	for i := 0; i < m.c; i++ {
		for j := 0; j <= i; j++ {
			v := m.applied[i*m.c+j]
			for k := 0; k < j; k++ {
				v -= m.lower[i*m.c+k] * m.lower[j*m.c+k]
			}
			if !vectorConfidenceFinite(v) {
				return ErrVectorConfidenceNumeric
			}
			if i == j {
				// Division preserves the relative rule for subnormal scales where
				// multiplying maxDiagonal by the ratio could underflow to zero.
				if !(v > 0) || !(v/maxDiagonal > g.params.MinPivotRatio) {
					return fmt.Errorf("%w: pivot %d fails relative admission", ErrVectorConfidenceCovariance, i)
				}
				m.lower[i*m.c+j] = math.Sqrt(v)
			} else {
				m.lower[i*m.c+j] = v / m.lower[j*m.c+j]
				if !vectorConfidenceFinite(m.lower[i*m.c+j]) {
					return ErrVectorConfidenceNumeric
				}
			}
		}
	}
	return nil
}

func (m *vectorConfidenceModel) distance(g *vectorConfidenceGrower, value []float64) (float64, error) {
	if err := g.charge(uint64(m.c*m.c + 3*m.c)); err != nil {
		return 0, err
	}
	distance := 0.0
	for i := 0; i < m.c; i++ {
		v := value[i] - m.mean[i]
		for j := 0; j < i; j++ {
			v -= m.lower[i*m.c+j] * m.solve[j]
		}
		m.solve[i] = v / m.lower[i*m.c+i]
		if !vectorConfidenceFinite(m.solve[i]) {
			return 0, ErrVectorConfidenceNumeric
		}
		distance = math.Hypot(distance, m.solve[i])
	}
	if !vectorConfidenceFinite(distance) {
		return 0, ErrVectorConfidenceNumeric
	}
	return distance, nil
}
