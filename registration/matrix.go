package registration

import (
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// Matrix3 is a row-major 3×3 matrix acting on column vectors: element
// [row][column].
type Matrix3 [3][3]float64

// Identity3 returns the 3×3 identity.
func Identity3() Matrix3 {
	return Matrix3{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
}

// Mul returns m·n.
func (m Matrix3) Mul(n Matrix3) Matrix3 {
	var out Matrix3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			out[i][j] = m[i][0]*n[0][j] + m[i][1]*n[1][j] + m[i][2]*n[2][j]
		}
	}
	return out
}

// MulVec returns m·v.
func (m Matrix3) MulVec(v render.Vec3) render.Vec3 {
	return render.Vec3{
		X: m[0][0]*v.X + m[0][1]*v.Y + m[0][2]*v.Z,
		Y: m[1][0]*v.X + m[1][1]*v.Y + m[1][2]*v.Z,
		Z: m[2][0]*v.X + m[2][1]*v.Y + m[2][2]*v.Z,
	}
}

// Scale returns s·m.
func (m Matrix3) Scale(s float64) Matrix3 {
	for i := range m {
		for j := range m[i] {
			m[i][j] *= s
		}
	}
	return m
}

// Transpose returns mᵀ.
func (m Matrix3) Transpose() Matrix3 {
	return Matrix3{
		{m[0][0], m[1][0], m[2][0]},
		{m[0][1], m[1][1], m[2][1]},
		{m[0][2], m[1][2], m[2][2]},
	}
}

// Determinant returns det(m).
func (m Matrix3) Determinant() float64 {
	return m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
}

// Finite reports whether every element is finite.
func (m Matrix3) Finite() bool {
	for i := range m {
		for _, v := range m[i] {
			if !finite(v) {
				return false
			}
		}
	}
	return true
}

// maxCondition bounds the infinity-norm condition number accepted by
// Inverse, matching spatialreg's rejection of ill-conditioned matrices.
const maxCondition = 1e10

// Inverse returns m⁻¹. Non-finite, singular or ill-conditioned matrices
// (infinity-norm condition number above 1e10) return ErrSingularMatrix.
func (m Matrix3) Inverse() (Matrix3, error) {
	if !m.Finite() {
		return Matrix3{}, ErrInvalidMatrix
	}
	det := m.Determinant()
	norm := m.infinityNorm()
	if norm == 0 || !finite(det) || math.Abs(det) <= 1e-15*norm*norm*norm {
		return Matrix3{}, ErrSingularMatrix
	}
	inv := 1 / det
	out := Matrix3{
		{(m[1][1]*m[2][2] - m[1][2]*m[2][1]) * inv, (m[0][2]*m[2][1] - m[0][1]*m[2][2]) * inv, (m[0][1]*m[1][2] - m[0][2]*m[1][1]) * inv},
		{(m[1][2]*m[2][0] - m[1][0]*m[2][2]) * inv, (m[0][0]*m[2][2] - m[0][2]*m[2][0]) * inv, (m[0][2]*m[1][0] - m[0][0]*m[1][2]) * inv},
		{(m[1][0]*m[2][1] - m[1][1]*m[2][0]) * inv, (m[0][1]*m[2][0] - m[0][0]*m[2][1]) * inv, (m[0][0]*m[1][1] - m[0][1]*m[1][0]) * inv},
	}
	condition := norm * out.infinityNorm()
	if !out.Finite() || !finite(condition) || condition > maxCondition {
		return Matrix3{}, ErrSingularMatrix
	}
	return out, nil
}

func (m Matrix3) infinityNorm() float64 {
	maximum := 0.0
	for i := range m {
		maximum = math.Max(maximum, math.Abs(m[i][0])+math.Abs(m[i][1])+math.Abs(m[i][2]))
	}
	return maximum
}

// orthogonal reports whether m·mᵀ is the identity within tolerance per
// element, as ITK's Rigid3DTransform::MatrixIsOrthogonal checks.
func (m Matrix3) orthogonal(tolerance float64) bool {
	product := m.Mul(m.Transpose())
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			want := 0.0
			if i == j {
				want = 1
			}
			if !(math.Abs(product[i][j]-want) <= tolerance) {
				return false
			}
		}
	}
	return true
}

// Jacobian is the 3×Columns matrix ∂T(p)/∂parameters at one point, stored
// row-major: Values[row*Columns+column], row 0..2 being the x, y and z
// components of the mapped point.
type Jacobian struct {
	Columns int
	Values  []float64
}

// NewJacobian allocates a zero Jacobian for columns parameters.
func NewJacobian(columns int) Jacobian {
	if columns < 0 {
		columns = 0
	}
	return Jacobian{Columns: columns, Values: make([]float64, 3*columns)}
}

// At returns element (row, column).
func (j Jacobian) At(row, column int) float64 { return j.Values[row*j.Columns+column] }

func (j *Jacobian) set(row, column int, v float64) { j.Values[row*j.Columns+column] = v }

// reset resizes j to columns parameters, reusing its storage when large
// enough, and zeroes it.
func (j *Jacobian) reset(columns int) {
	n := 3 * columns
	if cap(j.Values) < n {
		j.Values = make([]float64, n)
	} else {
		j.Values = j.Values[:n]
		for i := range j.Values {
			j.Values[i] = 0
		}
	}
	j.Columns = columns
}

// setColumn writes v into column.
func (j *Jacobian) setColumn(column int, v render.Vec3) {
	j.set(0, column, v.X)
	j.set(1, column, v.Y)
	j.set(2, column, v.Z)
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func finiteVec(v render.Vec3) bool { return finite(v.X) && finite(v.Y) && finite(v.Z) }

func finiteSlice(values []float64) bool {
	for _, v := range values {
		if !finite(v) {
			return false
		}
	}
	return true
}
