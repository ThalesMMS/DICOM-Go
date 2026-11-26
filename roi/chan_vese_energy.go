package roi

import (
	"context"
	"fmt"
	"math"
)

// evaluate uses the exact negative transpose of the forward gradient.
// The energy derivative includes dependence of H on phi; derivatives of the
// optimal regional means cancel because each mean minimizes its regional L2.
func (s *cvState) evaluate(ctx context.Context, o ChanVeseOptions, forces bool) (ChanVeseIteration, error) {
	var st ChanVeseIteration
	sumIn, sumOut, massIn, massOut := 0.0, 0.0, 0.0, 0.0
	anchor, haveAnchor := 0.0, false
	for i, p := range s.phi {
		if i%256 == 0 {
			if e := cvCheck(ctx); e != nil {
				return st, e
			}
		}
		if !s.available[i] {
			s.h[i] = 0
			continue
		}
		if !cvFinite(p) {
			return st, fmt.Errorf("%w: Chan-Vese level set", ErrLevelSetInvalidInput)
		}
		h := .5 + math.Atan(p/o.EpsilonMM)/math.Pi
		s.h[i] = h
		massIn += h
		massOut += 1 - h
		// Centering is algebraically identical to a weighted mean, but keeps
		// a constant image exactly constant and avoids summing its DC offset.
		if !haveAnchor {
			anchor, haveAnchor = s.image[i], true
		}
		sumIn += h * (s.image[i] - anchor)
		sumOut += (1 - h) * (s.image[i] - anchor)
		st.AvailableVoxels++
		if p >= 0 {
			st.InsideVoxels++
		}
	}
	if massIn <= 0 || massOut <= 0 {
		return st, fmt.Errorf("%w: Chan-Vese weighted phase empty", ErrLevelSetInvalidInput)
	}
	st.InsideMean = anchor + sumIn/massIn
	st.OutsideMean = anchor + sumOut/massOut
	for i := range s.h {
		if i%256 == 0 {
			if e := cvCheck(ctx); e != nil {
				return st, e
			}
		}
		for a := range s.flux {
			s.flux[a][i] = 0
		}
		if !s.available[i] {
			s.speed[i] = 0
			continue
		}
		coords := [3]int{i % s.size[0], (i / s.size[0]) % s.size[1], i / s.stride[2]}
		magnitude := o.GradientEpsilon * o.GradientEpsilon
		for a := 0; a < 3; a++ {
			if coords[a]+1 < s.size[a] && s.available[i+s.stride[a]] {
				g := (s.h[i+s.stride[a]] - s.h[i]) / s.spacing[a]
				s.flux[a][i] = g
				magnitude += g * g
			}
		}
		norm := math.Sqrt(magnitude)
		for a := range s.flux {
			s.flux[a][i] /= norm
		}
		din, dout := s.image[i]-st.InsideMean, s.image[i]-st.OutsideMean
		fi, fo := o.LambdaInside*din*din, o.LambdaOutside*dout*dout
		st.Energy += (o.Mu*(norm-o.GradientEpsilon) + o.Nu*s.h[i] + fi*s.h[i] + fo*(1-s.h[i])) * s.measure
		s.speed[i] = -o.Nu - fi + fo
	}
	if !cvFinite(st.Energy) || !cvFinite(st.InsideMean) || !cvFinite(st.OutsideMean) {
		return st, fmt.Errorf("%w: Chan-Vese energy overflow", ErrLevelSetInvalidInput)
	}
	if forces {
		for i := range s.h {
			if i%256 == 0 {
				if e := cvCheck(ctx); e != nil {
					return st, e
				}
			}
			if !s.available[i] {
				continue
			}
			coords := [3]int{i % s.size[0], (i / s.size[0]) % s.size[1], i / s.stride[2]}
			div := 0.0
			for a := 0; a < 3; a++ {
				if s.size[a] <= 1 {
					continue // inactive dimension has no gradient; 2D spacing Z may be zero
				}
				div += s.flux[a][i] / s.spacing[a]
				if coords[a] > 0 && s.available[i-s.stride[a]] {
					div -= s.flux[a][i-s.stride[a]] / s.spacing[a]
				}
			}
			s.speed[i] += o.Mu * div
		}
	}
	return st, cvCheck(ctx)
}
