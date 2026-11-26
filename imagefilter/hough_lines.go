package imagefilter

import (
	"context"
	"math"
)

// HoughLineOptions uses aligned inclusive signed-rho bins in mm and aligned
// half-open normal-angle bins in radians [ThetaMin,ThetaMax), within [0,pi).
// Nonmaximum suppression compares physical rho and normal angle modulo pi,
// accounting for the sign reversal across the 0/pi seam.
type HoughLineOptions struct {
	Hough2DOptions
	RhoMinMM, RhoMaxMM, RhoStepMM float64
	ThetaMin, ThetaMax, ThetaStep float64
	SuppressRhoMM, SuppressTheta  float64
}
type HoughLine struct {
	RhoMM, Theta float64
	Votes        uint32
}
type HoughLineResult struct {
	Candidates []HoughLine
	Report     HoughReport
}

// HoughLines2D votes immutable binary edges into a finite physical accumulator.
// Ties are deterministic (theta-major, then rho); no inferred anatomical label.
func HoughLines2D(ctx context.Context, edges *Volume, o HoughLineOptions) (*HoughLineResult, error) {
	n, area, err := houghDimensions(edges, o.Hough2DOptions)
	if err != nil {
		return nil, err
	}
	if !houghFinite(o.ThetaMin) || !houghFinite(o.ThetaMax) || o.ThetaMin < 0 || o.ThetaMax > math.Pi || !(o.SuppressRhoMM > 0) || !houghFinite(o.SuppressRhoMM) || !(o.SuppressTheta > 0) || !houghFinite(o.SuppressTheta) || o.SuppressTheta > math.Pi/2 {
		return nil, houghInvalid("line angle/suppression")
	}
	nr, err := houghGrid(o.RhoMinMM, o.RhoMaxMM, o.RhoStepMM, false)
	if err != nil {
		return nil, err
	}
	nt, err := houghGrid(o.ThetaMin, o.ThetaMax, o.ThetaStep, true)
	if err != nil {
		return nil, err
	}
	cells, err := houghMul(uint64(nr), uint64(nt))
	if err != nil || cells > 16<<20 {
		return nil, ErrHoughResourceLimit
	}
	votes, err := houghMul(area, uint64(nt))
	if err != nil {
		return nil, err
	}
	scans, err := houghMul(cells, uint64(1+2*o.MaxCandidates))
	if err != nil {
		return nil, err
	}
	ops, err := houghSum(n, votes, scans, uint64(nt))
	if err != nil {
		return nil, err
	}
	w, err := houghSetup(ctx, edges, o.Hough2DOptions, int(cells), 0, nt, ops)
	if err != nil {
		return nil, err
	}
	acc := make([]uint32, int(cells))
	trig := make([][2]float64, nt)
	for a := range trig {
		if err := w.tick(); err != nil {
			return nil, err
		}
		theta := o.ThetaMin + float64(a)*o.ThetaStep
		trig[a] = [2]float64{math.Cos(theta), math.Sin(theta)}
	}
	for y := o.Region.Min.Y; y < o.Region.Max.Y; y++ {
		for x := o.Region.Min.X; x < o.Region.Max.X; x++ {
			on := edges.At(x, y, 0) == 1
			if on {
				w.report.EdgePixels++
			}
			px, py := float64(x)*edges.SpacingMM[0], float64(y)*edges.SpacingMM[1]
			for a, t := range trig {
				if err := w.tick(); err != nil {
					return nil, err
				}
				if !on {
					continue
				}
				rho := px*t[0] + py*t[1]
				if rho < o.RhoMinMM || rho > o.RhoMaxMM {
					continue
				}
				r := int(math.Floor((rho-o.RhoMinMM)/o.RhoStepMM + .5))
				if r >= 0 && r < nr {
					acc[a*nr+r]++
				}
			}
		}
	}
	out := make([]HoughLine, 0, o.MaxCandidates)
	for len(out) < o.MaxCandidates {
		idx, err := houghPeak(w, acc, o.MinVotes)
		if err != nil {
			return nil, err
		}
		if idx < 0 {
			break
		}
		p := HoughLine{RhoMM: o.RhoMinMM + float64(idx%nr)*o.RhoStepMM, Theta: o.ThetaMin + float64(idx/nr)*o.ThetaStep, Votes: acc[idx]}
		out = append(out, p)
		for i := range acc {
			if err := w.tick(); err != nil {
				return nil, err
			}
			rho := o.RhoMinMM + float64(i%nr)*o.RhoStepMM
			theta := o.ThetaMin + float64(i/nr)*o.ThetaStep
			da := math.Abs(theta - p.Theta)
			dr := math.Abs(rho - p.RhoMM)
			if da > math.Pi/2 {
				da = math.Pi - da
				dr = math.Abs(rho + p.RhoMM)
			}
			if da <= o.SuppressTheta && dr <= o.SuppressRhoMM {
				acc[i] = 0
			}
		}
	}
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	return &HoughLineResult{Candidates: out, Report: w.report}, nil
}
