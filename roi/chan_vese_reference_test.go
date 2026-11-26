package roi

import (
	"context"
	"math"
	"testing"
)

// Independent direct energy: enumerates coordinate neighbors, does not reuse
// evaluate, flux, or divergence. The mean minimizers are explicitly summed.
func cvReferenceEnergy1436(p, image []float64, nx, ny, nz int, h [3]float64, o ChanVeseOptions) float64 {
	H := make([]float64, len(p))
	mi, mo, si, so := 0.0, 0.0, 0.0, 0.0
	for i, v := range p {
		H[i] = .5 + math.Atan(v/o.EpsilonMM)/math.Pi
		mi += H[i]
		mo += 1 - H[i]
		si += H[i] * image[i]
		so += (1 - H[i]) * image[i]
	}
	ci, co := si/mi, so/mo
	sum := 0.0
	for z := 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				i := (z*ny+y)*nx + x
				g2 := o.GradientEpsilon * o.GradientEpsilon
				for a, q := range [3][3]int{{x + 1, y, z}, {x, y + 1, z}, {x, y, z + 1}} {
					if q[0] >= nx || q[1] >= ny || q[2] >= nz {
						continue
					}
					j := (q[2]*ny+q[1])*nx + q[0]
					d := (H[j] - H[i]) / h[a]
					g2 += d * d
				}
				di, do := image[i]-ci, image[i]-co
				sum += o.Mu*(math.Sqrt(g2)-o.GradientEpsilon) + o.Nu*H[i] + o.LambdaInside*H[i]*di*di + o.LambdaOutside*(1-H[i])*do*do
			}
		}
	}
	measure := h[0] * h[1]
	if nz > 1 {
		measure *= h[2]
	}
	return sum * measure
}
func TestChanVeseIndependentEnergyGradient(t *testing.T) {
	for _, nz := range []int{1, 2} {
		o := DefaultChanVeseOptions()
		o.EpsilonMM = .7
		o.GradientEpsilon = .03
		o.Mu = .23
		o.Nu = .17
		o.LambdaInside = .8
		o.LambdaOutside = 1.2
		s := cvState{n: 4 * nz, size: [3]int{2, 2, nz}, stride: [3]int{1, 2, 4}, spacing: [3]float64{.8, 1.3, 2}, measure: .8 * 1.3, image: make([]float64, 4*nz), phi: make([]float64, 4*nz), h: make([]float64, 4*nz), speed: make([]float64, 4*nz), available: make([]bool, 4*nz)}
		if nz > 1 {
			s.measure *= 2
		} else {
			s.spacing[2] = 0 // allowed 2D contract; old divergence generated 0/0
		}
		for a := range s.flux {
			s.flux[a] = make([]float64, s.n)
		}
		for i := range s.image {
			s.image[i] = float64(i%3) * .4
			s.phi[i] = float64(i)*.3 - .8
			s.available[i] = true
		}
		st, e := s.evaluate(context.Background(), o, true)
		if e != nil {
			t.Fatal(e)
		}
		want := cvReferenceEnergy1436(s.phi, s.image, 2, 2, nz, s.spacing, o)
		if math.Abs(st.Energy-want) > 1e-12 {
			t.Fatal(st.Energy, want)
		}
		for i := range s.phi {
			v := s.phi[i]
			d := 1e-6
			s.phi[i] = v + d
			plus := cvReferenceEnergy1436(s.phi, s.image, 2, 2, nz, s.spacing, o)
			s.phi[i] = v - d
			minus := cvReferenceEnergy1436(s.phi, s.image, 2, 2, nz, s.spacing, o)
			s.phi[i] = v
			derivative := (plus - minus) / (2 * d)
			delta := o.EpsilonMM / (math.Pi * (o.EpsilonMM*o.EpsilonMM + v*v))
			expected := -s.speed[i] * delta * s.measure
			if !cvFinite(derivative) || !cvFinite(expected) || math.Abs(derivative-expected) > 2e-6 {
				t.Fatalf("i%d derivative%g speedgradient%g", i, derivative, expected)
			}
		}
	}
}
func TestChanVeseIndependentTwoRegionSolution(t *testing.T) {
	// For separated noiseless values, zero perimeter/area weights, the exact
	// optimum has zero data error and labels equal the two input classes.
	f := cvFixture1436(12, 12, 1, [3]float64{1, 1, 0}, false, false, false)
	o := DefaultChanVeseOptions()
	o.Mu = 0
	o.MaxIterations = 800
	o.MaximumRMSChangeMM = 1e-7
	r, e := ChanVese(nil, f.v, f.init, o)
	if e != nil {
		t.Fatal(e)
	}
	if cvDice1436(f, r) != 1 {
		t.Fatal("analytic partition mismatch", cvDice1436(f, r))
	}
	if len(r.History) != r.Iterations+1 {
		t.Fatal("history incomplete")
	}
	for k, x := range r.History {
		if x.Iteration != k || !cvFinite(x.Energy) || x.Energy < 0 {
			t.Fatal(x)
		}
	}
}
