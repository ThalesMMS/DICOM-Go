package imagefilter

import (
	"context"
	"fmt"
	"image"
	"math"
	"reflect"
	"testing"
)

func houghTestLineOptions(v *Volume) HoughLineOptions {
	return HoughLineOptions{Hough2DOptions: Hough2DOptions{Region: image.Rect(0, 0, v.Dims[0], v.Dims[1]), MinVotes: 16, MaxCandidates: 4}, RhoMinMM: -128, RhoMaxMM: 128, RhoStepMM: .5, ThetaMin: 0, ThetaMax: math.Pi, ThetaStep: math.Pi / 180, SuppressRhoMM: 2, SuppressTheta: 3 * math.Pi / 180}
}
func houghTestCircleOptions(v *Volume, c [2]float64, r float64) HoughCircleOptions {
	return HoughCircleOptions{Hough2DOptions: Hough2DOptions{Region: image.Rect(0, 0, v.Dims[0], v.Dims[1]), MinVotes: 8, MaxCandidates: 4}, CenterMinMM: [2]float64{c[0] - 2, c[1] - 2}, CenterMaxMM: [2]float64{c[0] + 2, c[1] + 2}, CenterStepMM: .5, RadiusMinMM: r - 1, RadiusMaxMM: r + 1, RadiusStepMM: .5, AngleBins: 180, SuppressCenterMM: 2*math.Max(v.SpacingMM[0], v.SpacingMM[1]) + .5, SuppressRadiusMM: 1}
}

// Phantom creation uses analytic distances, never the production accumulator.
func houghCirclePhantom(n int, s [2]float64, r, arc float64) (*Volume, [2]float64, [][2]float64) {
	v := NewVolume([3]int{n, n, 1}, [3]float64{s[0], s[1], 0})
	c := [2]float64{float64(n/2) * s[0], float64(n/2) * s[1]}
	points := [][2]float64{}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			px, py := float64(x)*s[0], float64(y)*s[1]
			angle := math.Atan2(py-c[1], px-c[0])
			if angle < 0 {
				angle += 2 * math.Pi
			}
			if angle <= arc && math.Abs(math.Hypot(px-c[0], py-c[1])-r) <= .35*math.Hypot(s[0], s[1]) {
				v.Data[v.Index(x, y, 0)] = 1
				points = append(points, [2]float64{px, py})
			}
		}
	}
	return v, c, points
}
func houghAddNoise(v *Volume, count int) {
	state := uint32(1547)
	for i := 0; i < count; i++ {
		state = 1664525*state + 1013904223
		v.Data[int(state)%len(v.Data)] = 1
	}
}

// Independent orthogonal least-squares line oracle from the covariance tensor.
func houghFitLine(p [][2]float64) (rho, theta float64) {
	var mx, my float64
	for _, q := range p {
		mx += q[0]
		my += q[1]
	}
	mx /= float64(len(p))
	my /= float64(len(p))
	var xx, xy, yy float64
	for _, q := range p {
		x, y := q[0]-mx, q[1]-my
		xx += x * x
		xy += x * y
		yy += y * y
	}
	theta = .5*math.Atan2(2*xy, xx-yy) + math.Pi/2
	for theta >= math.Pi {
		theta -= math.Pi
	}
	rho = mx*math.Cos(theta) + my*math.Sin(theta)
	return
}

// Independent algebraic circle oracle solves x²+y²=A*x+B*y+C by pivoted3x3
// normal equations. No radius/angle search, voting or peak suppression is used.
func houghFitCircle(p [][2]float64) ([2]float64, float64) {
	var a [3][4]float64
	for _, q := range p {
		z := [3]float64{q[0], q[1], 1}
		b := q[0]*q[0] + q[1]*q[1]
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				a[i][j] += z[i] * z[j]
			}
			a[i][3] += z[i] * b
		}
	}
	for col := 0; col < 3; col++ {
		pivot := col
		for row := col + 1; row < 3; row++ {
			if math.Abs(a[row][col]) > math.Abs(a[pivot][col]) {
				pivot = row
			}
		}
		a[col], a[pivot] = a[pivot], a[col]
		d := a[col][col]
		for j := col; j < 4; j++ {
			a[col][j] /= d
		}
		for row := 0; row < 3; row++ {
			if row == col {
				continue
			}
			d = a[row][col]
			for j := col; j < 4; j++ {
				a[row][j] -= d * a[col][j]
			}
		}
	}
	c := [2]float64{a[0][3] / 2, a[1][3] / 2}
	return c, math.Sqrt(a[2][3] + c[0]*c[0] + c[1]*c[1])
}
func houghLineDistance(p HoughLine, rho, theta float64) (float64, float64) {
	a := math.Abs(p.Theta - theta)
	d := math.Abs(p.RhoMM - rho)
	if a > math.Pi/2 {
		a = math.Pi - a
		d = math.Abs(p.RhoMM + rho)
	}
	return d, a
}

func TestHough2DAnalyticLinesScalesAndIntersections(t *testing.T) {
	for _, n := range []int{64, 96, 128} {
		for _, s := range [][2]float64{{.5, .5}, {.4, .8}, {1, 1}} {
			for _, theta := range []float64{0, math.Pi / 4, math.Pi / 2} {
				t.Run(fmt.Sprintf("%d-%g-%g-%g", n, s[0], s[1], theta), func(t *testing.T) {
					v := NewVolume([3]int{n, n, 1}, [3]float64{s[0], s[1], 0})
					rho := float64(n/2) * (s[0]*math.Cos(theta) + s[1]*math.Sin(theta))
					points := [][2]float64{}
					for y := 0; y < n; y++ {
						for x := 0; x < n; x++ {
							px, py := float64(x)*s[0], float64(y)*s[1]
							if math.Abs(px*math.Cos(theta)+py*math.Sin(theta)-rho) <= .35*math.Hypot(s[0], s[1]) {
								v.Data[v.Index(x, y, 0)] = 1
								points = append(points, [2]float64{px, py})
							}
						}
					}
					rr, tt := houghFitLine(points)
					before := append([]float32(nil), v.Data...)
					houghAddNoise(v, len(v.Data)/200)
					noisy := append([]float32(nil), v.Data...)
					o := houghTestLineOptions(v)
					o.MinVotes = uint32(len(points) / 2)
					r, e := HoughLines2D(context.Background(), v, o)
					if e != nil {
						t.Fatal(e)
					}
					hits, fp := 0, 0
					analyticHits := 0
					for _, p := range r.Candidates {
						idealRhoError, idealAngleError := houghLineDistance(p, rho, theta)
						if idealRhoError <= math.Max(s[0], s[1])+.5 && idealAngleError <= 2*math.Pi/180 {
							analyticHits++
							t.Logf("analyticLineMatch rhoErrorMM=%g normalErrorRadians=%g candidate=%+v ideal=%g/%g", idealRhoError, idealAngleError, p, rho, theta)
						}
						d, a := houghLineDistance(p, rr, tt)
						if d <= math.Max(s[0], s[1])+.5 && a <= 2*math.Pi/180 {
							hits++
						} else {
							fp++
						}
					}
					if hits < 1 || fp > 1 {
						t.Fatalf("sensitivity/FP outside prerecipe: candidateHits=%d FP=%d candidates=%+v", hits, fp, r.Candidates)
					}
					if analyticHits < 1 {
						t.Fatalf("no candidate jointly satisfies original analytic geometry tolerances: candidates=%+v", r.Candidates)
					}
					if !reflect.DeepEqual(v.Data, noisy) || reflect.DeepEqual(before, noisy) {
						t.Fatal("ownership or seeded-noise fixture")
					}
					t.Logf("candidateHits=%d analyticHits=%d FP=%d work=%+v", hits, analyticHits, fp, r.Report)
				})
			}
		}
	}
	v := NewVolume([3]int{64, 64, 1}, [3]float64{.5, .5, 0})
	for y := 0; y < 64; y++ {
		v.Data[v.Index(20, y, 0)] = 1
		v.Data[v.Index(y, y, 0)] = 1
	}
	o := houghTestLineOptions(v)
	o.MinVotes = 40
	r, e := HoughLines2D(nil, v, o)
	if e != nil {
		t.Fatal(e)
	}
	found := [2]bool{}
	fp := 0
	for _, p := range r.Candidates {
		d0, a0 := houghLineDistance(p, 10, 0)
		d1, a1 := houghLineDistance(p, 0, 3*math.Pi/4)
		if d0 <= 1 && a0 <= 2*math.Pi/180 {
			found[0] = true
		} else if d1 <= 1 && a1 <= 2*math.Pi/180 {
			found[1] = true
		} else {
			fp++
		}
	}
	if !found[0] || !found[1] || fp > 1 {
		t.Fatalf("intersecting lines %+v FP=%d", r.Candidates, fp)
	}
}
func TestHough2DPhysicalCirclesAndPartialArcs(t *testing.T) {
	for i, n := range []int{64, 96, 128} {
		radius := []float64{6, 10, 14}[i]
		for _, s := range [][2]float64{{.5, .5}, {.4, .8}, {1, 1}} {
			for _, arc := range []float64{2 * math.Pi, 210 * math.Pi / 180} {
				t.Run(fmt.Sprintf("%d-%g-%g-%g", n, s[0], s[1], arc), func(t *testing.T) {
					v, c, points := houghCirclePhantom(n, s, radius, arc)
					oracleC, oracleR := houghFitCircle(points)
					o := houghTestCircleOptions(v, c, radius)
					o.MinVotes = uint32(len(points) / 3)
					if o.MinVotes < 8 {
						o.MinVotes = 8
					}
					houghAddNoise(v, len(v.Data)/200)
					before := append([]float32(nil), v.Data...)
					r, e := HoughCircles2D(nil, v, o)
					if e != nil {
						t.Fatal(e)
					}
					hits, fp := 0, 0
					analyticHits := 0
					for _, p := range r.Candidates {
						idealCenterError := math.Hypot(p.CenterMM[0]-c[0], p.CenterMM[1]-c[1])
						idealRadiusError := math.Abs(p.RadiusMM - radius)
						if idealCenterError <= 1.5*math.Max(s[0], s[1])+.5 && idealRadiusError <= math.Max(s[0], s[1])+.5 {
							analyticHits++
							t.Logf("analyticCircleMatch centerErrorMM=%g radiusErrorMM=%g candidate=%+v ideal=%v/%g", idealCenterError, idealRadiusError, p, c, radius)
						}
						if math.Hypot(p.CenterMM[0]-oracleC[0], p.CenterMM[1]-oracleC[1]) <= 1.5*math.Max(s[0], s[1])+.5 && math.Abs(p.RadiusMM-oracleR) <= math.Max(s[0], s[1])+.5 {
							hits++
						} else {
							fp++
						}
					}
					if hits < 1 || fp > 1 {
						t.Fatalf("physical circle sensitivity/FP candidateHits=%d FP=%d candidates=%+v oracle=%v/%g", hits, fp, r.Candidates, oracleC, oracleR)
					}
					if analyticHits < 1 {
						t.Fatalf("no candidate jointly satisfies original analytic geometry tolerances: candidates=%+v", r.Candidates)
					}
					if !reflect.DeepEqual(v.Data, before) {
						t.Fatal("mutated source")
					}
					for _, p := range r.Candidates {
						if p.Votes > uint32(r.Report.EdgePixels) {
							t.Fatal("duplicate edge votes")
						}
					}
					t.Logf("candidateHits=%d analyticHits=%d FP=%d oracle=%v/%g physicaltruth=%v/%g work=%+v", hits, analyticHits, fp, oracleC, oracleR, c, radius, r.Report)
				})
			}
		}
	}
	// Low-visibility90°arcs are a preregistered challenge, not qualified sensitivity.
	v, c, _ := houghCirclePhantom(96, [2]float64{.5, .5}, 10, math.Pi/2)
	r, e := HoughCircles2D(nil, v, houghTestCircleOptions(v, c, 10))
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("unqualified90degree arc candidates=%+v", r.Candidates)
}
func TestHough2DCannyInvertedContrastAndEmpty(t *testing.T) {
	a := NewVolume([3]int{64, 64, 1}, [3]float64{.5, .5, 0})
	b := a.Clone()
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if x >= 32 {
				a.Data[a.Index(x, y, 0)] = 1
			}
			b.Data[b.Index(x, y, 0)] = -a.Data[a.Index(x, y, 0)]
		}
	}
	options := CannyOptions{CannyParameters: CannyParameters{SigmaMM: [3]float64{.5, .5, 0}, LowerThreshold: .2, UpperThreshold: .4}, Workers: 1}
	aa, e := Canny(nil, a, options)
	if e != nil {
		t.Fatal(e)
	}
	bb, e := Canny(nil, b, options)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(aa.Edges.Data, bb.Edges.Data) {
		t.Fatal("inverted Canny geometry differs")
	}
	r, e := HoughLines2D(nil, aa.Edges, houghTestLineOptions(aa.Edges))
	if e != nil {
		t.Fatal(e)
	}
	q, e := HoughLines2D(nil, bb.Edges, houghTestLineOptions(bb.Edges))
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Candidates) == 0 || !reflect.DeepEqual(r, q) {
		t.Fatal("contrast changed candidates")
	}
	blank := a.Clone()
	for i := range blank.Data {
		blank.Data[i] = 0
	}
	l, e := HoughLines2D(nil, blank, houghTestLineOptions(blank))
	if e != nil || len(l.Candidates) != 0 {
		t.Fatalf("empty lines %v", e)
	}
	cc, e := HoughCircles2D(nil, blank, houghTestCircleOptions(blank, [2]float64{16, 16}, 6))
	if e != nil || len(cc.Candidates) != 0 {
		t.Fatalf("empty circles %v", e)
	}
}
