package registration

import (
	"fmt"
	"math"
)

// Translation2D has parameters tx,ty and no fixed parameters.
type Translation2D struct{ linear2D }

// Euler2D has angle (radians),tx,ty and fixed centre cx,cy.
type Euler2D struct{ linear2D }

// Similarity2D has scale,angle,tx,ty and fixed centre cx,cy. Scale can be
// finite zero or negative; an inverse requires a nonsingular mapping.
type Similarity2D struct{ linear2D }

// Affine2D has row-major m00,m01,m10,m11,tx,ty and fixed centre cx,cy.
type Affine2D struct{ linear2D }
type linear2D struct {
	kind                        Kind
	matrix                      Matrix2
	center, translation, offset Point2
	angle, scale                float64
}

func NewTranslation2D() *Translation2D {
	return &Translation2D{linear2D{kind: KindTranslation, matrix: Identity2()}}
}
func NewEuler2D() *Euler2D { return &Euler2D{linear2D{kind: KindEuler2D, matrix: Identity2()}} }
func NewSimilarity2D() *Similarity2D {
	return &Similarity2D{linear2D{kind: KindSimilarity2D, matrix: Identity2(), scale: 1}}
}
func NewAffine2D() *Affine2D                                     { return &Affine2D{linear2D{kind: KindAffine, matrix: Identity2()}} }
func (b *linear2D) Kind() Kind                                   { return b.kind }
func (b *linear2D) Matrix() Matrix2                              { return b.matrix }
func (b *linear2D) Center() Point2                               { return b.center }
func (b *linear2D) Translation() Point2                          { return b.translation }
func (b *linear2D) Offset() Point2                               { return b.offset }
func (b *linear2D) TransformPoint(p Point2) Point2               { return b.matrix.MulVec(p).Add(b.offset) }
func (b *linear2D) TransformVector(p Point2) Point2              { return b.matrix.MulVec(p) }
func (b *linear2D) JacobianWithRespectToPosition(Point2) Matrix2 { return b.matrix }
func (b *linear2D) NumberOfParameters() int {
	switch b.kind {
	case KindTranslation:
		return 2
	case KindEuler2D:
		return 3
	case KindSimilarity2D:
		return 4
	default:
		return 6
	}
}
func (b *linear2D) Parameters() []float64 {
	t := b.translation
	switch b.kind {
	case KindTranslation:
		return []float64{t[0], t[1]}
	case KindEuler2D:
		return []float64{b.angle, t[0], t[1]}
	case KindSimilarity2D:
		return []float64{b.scale, b.angle, t[0], t[1]}
	default:
		return []float64{b.matrix[0][0], b.matrix[0][1], b.matrix[1][0], b.matrix[1][1], t[0], t[1]}
	}
}
func (b *linear2D) FixedParameters() []float64 {
	if b.kind == KindTranslation {
		return []float64{}
	}
	return []float64{b.center[0], b.center[1]}
}
func (b *linear2D) set(p []float64, c Point2) error {
	if err := checkLength(p, b.NumberOfParameters(), string(b.kind)+" parameters"); err != nil {
		return err
	}
	if !finitePoint2(c) {
		return fmt.Errorf("%w: non-finite 2D centre", ErrInvalidParameters)
	}
	n := *b
	n.center = c
	switch b.kind {
	case KindTranslation:
		n.matrix = Identity2()
		n.translation = Point2{p[0], p[1]}
		n.center = Point2{}
	case KindEuler2D, KindSimilarity2D:
		n.scale = 1
		n.angle = p[0]
		start := 1
		if b.kind == KindSimilarity2D {
			n.scale = p[0]
			n.angle = p[1]
			start = 2
		}
		s, co := math.Sincos(n.angle)
		n.matrix = Matrix2{{n.scale * co, -n.scale * s}, {n.scale * s, n.scale * co}}
		n.translation = Point2{p[start], p[start+1]}
	case KindAffine:
		n.matrix = Matrix2{{p[0], p[1]}, {p[2], p[3]}}
		n.translation = Point2{p[4], p[5]}
	default:
		return ErrUnsupportedTransform
	}
	n.offset = n.translation.Add(n.center).Sub(n.matrix.MulVec(n.center))
	if !n.matrix.Finite() || !finitePoint2(n.offset) {
		return fmt.Errorf("%w: derived 2D matrix/offset overflow", ErrInvalidParameters)
	}
	*b = n
	return nil
}
func (b *linear2D) SetParameters(p []float64) error { return b.set(p, b.center) }
func (b *linear2D) SetFixedParameters(p []float64) error {
	if b.kind == KindTranslation {
		return checkLength(p, 0, "Translation2D fixed parameters")
	}
	if err := checkLength(p, 2, "2D fixed parameters"); err != nil {
		return err
	}
	return b.SetCenter(Point2{p[0], p[1]})
}

// SetCenter keeps the translation. Translation2D only accepts a zero centre.
func (b *linear2D) SetCenter(c Point2) error {
	if b.kind == KindTranslation && c != (Point2{}) {
		return fmt.Errorf("%w: translation has no centre", ErrInvalidParameters)
	}
	return b.set(b.Parameters(), c)
}
func (b *linear2D) SetTranslation(t Point2) error {
	p := b.Parameters()
	p[len(p)-2], p[len(p)-1] = t[0], t[1]
	return b.SetParameters(p)
}
func (b *linear2D) SetOffset(o Point2) error {
	if !finitePoint2(o) {
		return ErrInvalidParameters
	}
	return b.SetTranslation(o.Sub(b.center).Add(b.matrix.MulVec(b.center)))
}
func (b *linear2D) UpdateParameters(u []float64, f float64) error {
	if err := checkUpdate(u, b.NumberOfParameters(), f); err != nil {
		return err
	}
	p := b.Parameters()
	for i := range p {
		p[i] += f * u[i]
	}
	return b.SetParameters(p)
}
func (b *linear2D) JacobianWithRespectToParameters(p Point2, j *Jacobian2D) {
	j.reset(b.NumberOfParameters())
	r := p.Sub(b.center)
	switch b.kind {
	case KindTranslation:
		j.set(0, 0, 1)
		j.set(1, 1, 1)
	case KindAffine:
		for i := 0; i < 2; i++ {
			for k := 0; k < 2; k++ {
				j.set(i, 2*i+k, r[k])
			}
			j.set(i, 4+i, 1)
		}
	default:
		s, c := math.Sincos(b.angle)
		rot := Point2{c*r[0] - s*r[1], s*r[0] + c*r[1]}
		d := Point2{-s*r[0] - c*r[1], c*r[0] - s*r[1]}
		angleCol, transCol := 0, 1
		if b.kind == KindSimilarity2D {
			j.set(0, 0, rot[0])
			j.set(1, 0, rot[1])
			d = d.Scale(b.scale)
			angleCol, transCol = 1, 2
		}
		j.set(0, angleCol, d[0])
		j.set(1, angleCol, d[1])
		j.set(0, transCol, 1)
		j.set(1, transCol+1, 1)
	}
}
func (t *Translation2D) Clone() Transform2D { c := *t; return &c }
func (t *Euler2D) Clone() Transform2D       { c := *t; return &c }
func (t *Similarity2D) Clone() Transform2D  { c := *t; return &c }
func (t *Affine2D) Clone() Transform2D      { c := *t; return &c }
func (t *Affine2D) SetMatrix(m Matrix2) error {
	return t.SetParameters([]float64{m[0][0], m[0][1], m[1][0], m[1][1], t.translation[0], t.translation[1]})
}

// Inverse returns an affine representation in the reverse direction.
func (b *linear2D) Inverse() (*Affine2D, error) {
	m, err := b.matrix.Inverse()
	if err != nil {
		return nil, err
	}
	o := m.MulVec(b.offset).Scale(-1)
	t := NewAffine2D()
	t.center = b.center
	if err := t.SetMatrix(m); err != nil {
		return nil, err
	}
	if err := t.SetOffset(o); err != nil {
		return nil, err
	}
	return t, nil
}
