package roi_test

import (
	"context"
	"math"
	"math/big"
	"testing"

	"github.com/ThalesMMS/dicom-go/roi"
)

// independentDistance uses a cross-product line distance plus endpoint dot
// tests. It does not call the implementation's projection or exact helpers.
func independentDistance(p, a, b roi.Point3D) float64 {
	ux, uy, uz := b.X-a.X, b.Y-a.Y, b.Z-a.Z
	vx, vy, vz := p.X-a.X, p.Y-a.Y, p.Z-a.Z
	dot := ux*vx + uy*vy + uz*vz
	if dot <= 0 {
		return math.Sqrt(vx*vx + vy*vy + vz*vz)
	}
	length2 := ux*ux + uy*uy + uz*uz
	if dot >= length2 {
		x, y, z := p.X-b.X, p.Y-b.Y, p.Z-b.Z
		return math.Sqrt(x*x + y*y + z*z)
	}
	x, y, z := uy*vz-uz*vy, uz*vx-ux*vz, ux*vy-uy*vx
	return math.Sqrt((x*x + y*y + z*z) / length2)
}

// independentExactWithin compares squared cross-product distance rather than
// the implementation's v²*length²-dot² expression. No rounded distance or epsilon
// authorizes the chord. All Rat inputs are the original float64 numbers.
func independentExactWithin(p, a, b roi.Point3D, tolerance float64) bool {
	scalar := func(x float64) *big.Rat { return new(big.Rat).SetFloat64(x) }
	sub := func(x, y *big.Rat) *big.Rat { return new(big.Rat).Sub(x, y) }
	mul := func(x, y *big.Rat) *big.Rat { return new(big.Rat).Mul(x, y) }
	add := func(x, y *big.Rat) *big.Rat { return new(big.Rat).Add(x, y) }
	pv, av, bv := [3]float64{p.X, p.Y, p.Z}, [3]float64{a.X, a.Y, a.Z}, [3]float64{b.X, b.Y, b.Z}
	var v, d, w [3]*big.Rat
	for i := range v {
		v[i] = sub(scalar(pv[i]), scalar(av[i]))
		d[i] = sub(scalar(bv[i]), scalar(av[i]))
		w[i] = sub(scalar(pv[i]), scalar(bv[i]))
	}
	dot, norm2, distA, distB := new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat)
	for i := range v {
		dot = add(dot, mul(v[i], d[i]))
		norm2 = add(norm2, mul(d[i], d[i]))
		distA = add(distA, mul(v[i], v[i]))
		distB = add(distB, mul(w[i], w[i]))
	}
	tol2 := mul(scalar(tolerance), scalar(tolerance))
	if dot.Sign() <= 0 || norm2.Sign() == 0 {
		return distA.Cmp(tol2) <= 0
	}
	if dot.Cmp(norm2) >= 0 {
		return distB.Cmp(tol2) <= 0
	}
	cross2 := new(big.Rat)
	for i := range v {
		j, k := (i+1)%3, (i+2)%3
		component := sub(mul(v[j], d[k]), mul(v[k], d[j]))
		cross2 = add(cross2, mul(component, component))
	}
	return cross2.Cmp(mul(tol2, norm2)) <= 0
}
func independentLength(p []roi.Point3D, closed bool) float64 {
	sum := 0.0
	for i := 1; i < len(p); i++ {
		a, b := p[i-1], p[i]
		x, y, z := b.X-a.X, b.Y-a.Y, b.Z-a.Z
		sum += math.Sqrt(x*x + y*y + z*z)
	}
	if closed && len(p) > 1 {
		a, b := p[len(p)-1], p[0]
		x, y, z := b.X-a.X, b.Y-a.Y, b.Z-a.Z
		sum += math.Sqrt(x*x + y*y + z*z)
	}
	return sum
}
func qualifyPhysical(t *testing.T, p []roi.Point3D, o roi.PolylineSimplifyOptions) roi.PolylineSimplifyResult {
	t.Helper()
	got, err := roi.SimplifyPhysicalPolyline(context.Background(), p, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != len(got.OriginalIndices) {
		t.Fatal("correspondence length")
	}
	for j, i := range got.OriginalIndices {
		if i < 0 || i >= len(p) || got.Points[j] != p[i] || (j > 0 && i <= got.OriginalIndices[j-1]) {
			t.Fatal("correspondence/order")
		}
	}
	for _, protected := range o.ProtectedIndices {
		found := false
		for _, i := range got.OriginalIndices {
			if i == protected {
				found = true
			}
		}
		if !found {
			t.Fatalf("protected index %d removed", protected)
		}
	}
	worst := 0.0
	check := func(start, end int, b roi.Point3D) {
		for i := start; i <= end; i++ {
			if !independentExactWithin(p[i], p[start], b, o.ToleranceMM) {
				t.Fatalf("independent exact rational tolerance violation at original index %d", i)
			}
			d := independentDistance(p[i], p[start], b)
			worst = math.Max(worst, d)
			if d > o.ToleranceMM+1e-11 {
				t.Fatalf("independent deviation %g > %g", d, o.ToleranceMM)
			}
		}
	}
	for j := 1; j < len(got.OriginalIndices); j++ {
		check(got.OriginalIndices[j-1], got.OriginalIndices[j], got.Points[j])
	}
	if o.Closed && len(got.OriginalIndices) > 0 {
		check(got.OriginalIndices[len(got.OriginalIndices)-1], len(p)-1, p[0])
	}
	if math.Abs(worst-got.MaxDeviationMM) > 1e-10 {
		t.Fatalf("deviation diagnostic %g vs %g", got.MaxDeviationMM, worst)
	}
	if math.Abs(independentLength(p, o.Closed)-got.InputLengthMM) > 1e-10 || math.Abs(independentLength(got.Points, o.Closed)-got.OutputLengthMM) > 1e-10 {
		t.Fatal("independent length mismatch")
	}
	if got.OutputLengthMM > got.InputLengthMM+1e-10 {
		t.Fatal("chord output longer than original")
	}
	return got
}

func TestPhysicalPolylineAnalyticIndependent(t *testing.T) {
	for _, closed := range []bool{false, true} {
		for _, count := range []int{33, 65, 129} {
			for _, tol := range []float64{0, .01, .1, .5} {
				name := "open"
				if closed {
					name = "closed"
				}
				t.Run(name+"/"+fmtPolylineInt(count)+"/"+fmtPolylineTolerance(tol), func(t *testing.T) {
					p := make([]roi.Point3D, count)
					for i := range p {
						angle := 2 * math.Pi * float64(i) / float64(count)
						p[i] = roi.Point3D{X: 10 * math.Cos(angle), Y: 3 * math.Sin(angle)}
						if !closed {
							p[i].Z = float64(i) / 10
						}
					}
					got := qualifyPhysical(t, p, roi.PolylineSimplifyOptions{Closed: closed, ToleranceMM: tol, ProtectedIndices: []int{count / 4, count / 2}})
					if closed && len(got.Points) < 3 {
						t.Fatal("loop collapsed")
					}
					if !closed && (got.OriginalIndices[0] != 0 || got.OriginalIndices[len(got.Points)-1] != count-1) {
						t.Fatal("endpoints lost")
					}
				})
			}
		}
	}
}
func fmtPolylineInt(n int) string {
	if n == 33 {
		return "33"
	}
	if n == 65 {
		return "65"
	}
	return "129"
}
func fmtPolylineTolerance(t float64) string {
	if t == 0 {
		return "0"
	}
	if t == .01 {
		return "0.01"
	}
	if t == .1 {
		return "0.1"
	}
	return "0.5"
}

func TestPhysicalPolylineAnisotropicMetricIndependent(t *testing.T) {
	pixels := []roi.Point2D{{}, {X: 1, Y: .2}, {X: 2}, {X: 3, Y: -.2}, {X: 4}}
	for _, spacing := range []float64{.5, 5} {
		plane := roi.PolylinePixelPlane{ColumnStepMM: roi.Point3D{X: 1}, RowStepMM: roi.Point3D{Y: spacing}}
		got, err := roi.SimplifyPixelPolyline(context.Background(), pixels, plane, roi.PolylineSimplifyOptions{ToleranceMM: .5})
		if err != nil {
			t.Fatal(err)
		}
		physical := make([]roi.Point3D, len(pixels))
		for i, p := range pixels {
			physical[i] = roi.Point3D{X: p.X, Y: p.Y * spacing}
		}
		reference := qualifyPhysical(t, physical, roi.PolylineSimplifyOptions{ToleranceMM: .5})
		if len(got.Points) != len(reference.Points) {
			t.Fatal("adapter metric mismatch")
		}
		if spacing == .5 && len(got.Points) != 2 {
			t.Fatal("low anisotropy reduction")
		}
		if spacing == 5 && len(got.Points) <= 2 {
			t.Fatal("high anisotropy reduction")
		}
	}
}

func TestPhysicalPolylineIndependentExactBoundary(t *testing.T) {
	for _, height := range []float64{math.SmallestNonzeroFloat64, 1e-20, 1, 1e8} {
		p := []roi.Point3D{{}, {X: .5, Y: height}, {X: 1}}
		for _, tol := range []float64{height, math.Nextafter(height, 0)} {
			got := qualifyPhysical(t, p, roi.PolylineSimplifyOptions{ToleranceMM: tol})
			expected := 2
			if tol < height {
				expected = 3
			}
			if len(got.Points) != expected {
				t.Fatalf("boundary %g tolerance %g retained %d want %d", height, tol, len(got.Points), expected)
			}
		}
	}
}
