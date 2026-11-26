package registration

import "github.com/ThalesMMS/dicom-go/render"

// Translation3D maps p to p+t. Its three parameters are tx,ty,tz; it has
// no fixed parameters or centre. Use NewTranslation3D for the identity.
type Translation3D struct{ translation render.Vec3 }

func NewTranslation3D() *Translation3D         { return &Translation3D{} }
func (*Translation3D) Kind() Kind              { return KindTranslation }
func (*Translation3D) NumberOfParameters() int { return 3 }
func (t *Translation3D) Parameters() []float64 {
	return []float64{t.translation.X, t.translation.Y, t.translation.Z}
}
func (t *Translation3D) SetParameters(p []float64) error {
	if err := checkLength(p, 3, "Translation3D parameters"); err != nil {
		return err
	}
	t.translation = vecFrom(p)
	return nil
}
func (*Translation3D) FixedParameters() []float64 { return []float64{} }
func (*Translation3D) SetFixedParameters(p []float64) error {
	return checkLength(p, 0, "Translation3D fixed parameters")
}
func (t *Translation3D) SetTranslation(v render.Vec3) error {
	return t.SetParameters([]float64{v.X, v.Y, v.Z})
}
func (t *Translation3D) UpdateParameters(u []float64, f float64) error { return addUpdate(t, u, f) }
func (t *Translation3D) TransformPoint(p render.Vec3) render.Vec3      { return p.Add(t.translation) }
func (*Translation3D) TransformVector(p render.Vec3) render.Vec3       { return p }
func (*Translation3D) Matrix() Matrix3                                 { return Identity3() }
func (*Translation3D) Center() render.Vec3                             { return render.Vec3{} }
func (t *Translation3D) Offset() render.Vec3                           { return t.translation }
func (t *Translation3D) Translation() render.Vec3                      { return t.translation }
func (t *Translation3D) GeometryAffine() render.GeometryAffine {
	return geometryAffine(Identity3(), t.translation)
}
func (*Translation3D) JacobianWithRespectToPosition(render.Vec3) Matrix3 { return Identity3() }
func (*Translation3D) JacobianWithRespectToParameters(_ render.Vec3, j *Jacobian) {
	j.reset(3)
	for i := 0; i < 3; i++ {
		j.set(i, i, 1)
	}
}
func (t *Translation3D) Clone() Transform { c := *t; return &c }
func (t *Translation3D) Inverse() (*Translation3D, error) {
	c := *t
	c.translation = c.translation.Scale(-1)
	return &c, nil
}
