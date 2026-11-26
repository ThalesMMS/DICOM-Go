package registration

import "math"

// Point2 is a point in the caller's two-dimensional physical coordinate
// basis. It is not implicitly an XY slice of DICOM patient coordinates.
type Point2 [2]float64

func (p Point2) Add(q Point2) Point2    { return Point2{p[0] + q[0], p[1] + q[1]} }
func (p Point2) Sub(q Point2) Point2    { return Point2{p[0] - q[0], p[1] - q[1]} }
func (p Point2) Scale(s float64) Point2 { return Point2{p[0] * s, p[1] * s} }
func finitePoint2(p Point2) bool        { return finite(p[0]) && finite(p[1]) }

// Matrix2 is row-major and acts on column vectors.
type Matrix2 [2][2]float64

func Identity2() Matrix2 { return Matrix2{{1, 0}, {0, 1}} }
func (m Matrix2) Mul(n Matrix2) Matrix2 {
	var o Matrix2
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			o[i][j] = m[i][0]*n[0][j] + m[i][1]*n[1][j]
		}
	}
	return o
}
func (m Matrix2) MulVec(p Point2) Point2 {
	return Point2{m[0][0]*p[0] + m[0][1]*p[1], m[1][0]*p[0] + m[1][1]*p[1]}
}
func (m Matrix2) Finite() bool         { return finiteSlice(m[0][:]) && finiteSlice(m[1][:]) }
func (m Matrix2) Determinant() float64 { return m[0][0]*m[1][1] - m[0][1]*m[1][0] }
func (m Matrix2) infinityNorm() float64 {
	return math.Max(math.Abs(m[0][0])+math.Abs(m[0][1]), math.Abs(m[1][0])+math.Abs(m[1][1]))
}

// Inverse rejects non-finite/singular matrices and condition numbers above
// 1e10. Scaling avoids overflow merely from a large finite determinant.
func (m Matrix2) Inverse() (Matrix2, error) {
	if !m.Finite() {
		return Matrix2{}, ErrInvalidMatrix
	}
	s := 0.0
	for _, row := range m {
		for _, v := range row {
			s = math.Max(s, math.Abs(v))
		}
	}
	if s == 0 {
		return Matrix2{}, ErrSingularMatrix
	}
	a := m
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			a[i][j] /= s
		}
	}
	d := a.Determinant()
	norm := a.infinityNorm()
	if !finite(d) || math.Abs(d) <= 1e-15*norm*norm {
		return Matrix2{}, ErrSingularMatrix
	}
	b := Matrix2{{a[1][1] / d, -a[0][1] / d}, {-a[1][0] / d, a[0][0] / d}}
	condition := norm * b.infinityNorm()
	if !finite(condition) || condition > maxCondition {
		return Matrix2{}, ErrSingularMatrix
	}
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			b[i][j] /= s
		}
	}
	if !b.Finite() {
		return Matrix2{}, ErrSingularMatrix
	}
	return b, nil
}

// Jacobian2D is a two-row, row-major parameter Jacobian. A zero value is
// ready to be passed as the output of JacobianWithRespectToParameters.
type Jacobian2D struct {
	Columns int
	Values  []float64
}

func (j Jacobian2D) At(r, c int) float64      { return j.Values[r*j.Columns+c] }
func (j *Jacobian2D) set(r, c int, v float64) { j.Values[r*j.Columns+c] = v }
func (j *Jacobian2D) reset(n int) {
	if cap(j.Values) < 2*n {
		j.Values = make([]float64, 2*n)
	} else {
		j.Values = j.Values[:2*n]
		clear(j.Values)
	}
	j.Columns = n
}

// Transform2D is independent of Transform. Points stay in the caller's
// two-dimensional basis; no lifting, registration engine or DICOM mapping
// is implied. Parameter getters copy. Setters validate atomically. Mutation
// must not run concurrently; independent read-only calls are safe.
type Transform2D interface {
	Kind() Kind
	TransformPoint(Point2) Point2
	NumberOfParameters() int
	Parameters() []float64
	SetParameters([]float64) error
	FixedParameters() []float64
	SetFixedParameters([]float64) error
	UpdateParameters([]float64, float64) error
	JacobianWithRespectToParameters(Point2, *Jacobian2D)
	JacobianWithRespectToPosition(Point2) Matrix2
	Clone() Transform2D
}

// MatrixTransform2D maps p to M*p+offset; offset=t+c-M*c.
type MatrixTransform2D interface {
	Transform2D
	Matrix() Matrix2
	Offset() Point2
	Center() Point2
	Translation() Point2
}
