package registration

import (
	"errors"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
	"math"
	"reflect"
	"testing"
)

func TestTranslation3DContract(t *testing.T) {
	v := NewTranslation3D()
	if err := v.SetParameters([]float64{1.25, -2.5, 3.75}); err != nil {
		t.Fatal(err)
	}
	p := render.Vec3{X: 2, Y: -3, Z: 4}
	want := render.Vec3{X: 3.25, Y: -5.5, Z: 7.75}
	if v.TransformPoint(p) != want || v.Matrix() != Identity3() || len(v.FixedParameters()) != 0 || v.Center() != (render.Vec3{}) {
		t.Fatal("translation geometry")
	}
	var j Jacobian
	v.JacobianWithRespectToParameters(p, &j)
	for i := 0; i < 3; i++ {
		for k := 0; k < 3; k++ {
			w := 0.0
			if i == k {
				w = 1
			}
			if j.At(i, k) != w {
				t.Fatal(j)
			}
		}
	}
	inv, err := v.Inverse()
	if err != nil || inv.TransformPoint(want) != p {
		t.Fatal("inverse", err)
	}
	before := v.Parameters()
	copy := v.Clone()
	_ = copy.SetParameters([]float64{0, 0, 0})
	for _, bad := range [][]float64{{1}, {math.NaN(), 0, 0}, {math.Inf(1), 0, 0}} {
		if !errors.Is(v.SetParameters(bad), ErrInvalidParameters) || !reflect.DeepEqual(v.Parameters(), before) {
			t.Fatal("atomic parameters")
		}
	}
	if !errors.Is(v.SetFixedParameters([]float64{1}), ErrInvalidParameters) || transformType(v) != spatialreg.TransformRigid {
		t.Fatal("fixed/DICOM")
	}
	pvalues := v.Parameters()
	pvalues[0] = 99
	if v.Parameters()[0] != before[0] {
		t.Fatal("copy")
	}
	if err := v.UpdateParameters([]float64{math.MaxFloat64, 0, 0}, 2); !errors.Is(err, ErrInvalidParameters) || !reflect.DeepEqual(v.Parameters(), before) {
		t.Fatal("atomic update", err)
	}
}
