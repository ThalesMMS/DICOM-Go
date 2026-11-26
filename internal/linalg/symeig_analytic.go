package linalg

import "math"

// SymmetricEigenvalues3 returns the eigenvalues, in ascending order, of the
// real symmetric 3×3 matrix
//
//	| a00 a01 a02 |
//	| a01 a11 a12 |
//	| a02 a12 a22 |
//
// in closed form, without iterating or allocating: the trigonometric
// solution of the characteristic cubic (O. K. Smith, "Eigenvalues of a
// symmetric 3 × 3 matrix", Commun. ACM 4(4), 1961). It is meant for per-voxel
// analysis of Hessians and structure tensors, where SymmetricEigen3's
// iterative Jacobi method and its allocations are too slow. The matrix is
// scaled by its largest entry before solving, so the result neither
// overflows nor underflows for any finite input. Eigenvalues are accurate to
// a few units of round-off relative to the largest one; close to a repeated
// eigenvalue the pair loses up to half the digits of that relative accuracy
// (about 1e-8 of the largest), which is below float32 resolution. A diagonal
// matrix is sorted exactly. The input must be finite: a non-diagonal matrix
// with an infinite or NaN entry gives NaN eigenvalues.
func SymmetricEigenvalues3(a00, a01, a02, a11, a12, a22 float64) [3]float64 {
	if a01 == 0 && a02 == 0 && a12 == 0 {
		return sort3(a00, a11, a22)
	}
	scale := math.Max(math.Max(math.Max(math.Abs(a00), math.Abs(a01)), math.Max(math.Abs(a02), math.Abs(a11))),
		math.Max(math.Abs(a12), math.Abs(a22)))
	if !(scale > 0) || math.IsInf(scale, 0) {
		nan := math.NaN()
		return [3]float64{nan, nan, nan}
	}
	inv := 1 / scale
	a00, a01, a02, a11, a12, a22 = a00*inv, a01*inv, a02*inv, a11*inv, a12*inv, a22*inv
	q := (a00 + a11 + a22) / 3
	b00, b11, b22 := a00-q, a11-q, a22-q
	p2 := b00*b00 + b11*b11 + b22*b22 + 2*(a01*a01+a02*a02+a12*a12)
	p := math.Sqrt(p2 / 6)
	if p == 0 {
		return [3]float64{q * scale, q * scale, q * scale}
	}
	// det(B)/2 with B = (A − qI)/p.
	det := b00*(b11*b22-a12*a12) - a01*(a01*b22-a12*a02) + a02*(a01*a12-b11*a02)
	r := det / (2 * p * p * p)
	r = math.Max(-1, math.Min(1, r))
	phi := math.Acos(r) / 3
	largest := q + 2*p*math.Cos(phi)
	smallest := q + 2*p*math.Cos(phi+2*math.Pi/3)
	middle := 3*q - largest - smallest
	return sort3(smallest*scale, middle*scale, largest*scale)
}

// SymmetricEigenvalues2 returns the eigenvalues, in ascending order, of the
// real symmetric 2×2 matrix [[a00, a01], [a01, a11]] in closed form. The
// eigenvalue of smaller magnitude is taken from the determinant, so it keeps
// full relative accuracy.
func SymmetricEigenvalues2(a00, a01, a11 float64) [2]float64 {
	if a01 == 0 {
		if a00 <= a11 {
			return [2]float64{a00, a11}
		}
		return [2]float64{a11, a00}
	}
	mean := (a00 + a11) / 2
	radius := math.Hypot((a00-a11)/2, a01)
	var big float64
	if mean >= 0 {
		big = mean + radius
	} else {
		big = mean - radius
	}
	small := 0.0
	if big != 0 {
		// Product of the eigenvalues, without cancellation when a00·a11 ≈ a01².
		small = (a00*a11 - a01*a01) / big
	}
	if small <= big {
		return [2]float64{small, big}
	}
	return [2]float64{big, small}
}

func sort3(a, b, c float64) [3]float64 {
	if a > b {
		a, b = b, a
	}
	if b > c {
		b, c = c, b
	}
	if a > b {
		a, b = b, a
	}
	return [3]float64{a, b, c}
}
