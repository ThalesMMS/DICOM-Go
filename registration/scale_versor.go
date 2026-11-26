package registration

import (
	"fmt"
	"github.com/ThalesMMS/dicom-go/render"
)

// ScaleVersor3D is ITK's additive scale/rotation family: M=R(v)+diag(s-1).
// Parameters are vx,vy,vz,tx,ty,tz,sx,sy,sz; fixed parameters are cx,cy,cz.
// Finite zero/negative scales are valid forward mappings; inverse may fail.
type ScaleVersor3D struct{ scaleVersorBase }

// ScaleSkewVersor3D adds k0..k5 after the nine ScaleVersor3D parameters,
// with K=[[0,k0,k1],[k2,0,k3],[k4,k5,0]]. M=R(v)+diag(s-1)+K.
// A matrix cannot uniquely determine these fifteen parameters; no SetMatrix
// decomposition is offered. Both families use the existing versor update rule.
type ScaleSkewVersor3D struct{ scaleVersorBase }
type scaleVersorBase struct {
	matrixOffset
	versor      Versor
	scale       render.Vec3
	skew        [6]float64
	derivatives [3]Matrix3
	withSkew    bool
}

var skewPositions = [6][2]int{{0, 1}, {0, 2}, {1, 0}, {1, 2}, {2, 0}, {2, 1}}

func newScaleVersorBase(skew bool) scaleVersorBase {
	b := scaleVersorBase{withSkew: skew}
	p := []float64{0, 0, 0, 0, 0, 0, 1, 1, 1}
	if skew {
		p = append(p, make([]float64, 6)...)
	}
	_ = b.SetParameters(p)
	return b
}
func NewScaleVersor3D() *ScaleVersor3D         { return &ScaleVersor3D{newScaleVersorBase(false)} }
func NewScaleSkewVersor3D() *ScaleSkewVersor3D { return &ScaleSkewVersor3D{newScaleVersorBase(true)} }
func (b *scaleVersorBase) Kind() Kind {
	if b.withSkew {
		return KindScaleSkewVersor3D
	}
	return KindScaleVersor3D
}
func (b *scaleVersorBase) NumberOfParameters() int {
	if b.withSkew {
		return 15
	}
	return 9
}
func (b *scaleVersorBase) Parameters() []float64 {
	p := []float64{b.versor.X, b.versor.Y, b.versor.Z, b.translation.X, b.translation.Y, b.translation.Z, b.scale.X, b.scale.Y, b.scale.Z}
	if b.withSkew {
		p = append(p, b.skew[:]...)
	}
	return p
}
func (b *scaleVersorBase) FixedParameters() []float64 { return b.centerFixedParameters() }
func (b *scaleVersorBase) set(p []float64, c render.Vec3) error {
	if err := checkLength(p, b.NumberOfParameters(), string(b.Kind())+" parameters"); err != nil {
		return err
	}
	if !finiteVec(c) || !finite(vecFrom(p).Length()) {
		return fmt.Errorf("%w: centre/versor norm overflow", ErrInvalidParameters)
	}
	q, err := versorFromParameters(p)
	if err != nil {
		return err
	}
	return b.commit(p, c, q)
}
func (b *scaleVersorBase) commit(p []float64, c render.Vec3, q Versor) error {
	if err := checkLength(p, b.NumberOfParameters(), "scale versor parameters"); err != nil {
		return err
	}
	if !finiteVec(c) {
		return fmt.Errorf("%w: non-finite centre", ErrInvalidParameters)
	}
	next := *b
	next.versor = q
	next.center = c
	next.translation = vecFrom(p[3:])
	next.scale = vecFrom(p[6:])
	next.skew = [6]float64{}
	if b.withSkew {
		copy(next.skew[:], p[9:])
	}
	next.matrix = q.Matrix()
	scales := [3]float64{next.scale.X, next.scale.Y, next.scale.Z}
	for i := 0; i < 3; i++ {
		next.matrix[i][i] += scales[i] - 1
	}
	for i, pos := range skewPositions {
		next.matrix[pos[0]][pos[1]] += next.skew[i]
	}
	next.computeOffset()
	if !next.matrix.Finite() || !finiteVec(next.offset) {
		return fmt.Errorf("%w: derived scale/skew matrix or offset overflow", ErrInvalidParameters)
	}
	next.derivatives = q.rightPartDerivatives()
	*b = next
	return nil
}
func (b *scaleVersorBase) SetParameters(p []float64) error { return b.set(p, b.center) }
func (b *scaleVersorBase) SetFixedParameters(p []float64) error {
	if err := checkLength(p, 3, "scale versor fixed parameters"); err != nil {
		return err
	}
	return b.SetCenter(vecFrom(p))
}
func (b *scaleVersorBase) SetCenter(c render.Vec3) error {
	return b.commit(b.Parameters(), c, b.versor)
}
func (b *scaleVersorBase) SetTranslation(t render.Vec3) error {
	p := b.Parameters()
	p[3], p[4], p[5] = t.X, t.Y, t.Z
	return b.commit(p, b.center, b.versor)
}
func (b *scaleVersorBase) SetOffset(o render.Vec3) error {
	if !finiteVec(o) {
		return fmt.Errorf("%w: non-finite offset", ErrInvalidParameters)
	}
	return b.SetTranslation(o.Sub(b.center).Add(b.matrix.MulVec(b.center)))
}
func (b *scaleVersorBase) UpdateParameters(u []float64, f float64) error {
	if err := checkUpdate(u, b.NumberOfParameters(), f); err != nil {
		return err
	}
	q, err := composeVersorUpdate(b.versor, u, f)
	if err != nil {
		return err
	}
	p := b.Parameters()
	p[0], p[1], p[2] = q.X, q.Y, q.Z
	for i := 3; i < len(p); i++ {
		p[i] += f * u[i]
	}
	return b.commit(p, b.center, q)
}
func (b *scaleVersorBase) JacobianWithRespectToParameters(p render.Vec3, j *Jacobian) {
	j.reset(b.NumberOfParameters())
	r := p.Sub(b.center)
	rotationTranslationJacobian(j, b.derivatives[:], r, 3)
	rv := [3]float64{r.X, r.Y, r.Z}
	for i := 0; i < 3; i++ {
		j.set(i, 6+i, rv[i])
	}
	if b.withSkew {
		for i, pos := range skewPositions {
			j.set(pos[0], 9+i, rv[pos[1]])
		}
	}
}
func (t *ScaleVersor3D) Clone() Transform     { c := *t; return &c }
func (t *ScaleSkewVersor3D) Clone() Transform { c := *t; return &c }

// Inverse returns an exact affine representation, or ErrSingularMatrix.
func (b *scaleVersorBase) Inverse() (*Affine, error) {
	m, offset, err := b.inverse()
	if err != nil {
		return nil, err
	}
	out := NewAffine()
	out.center = b.center
	out.matrix = m
	t := offset.Sub(out.center).Add(m.MulVec(out.center))
	if !finiteVec(offset) || !finiteVec(t) {
		return nil, ErrSingularMatrix
	}
	out.translation = t
	out.offset = offset
	return out, nil
}
