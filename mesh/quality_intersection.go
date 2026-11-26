package mesh

import (
	"math"
	"math/big"
)

func qualityNormal(t [3][3]float64) [3]float64 {
	n := cross3(sub3(t[1], t[0]), sub3(t[2], t[0]))
	return scale3(n, 1/math.Sqrt(dot3(n, n)))
}
func qualityDistances(t [3][3]float64, p, n [3]float64) (d [3]float64) {
	for i := range t {
		d[i] = dot3(sub3(t[i], p), n)
	}
	return
}
func qualitySeparated(d [3]float64, tol float64) bool {
	return (d[0] > tol && d[1] > tol && d[2] > tol) || (d[0] < -tol && d[1] < -tol && d[2] < -tol)
}

// The roundoff band is coordinate-scale dependent. It is deliberately
// conservative, not an exact-predicate or arbitrary-precision guarantee.
func qualityRoundoff(a, b [3][3]float64) float64 {
	s := 0.0
	for _, t := range [2][3][3]float64{a, b} {
		for _, p := range t {
			for _, x := range p {
				s = math.Max(s, math.Abs(x))
			}
		}
	}
	return 64 * (math.Nextafter(1, 2) - 1) * s
}
func qualityIntersect(a, b [3][3]float64, tol float64) QualityPairKind {
	na, nb := qualityNormal(a), qualityNormal(b)
	da, db := qualityDistances(a, b[0], nb), qualityDistances(b, a[0], na)
	eps := qualityRoundoff(a, b)
	band := math.Max(tol, eps)
	if qualitySeparated(da, band) || qualitySeparated(db, band) {
		return ""
	}
	axis := cross3(na, nb)
	length := math.Sqrt(dot3(axis, axis))
	if length <= 64*(math.Nextafter(1, 2)-1) {
		maxD := 0.0
		for _, d := range da {
			maxD = math.Max(maxD, math.Abs(d))
		}
		for _, d := range db {
			maxD = math.Max(maxD, math.Abs(d))
		}
		if maxD <= eps && qualityExactlyCoplanar(a, b) {
			return qualityCoplanar(a, b, na, band)
		}
		if maxD <= band {
			return QualityUncertain
		}
		return ""
	}
	axis = scale3(axis, 1/length)
	alo, ahi, ac := qualityPlaneInterval(a, da, axis, eps)
	blo, bhi, bc := qualityPlaneInterval(b, db, axis, eps)
	if ac == 0 || bc == 0 {
		return QualityUncertain
	}
	lo, hi := math.Max(alo, blo), math.Min(ahi, bhi)
	if hi < lo-band {
		return ""
	}
	if hi-lo <= band {
		return QualityContact
	}
	// A positive-length intersection lying entirely on a triangle edge is a
	// contact. Interior cuts of both triangles establish a crossing.
	if qualityStraddles(da, eps) && qualityStraddles(db, eps) {
		return QualityCrossing
	}
	return QualityContact
}
func qualityStraddles(d [3]float64, e float64) bool {
	lo, hi := d[0], d[0]
	for _, x := range d {
		lo = math.Min(lo, x)
		hi = math.Max(hi, x)
	}
	return lo < -e && hi > e
}
func qualityPlaneInterval(t [3][3]float64, d [3]float64, axis [3]float64, eps float64) (lo, hi float64, count int) {
	lo, hi = math.Inf(1), math.Inf(-1)
	add := func(p [3]float64) { x := dot3(p, axis); lo = math.Min(lo, x); hi = math.Max(hi, x); count++ }
	for i := 0; i < 3; i++ {
		j := (i + 1) % 3
		if math.Abs(d[i]) <= eps {
			add(t[i])
		}
		if (d[i] < -eps && d[j] > eps) || (d[i] > eps && d[j] < -eps) {
			f := d[i] / (d[i] - d[j])
			add(add3(t[i], scale3(sub3(t[j], t[i]), f)))
		}
	}
	return
}

type qualityP2 [2]float64

func qualityCoplanar(a, b [3][3]float64, n [3]float64, band float64) QualityPairKind {
	drop := 0
	for i := 1; i < 3; i++ {
		if math.Abs(n[i]) > math.Abs(n[drop]) {
			drop = i
		}
	}
	u, v := (drop+1)%3, (drop+2)%3
	var aa, bb [3]qualityP2
	for i := 0; i < 3; i++ {
		aa[i] = qualityP2{a[i][u], a[i][v]}
		bb[i] = qualityP2{b[i][u], b[i][v]}
	}
	// Separating axes distinguish disjoint faces and boundary-only contact.
	// All axes are normalized, so projection tolerances remain millimetres.
	contact := false
	for _, t := range [2][3]qualityP2{aa, bb} {
		for i := 0; i < 3; i++ {
			j := (i + 1) % 3
			dx, dy := t[j][0]-t[i][0], t[j][1]-t[i][1]
			l := math.Hypot(dx, dy)
			if l == 0 {
				continue
			}
			x, y := -dy/l, dx/l
			amin, amax, bmin, bmax := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
			for k := 0; k < 3; k++ {
				ap := aa[k][0]*x + aa[k][1]*y
				bp := bb[k][0]*x + bb[k][1]*y
				amin = math.Min(amin, ap)
				amax = math.Max(amax, ap)
				bmin = math.Min(bmin, bp)
				bmax = math.Max(bmax, bp)
			}
			overlap := math.Min(amax, bmax) - math.Max(amin, bmin)
			if overlap < -band {
				return ""
			}
			if overlap <= band {
				contact = true
			}
		}
	}
	if contact {
		return QualityContact
	}
	return QualityCoplanarOverlap
}

// float32 coordinates are exact dyadics. Scaling by 2^149 gives signed
// integers of at most 277 bits. A 3x3 determinant then needs at most 840
// bits: fixed-size mathematics, no precision chosen by input size.
func qualityDyadic(x float64, out *big.Int) {
	bits := math.Float32bits(float32(x))
	exp := (bits >> 23) & 255
	mant := bits & 0x7fffff
	if exp != 0 {
		mant |= 1 << 23
	}
	out.SetUint64(uint64(mant))
	if exp > 0 {
		out.Lsh(out, uint(exp-1))
	}
	if bits>>31 != 0 {
		out.Neg(out)
	}
}
func qualityOrientationExact(t [3][3]float64, p [3]float64) int {
	var d [3][3]big.Int
	var a, b, x, y big.Int
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			v := p[j]
			if i < 2 {
				v = t[i+1][j]
			}
			qualityDyadic(v, &a)
			qualityDyadic(t[0][j], &b)
			d[i][j].Sub(&a, &b)
		}
	}
	var cross [3]big.Int
	for i := 0; i < 3; i++ {
		j, k := (i+1)%3, (i+2)%3
		x.Mul(&d[0][j], &d[1][k])
		y.Mul(&d[0][k], &d[1][j])
		cross[i].Sub(&x, &y)
	}
	var det big.Int
	for i := 0; i < 3; i++ {
		x.Mul(&cross[i], &d[2][i])
		det.Add(&det, &x)
	}
	return det.Sign()
}
func qualityExactlyCoplanar(a, b [3][3]float64) bool {
	// Common axis-aligned slices need no multiprecision allocation.
	for axis := 0; axis < 3; axis++ {
		same := true
		for i := 0; i < 3; i++ {
			same = same && a[i][axis] == a[0][axis] && b[i][axis] == a[0][axis]
		}
		if same {
			return true
		}
	}
	for _, p := range b {
		if qualityOrientationExact(a, p) != 0 {
			return false
		}
	}
	return true
}
