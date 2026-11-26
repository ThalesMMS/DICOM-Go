package roi

import "math/big"

// These expressions are independently derived projection and determinant
// algebra. Rat.SetFloat64 represents finite input coordinates exactly, avoiding
// an epsilon that could silently accept a crossing or a too-distant sample.
type polylineRatVec [3]big.Rat

func polylineRatPoint(p Point3D) polylineRatVec {
	var v polylineRatVec
	v[0].SetFloat64(p.X)
	v[1].SetFloat64(p.Y)
	v[2].SetFloat64(p.Z)
	return v
}
func polylineRatSub(a, b *polylineRatVec) polylineRatVec {
	var v polylineRatVec
	for i := range v {
		v[i].Sub(&a[i], &b[i])
	}
	return v
}
func polylineRatDot(a, b *polylineRatVec) big.Rat {
	var value, term big.Rat
	for i := range a {
		term.Mul(&a[i], &b[i])
		value.Add(&value, &term)
	}
	return value
}
func polylineRatCross(a, b *polylineRatVec) polylineRatVec {
	var c polylineRatVec
	var x, y big.Rat
	for i := range c {
		j, k := (i+1)%3, (i+2)%3
		x.Mul(&a[j], &b[k])
		y.Mul(&a[k], &b[j])
		c[i].Sub(&x, &y)
	}
	return c
}
func polylineIndependent(a, b Point3D) bool {
	av, bv := polylineRatPoint(a), polylineRatPoint(b)
	c := polylineRatCross(&av, &bv)
	return c[0].Sign() != 0 || c[1].Sign() != 0 || c[2].Sign() != 0
}

func polylineWithin(p, a, b Point3D, tolerance float64) bool {
	pv, av, bv := polylineRatPoint(p), polylineRatPoint(a), polylineRatPoint(b)
	d, v := polylineRatSub(&bv, &av), polylineRatSub(&pv, &av)
	l2, dot := polylineRatDot(&d, &d), polylineRatDot(&v, &d)
	var t, t2 big.Rat
	t.SetFloat64(tolerance)
	t2.Mul(&t, &t)
	if l2.Sign() == 0 || dot.Sign() <= 0 {
		v2 := polylineRatDot(&v, &v)
		return v2.Cmp(&t2) <= 0
	}
	if dot.Cmp(&l2) >= 0 {
		w := polylineRatSub(&pv, &bv)
		w2 := polylineRatDot(&w, &w)
		return w2.Cmp(&t2) <= 0
	}
	v2 := polylineRatDot(&v, &v)
	var left, square, right big.Rat
	left.Mul(&v2, &l2)
	square.Mul(&dot, &dot)
	left.Sub(&left, &square)
	right.Mul(&t2, &l2)
	return left.Cmp(&right) <= 0
}

func polylineSegmentsIntersect(a, b, c, d Point3D) bool {
	// Float64 coordinate comparisons are exact order comparisons for finite data.
	for axis := 0; axis < 3; axis++ {
		av, bv, cv, dv := polylineCoord(a, axis), polylineCoord(b, axis), polylineCoord(c, axis), polylineCoord(d, axis)
		if av > bv {
			av, bv = bv, av
		}
		if cv > dv {
			cv, dv = dv, cv
		}
		if bv < cv || dv < av {
			return false
		}
	}
	av, bv, cv, dv := polylineRatPoint(a), polylineRatPoint(b), polylineRatPoint(c), polylineRatPoint(d)
	ab, ac, ad := polylineRatSub(&bv, &av), polylineRatSub(&cv, &av), polylineRatSub(&dv, &av)
	normal := polylineRatCross(&ab, &ac)
	volume := polylineRatDot(&normal, &ad)
	if volume.Sign() != 0 {
		return false
	} // Skew lines cannot intersect in 3D.
	axis := -1
	for i := range normal {
		if normal[i].Sign() != 0 {
			axis = i
			break
		}
	}
	if axis < 0 {
		normal = polylineRatCross(&ab, &ad)
		for i := range normal {
			if normal[i].Sign() != 0 {
				axis = i
				break
			}
		}
	}
	if axis < 0 {
		return true
	} // All four collinear; overlapping AABBs suffice.
	x, y := (axis+1)%3, (axis+2)%3
	s1 := polylineOrient(&av, &bv, &cv, x, y)
	s2 := polylineOrient(&av, &bv, &dv, x, y)
	s3 := polylineOrient(&cv, &dv, &av, x, y)
	s4 := polylineOrient(&cv, &dv, &bv, x, y)
	if s1 == 0 && polylineOnBox(c, a, b) {
		return true
	}
	if s2 == 0 && polylineOnBox(d, a, b) {
		return true
	}
	if s3 == 0 && polylineOnBox(a, c, d) {
		return true
	}
	if s4 == 0 && polylineOnBox(b, c, d) {
		return true
	}
	return s1*s2 < 0 && s3*s4 < 0
}
func polylineOrient(a, b, c *polylineRatVec, x, y int) int {
	var bx, by, cx, cy, left, right big.Rat
	bx.Sub(&b[x], &a[x])
	by.Sub(&b[y], &a[y])
	cx.Sub(&c[x], &a[x])
	cy.Sub(&c[y], &a[y])
	left.Mul(&bx, &cy)
	right.Mul(&by, &cx)
	left.Sub(&left, &right)
	return left.Sign()
}
func polylineCoord(p Point3D, axis int) float64 {
	if axis == 0 {
		return p.X
	}
	if axis == 1 {
		return p.Y
	}
	return p.Z
}
func polylineOnBox(p, a, b Point3D) bool {
	for axis := 0; axis < 3; axis++ {
		v, x, y := polylineCoord(p, axis), polylineCoord(a, axis), polylineCoord(b, axis)
		if x > y {
			x, y = y, x
		}
		if v < x || v > y {
			return false
		}
	}
	return true
}

// Round the exact rational squared distance only for the diagnostic. Taking the
// square root before conversion avoids losing a representable subnormal length
// merely because its square is smaller than float64's minimum positive value.
func polylineExactSegmentDistance(p, a, b Point3D) float64 {
	pv, av, bv := polylineRatPoint(p), polylineRatPoint(a), polylineRatPoint(b)
	d, v := polylineRatSub(&bv, &av), polylineRatSub(&pv, &av)
	l2, dot := polylineRatDot(&d, &d), polylineRatDot(&v, &d)
	var distance2 big.Rat
	if l2.Sign() == 0 || dot.Sign() <= 0 {
		distance2 = polylineRatDot(&v, &v)
	} else if dot.Cmp(&l2) >= 0 {
		w := polylineRatSub(&pv, &bv)
		distance2 = polylineRatDot(&w, &w)
	} else {
		v2 := polylineRatDot(&v, &v)
		var square big.Rat
		square.Mul(&dot, &dot)
		square.Quo(&square, &l2)
		distance2.Sub(&v2, &square)
	}
	var diagnostic big.Float
	diagnostic.SetPrec(128)
	diagnostic.SetRat(&distance2)
	diagnostic.Sqrt(&diagnostic)
	result, _ := diagnostic.Float64()
	return result
}
