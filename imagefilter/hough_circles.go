package imagefilter

import (
	"context"
	"math"
)

// HoughCircleOptions uses aligned inclusive physical center/radius grids.
// Centers must be inside Region's pixel-center bounds; radii are positive mm.
// AngleBins samples the complete perimeter orientation, independently of edge
// polarity. One edge casts at most one vote into any center/radius cell.
type HoughCircleOptions struct {
	Hough2DOptions
	CenterMinMM, CenterMaxMM                             [2]float64
	CenterStepMM, RadiusMinMM, RadiusMaxMM, RadiusStepMM float64
	AngleBins                                            int
	SuppressCenterMM, SuppressRadiusMM                   float64
}
type HoughCircle struct {
	CenterMM [2]float64
	RadiusMM float64
	Votes    uint32
}
type HoughCircleResult struct {
	Candidates []HoughCircle
	Report     HoughReport
}

// HoughCircles2D returns bounded physical-circle proposals from immutable edges.
// A circle in mm can be an ellipse in indices when spacing is anisotropic.
// Ties are deterministic (radius-major, then y, then x). Visibility thresholds
// are caller-selected support counts; no probability or anatomy is inferred.
func HoughCircles2D(ctx context.Context, edges *Volume, o HoughCircleOptions) (*HoughCircleResult, error) {
	n, area, err := houghDimensions(edges, o.Hough2DOptions)
	if err != nil {
		return nil, err
	}
	if o.AngleBins < 8 || o.AngleBins > 4096 || !(o.RadiusMinMM > 0) || !(o.SuppressCenterMM > 0) || !houghFinite(o.SuppressCenterMM) || !(o.SuppressRadiusMM > 0) || !houghFinite(o.SuppressRadiusMM) {
		return nil, houghInvalid("circle angle/radius/suppression")
	}
	for a := 0; a < 2; a++ {
		lo := float64([]int{o.Region.Min.X, o.Region.Min.Y}[a]) * edges.SpacingMM[a]
		hi := float64([]int{o.Region.Max.X - 1, o.Region.Max.Y - 1}[a]) * edges.SpacingMM[a]
		if !houghFinite(lo) || !houghFinite(hi) || o.CenterMinMM[a] < lo || o.CenterMaxMM[a] > hi {
			return nil, houghInvalid("center grid outside physical region")
		}
	}
	nx, err := houghGrid(o.CenterMinMM[0], o.CenterMaxMM[0], o.CenterStepMM, false)
	if err != nil {
		return nil, err
	}
	ny, err := houghGrid(o.CenterMinMM[1], o.CenterMaxMM[1], o.CenterStepMM, false)
	if err != nil {
		return nil, err
	}
	nr, err := houghGrid(o.RadiusMinMM, o.RadiusMaxMM, o.RadiusStepMM, false)
	if err != nil {
		return nil, err
	}
	plane, err := houghMul(uint64(nx), uint64(ny))
	if err != nil {
		return nil, err
	}
	cells, err := houghMul(plane, uint64(nr))
	if err != nil || cells > 16<<20 {
		return nil, ErrHoughResourceLimit
	}
	voting, err := houghMul(area, uint64(o.AngleBins+1))
	if err != nil {
		return nil, err
	}
	voting, err = houghSum(voting, plane)
	if err != nil {
		return nil, err
	}
	voting, err = houghMul(voting, uint64(nr))
	if err != nil {
		return nil, err
	}
	scans, err := houghMul(cells, uint64(1+2*o.MaxCandidates))
	if err != nil {
		return nil, err
	}
	ops, err := houghSum(n, voting, scans, uint64(o.AngleBins))
	if err != nil {
		return nil, err
	}
	w, err := houghSetup(ctx, edges, o.Hough2DOptions, int(cells), int(plane), o.AngleBins, ops)
	if err != nil {
		return nil, err
	}
	acc := make([]uint32, int(cells))
	stamp := make([]uint32, int(plane))
	trig := make([][2]float64, o.AngleBins)
	for a := range trig {
		if err := w.tick(); err != nil {
			return nil, err
		}
		angle := 2 * math.Pi * float64(a) / float64(o.AngleBins)
		trig[a] = [2]float64{math.Cos(angle), math.Sin(angle)}
	}
	for r := 0; r < nr; r++ {
		for i := range stamp {
			if err := w.tick(); err != nil {
				return nil, err
			}
			stamp[i] = 0
		}
		radius := o.RadiusMinMM + float64(r)*o.RadiusStepMM
		for y := o.Region.Min.Y; y < o.Region.Max.Y; y++ {
			for x := o.Region.Min.X; x < o.Region.Max.X; x++ {
				if err := w.tick(); err != nil {
					return nil, err
				}
				if edges.At(x, y, 0) != 1 {
					continue
				}
				if r == 0 {
					w.report.EdgePixels++
				}
				id := uint32(edges.Index(x, y, 0) + 1)
				px, py := float64(x)*edges.SpacingMM[0], float64(y)*edges.SpacingMM[1]
				for _, t := range trig {
					if err := w.tick(); err != nil {
						return nil, err
					}
					cx, cy := px-radius*t[0], py-radius*t[1]
					if cx < o.CenterMinMM[0] || cx > o.CenterMaxMM[0] || cy < o.CenterMinMM[1] || cy > o.CenterMaxMM[1] {
						continue
					}
					ix := int(math.Floor((cx-o.CenterMinMM[0])/o.CenterStepMM + .5))
					iy := int(math.Floor((cy-o.CenterMinMM[1])/o.CenterStepMM + .5))
					if ix < 0 || ix >= nx || iy < 0 || iy >= ny {
						continue
					}
					i := iy*nx + ix
					if stamp[i] != id {
						stamp[i] = id
						acc[r*int(plane)+i]++
					}
				}
			}
		}
	}
	out := make([]HoughCircle, 0, o.MaxCandidates)
	for len(out) < o.MaxCandidates {
		idx, err := houghPeak(w, acc, o.MinVotes)
		if err != nil {
			return nil, err
		}
		if idx < 0 {
			break
		}
		p := HoughCircle{CenterMM: [2]float64{o.CenterMinMM[0] + float64(idx%nx)*o.CenterStepMM, o.CenterMinMM[1] + float64((idx/int(nx))%ny)*o.CenterStepMM}, RadiusMM: o.RadiusMinMM + float64(idx/int(plane))*o.RadiusStepMM, Votes: acc[idx]}
		out = append(out, p)
		for i := range acc {
			if err := w.tick(); err != nil {
				return nil, err
			}
			cx := o.CenterMinMM[0] + float64(i%nx)*o.CenterStepMM
			cy := o.CenterMinMM[1] + float64((i/nx)%ny)*o.CenterStepMM
			radius := o.RadiusMinMM + float64(i/int(plane))*o.RadiusStepMM
			if math.Hypot(cx-p.CenterMM[0], cy-p.CenterMM[1]) <= o.SuppressCenterMM && math.Abs(radius-p.RadiusMM) <= o.SuppressRadiusMM {
				acc[i] = 0
			}
		}
	}
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	return &HoughCircleResult{Candidates: out, Report: w.report}, nil
}
