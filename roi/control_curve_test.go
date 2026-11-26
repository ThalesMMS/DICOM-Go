package roi

import (
	"errors"
	"math"
	"slices"
	"testing"
)

func TestApproximateControlCurveAnisotropicMeasurements(t *testing.T) {
	spacing := MeasureSpacing{X: 0.37, Y: 1.23}
	for _, tc := range []struct {
		name     string
		controls []Point2D
		closed   bool
	}{
		{"open", []Point2D{{X: 5, Y: 12}, {X: 15.3, Y: 1.75}, {X: 31, Y: 24}, {X: 32, Y: 7}}, false},
		{"closed", []Point2D{{X: 2, Y: 3.25}, {X: 32, Y: 4}, {X: 24, Y: 25}, {X: 5.125, Y: 24}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contour, err := ApproximateControlCurve(tc.controls, tc.closed)
			if err != nil {
				t.Fatal(err)
			}
			if len(contour) <= len(tc.controls) || len(contour) > MaxCurveContourPoints || contour[0] != tc.controls[0] {
				t.Fatalf("contour point count or origin = %d, %v", len(contour), contour[0])
			}
			for _, control := range tc.controls {
				if !slices.Contains(contour, control) {
					t.Fatalf("contour omitted control %+v", control)
				}
			}
			oracle := denseControlCurve(tc.controls, tc.closed)
			lengthPoints := contour
			if tc.closed {
				if contour[0] == contour[len(contour)-1] {
					t.Fatal("closed contour duplicates first point")
				}
				lengthPoints = append(append([]Point2D(nil), contour...), contour[0])
				wantArea, _ := SubpixelPolygonAreaMM2(oracle, spacing)
				gotArea, _ := SubpixelPolygonAreaMM2(contour, spacing)
				if math.Abs(gotArea-wantArea) > 0.25 {
					t.Fatalf("curve area = %g mm², dense oracle %g mm²", gotArea, wantArea)
				}
			} else if contour[len(contour)-1] != tc.controls[len(tc.controls)-1] {
				t.Fatal("open contour changed last endpoint")
			}
			wantLength, _ := SubpixelPolygonLengthMM(oracle, spacing)
			gotLength, _ := SubpixelPolygonLengthMM(lengthPoints, spacing)
			if math.Abs(gotLength-wantLength) > 0.035 {
				t.Fatalf("curve length = %g mm, dense oracle %g mm", gotLength, wantLength)
			}
			maxDeviation := 0.0
			for _, p := range oracle {
				best := math.Inf(1)
				for i := 1; i < len(lengthPoints); i++ {
					a, b := lengthPoints[i-1], lengthPoints[i]
					dx, dy := b.X-a.X, b.Y-a.Y
					ratio := math.Max(0, math.Min(1, ((p.X-a.X)*dx+(p.Y-a.Y)*dy)/(dx*dx+dy*dy)))
					best = math.Min(best, math.Hypot(p.X-a.X-ratio*dx, p.Y-a.Y-ratio*dy))
				}
				maxDeviation = math.Max(maxDeviation, best)
			}
			if maxDeviation > CurveTolerancePx {
				t.Fatalf("curve deviates %.7g px, bound %g px", maxDeviation, CurveTolerancePx)
			}
		})
	}
}

func TestApproximateControlCurveRefusesDegenerateOrUnboundedGeometry(t *testing.T) {
	line := []Point2D{{X: 1, Y: 3}, {X: 5, Y: 8}}
	got, err := ApproximateControlCurve(line, false)
	if err != nil || !slices.Equal(got, line) {
		t.Fatalf("two-point open curve = %v, %v", got, err)
	}
	crossing := []Point2D{{X: 0, Y: 0}, {X: 12, Y: 12}, {X: 0, Y: 12}, {X: 12, Y: 0}}
	if got, err := ApproximateControlCurve(crossing, false); len(got) <= len(crossing) || err != nil {
		t.Fatalf("open crossing was refused: %v", err)
	}
	for _, tc := range []struct {
		name     string
		controls []Point2D
		closed   bool
		want     error
	}{
		{"zero length", []Point2D{{}, {}}, false, ErrCurveGeometry},
		{"nonfinite", []Point2D{{}, {X: math.NaN()}}, false, ErrCurveGeometry},
		{"closed requires three", line, true, ErrCurveGeometry},
		{"collinear closed", []Point2D{{}, {X: 7}, {X: 12}}, true, ErrCurveGeometry},
		{"bow tie", crossing, true, ErrCurveGeometry},
		{"control limit", make([]Point2D, MaxCurveControlPoints+1), false, ErrCurveLimit},
		{"sample limit", []Point2D{{}, {X: 1e6}, {X: 1e6, Y: 1e6}, {Y: 1e6}}, true, ErrCurveLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApproximateControlCurve(tc.controls, tc.closed)
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("ApproximateControlCurve returned %d points, %v, want %v", len(got), err, tc.want)
			}
		})
	}
}

// denseControlCurve evaluates the nonuniform interpolation directly with
// Barry–Goldman knot interpolation, without the production tangent or
// subdivision routines.
func denseControlCurve(controls []Point2D, closed bool) []Point2D {
	n := len(controls)
	spans := n - 1
	if closed {
		spans++
	}
	out := make([]Point2D, 0, spans*2048+1)
	at := func(a, b Point2D, ta, tb, t float64) Point2D {
		f := (t - ta) / (tb - ta)
		return Point2D{X: a.X + (b.X-a.X)*f, Y: a.Y + (b.Y-a.Y)*f}
	}
	nextKnot := func(k float64, a, b Point2D) float64 {
		return k + math.Sqrt(math.Hypot(b.X-a.X, b.Y-a.Y))
	}
	for i := range spans {
		b, c := controls[i], controls[(i+1)%n]
		a, d := at(c, b, 0, 1, 2), at(b, c, 0, 1, 2)
		if i > 0 || closed {
			a = controls[(i+n-1)%n]
		}
		if i+2 < n || closed {
			d = controls[(i+2)%n]
		}
		t0 := 0.0
		t1 := nextKnot(t0, a, b)
		t2 := nextKnot(t1, b, c)
		t3 := nextKnot(t2, c, d)
		for j := range 2048 {
			t := t1 + (t2-t1)*float64(j)/2048
			a1, a2, a3 := at(a, b, t0, t1, t), at(b, c, t1, t2, t), at(c, d, t2, t3, t)
			b1, b2 := at(a1, a2, t0, t2, t), at(a2, a3, t1, t3, t)
			out = append(out, at(b1, b2, t1, t2, t))
		}
	}
	if closed {
		return append(out, controls[0])
	}
	return append(out, controls[n-1])
}
