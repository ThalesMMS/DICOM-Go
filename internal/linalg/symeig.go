// Package linalg holds small dense linear-algebra kernels shared by the
// geometry, ROI and registration code. It is internal: callers outside the
// module get the results through the public packages that use it.
package linalg

import (
	"errors"
	"math"
	"sort"
)

// ErrInvalidMatrix reports a matrix that is not square, not finite or not
// symmetric.
var ErrInvalidMatrix = errors.New("linalg: invalid symmetric matrix")

// maxJacobiSweeps bounds the cyclic Jacobi iteration. Quadratic convergence
// makes a handful of sweeps enough for n <= 4; the bound only guards against
// pathological input.
const maxJacobiSweeps = 64

// SymmetricEigen returns the eigenvalues of the real symmetric matrix a in
// ascending order and the matching orthonormal eigenvectors as the columns of
// vectors, so a = V·diag(values)·Vᵀ. It uses the cyclic Jacobi method, which
// stays accurate for tiny, zero and repeated eigenvalues (the vectors of a
// repeated eigenvalue are an arbitrary orthonormal basis of its eigenspace)
// and is the right tool for the 2×2, 3×3 and 4×4 matrices of shape moments,
// Hessians and Horn's quaternion method. a is not modified. Asymmetry beyond
// round-off, non-finite entries or a non-square matrix return
// ErrInvalidMatrix.
func SymmetricEigen(a [][]float64) (values []float64, vectors [][]float64, err error) {
	n := len(a)
	scale := 0.0
	for _, row := range a {
		if len(row) != n {
			return nil, nil, ErrInvalidMatrix
		}
		for _, v := range row {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, nil, ErrInvalidMatrix
			}
			scale = math.Max(scale, math.Abs(v))
		}
	}
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
		for j := range m[i] {
			if math.Abs(a[i][j]-a[j][i]) > 1e-9*scale {
				return nil, nil, ErrInvalidMatrix
			}
			m[i][j] = (a[i][j] + a[j][i]) / 2
		}
	}
	v := make([][]float64, n)
	for i := range v {
		v[i] = make([]float64, n)
		v[i][i] = 1
	}
	jacobi(m, v)

	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return m[order[i]][order[i]] < m[order[j]][order[j]] })
	values = make([]float64, n)
	vectors = make([][]float64, n)
	for i := range vectors {
		vectors[i] = make([]float64, n)
	}
	for k, col := range order {
		values[k] = m[col][col]
		for i := 0; i < n; i++ {
			vectors[i][k] = v[i][col]
		}
	}
	return values, vectors, nil
}

// SymmetricEigen3 is SymmetricEigen for a 3×3 matrix: ascending eigenvalues
// and the eigenvectors as the columns of vectors (vectors[i][k] is component i
// of eigenvector k).
func SymmetricEigen3(a [3][3]float64) (values [3]float64, vectors [3][3]float64, err error) {
	rows := [][]float64{a[0][:], a[1][:], a[2][:]}
	w, v, err := SymmetricEigen(rows)
	if err != nil {
		return values, vectors, err
	}
	for i := 0; i < 3; i++ {
		values[i] = w[i]
		for k := 0; k < 3; k++ {
			vectors[i][k] = v[i][k]
		}
	}
	return values, vectors, nil
}

// SymmetricEigen4 is SymmetricEigen for a 4×4 matrix (for example the N
// matrix of Horn's closed-form quaternion registration).
func SymmetricEigen4(a [4][4]float64) (values [4]float64, vectors [4][4]float64, err error) {
	rows := [][]float64{a[0][:], a[1][:], a[2][:], a[3][:]}
	w, v, err := SymmetricEigen(rows)
	if err != nil {
		return values, vectors, err
	}
	for i := 0; i < 4; i++ {
		values[i] = w[i]
		for k := 0; k < 4; k++ {
			vectors[i][k] = v[i][k]
		}
	}
	return values, vectors, nil
}

// jacobi diagonalizes the symmetric m in place by plane rotations, applying
// the same rotations to the columns of v.
func jacobi(m, v [][]float64) {
	n := len(m)
	for sweep := 0; sweep < maxJacobiSweeps; sweep++ {
		off, diag := 0.0, 0.0
		for p := 0; p < n; p++ {
			diag += m[p][p] * m[p][p]
			for q := p + 1; q < n; q++ {
				off += m[p][q] * m[p][q]
			}
		}
		if off == 0 || off <= 1e-32*diag {
			return
		}
		for p := 0; p < n-1; p++ {
			for q := p + 1; q < n; q++ {
				apq := m[p][q]
				if apq == 0 {
					continue
				}
				app, aqq := m[p][p], m[q][q]
				// Once the rotation angle is below round-off relative to
				// the diagonal, zero the entry instead of rotating.
				g := 100 * math.Abs(apq)
				if sweep > 3 && math.Abs(app)+g == math.Abs(app) && math.Abs(aqq)+g == math.Abs(aqq) {
					m[p][q], m[q][p] = 0, 0
					continue
				}
				// tan of the rotation angle, the smaller root of
				// t² + 2θt - 1 = 0 for stability.
				theta := (aqq - app) / (2 * apq)
				var t float64
				if math.Abs(theta) > 1e150 {
					t = 1 / (2 * theta)
				} else {
					t = 1 / (math.Abs(theta) + math.Sqrt(theta*theta+1))
					if theta < 0 {
						t = -t
					}
				}
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := 0; k < n; k++ { // columns p and q
					mkp, mkq := m[k][p], m[k][q]
					m[k][p] = c*mkp - s*mkq
					m[k][q] = s*mkp + c*mkq
				}
				for k := 0; k < n; k++ { // rows p and q
					mpk, mqk := m[p][k], m[q][k]
					m[p][k] = c*mpk - s*mqk
					m[q][k] = s*mpk + c*mqk
				}
				m[p][q], m[q][p] = 0, 0
				for k := 0; k < n; k++ {
					vkp, vkq := v[k][p], v[k][q]
					v[k][p] = c*vkp - s*vkq
					v[k][q] = s*vkp + c*vkq
				}
			}
		}
	}
}
