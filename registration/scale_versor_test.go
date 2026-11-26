package registration

import (
	"errors"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"reflect"
	"sync"
	"testing"
)

func itk1345CheckJacobian3(t *testing.T, v Transform, p render.Vec3) {
	t.Helper()
	var j Jacobian
	v.JacobianWithRespectToParameters(p, &j)
	pars := v.Parameters()
	for k := range pars {
		plus, minus := v.Clone(), v.Clone()
		a, b := append([]float64(nil), pars...), append([]float64(nil), pars...)
		h := 1e-6
		a[k] += h
		b[k] -= h
		if err := plus.SetParameters(a); err != nil {
			t.Fatal(err)
		}
		if err := minus.SetParameters(b); err != nil {
			t.Fatal(err)
		}
		d := plus.TransformPoint(p).Sub(minus.TransformPoint(p)).Scale(1 / (2 * h))
		got := [3]float64{d.X, d.Y, d.Z}
		for r := 0; r < 3; r++ {
			if math.Abs(j.At(r, k)-got[r]) > 2e-7 {
				t.Fatalf("Jacobian%d,%d got%g finiteDiff%g", r, k, j.At(r, k), got[r])
			}
		}
	}
}
func TestScaleVersorAndScaleSkew3DContract(t *testing.T) {
	for _, skew := range []bool{false, true} {
		t.Run(map[bool]string{false: "scale", true: "skew"}[skew], func(t *testing.T) {
			var v MatrixTransform
			if skew {
				v = NewScaleSkewVersor3D()
			} else {
				v = NewScaleVersor3D()
			}
			pars := []float64{.1, .2, .3, 1.5, -2, 3, 1.25, .8, 1.1}
			if skew {
				pars = append(pars, .125, -.25, .375, .5, -.625, .75)
			}
			if err := v.SetFixedParameters([]float64{2, -3, 4}); err != nil {
				t.Fatal(err)
			}
			if err := v.SetParameters(pars); err != nil {
				t.Fatal(err)
			}
			want := Matrix3{{.99, -.5164171097297422, .43094473981982817}, {.5964171097297423, .6, -.06547236990991409}, {-.3109447398198282, .3054723699099141, 1}}
			if skew {
				for i, pos := range skewPositions {
					want[pos[0]][pos[1]] += pars[9+i]
				}
			}
			for i := 0; i < 3; i++ {
				for k := 0; k < 3; k++ {
					if math.Abs(v.Matrix()[i][k]-want[i][k]) > 1e-14 {
						t.Fatalf("matrix%v want%v", v.Matrix(), want)
					}
				}
			}
			p := render.Vec3{X: 1.5, Y: 2.25, Z: -3.5}
			expected := want.MulVec(p.Sub(v.Center())).Add(v.Center()).Add(v.Translation())
			if v.TransformPoint(p).Sub(expected).Length() > 1e-13 {
				t.Fatal("point/center/translation")
			}
			itk1345CheckJacobian3(t, v, p)
			beforeP, beforeF, beforeM, beforeO := v.Parameters(), v.FixedParameters(), v.Matrix(), v.Offset()
			bad := append([]float64(nil), beforeP...)
			bad[6] = math.NaN()
			if !errors.Is(v.SetParameters(bad), ErrInvalidParameters) {
				t.Fatal("NaN")
			}
			bad[6] = math.MaxFloat64
			bad[0] = math.MaxFloat64
			if !errors.Is(v.SetParameters(bad), ErrInvalidParameters) {
				t.Fatal("norm overflow")
			}
			if !errors.Is(v.SetFixedParameters([]float64{math.MaxFloat64, math.MaxFloat64, math.MaxFloat64}), ErrInvalidParameters) {
				t.Fatal("offset overflow")
			}
			if !reflect.DeepEqual(v.Parameters(), beforeP) || !reflect.DeepEqual(v.FixedParameters(), beforeF) || v.Matrix() != beforeM || v.Offset() != beforeO {
				t.Fatal("rejected mutation changed state")
			}
			cp := v.Clone()
			_ = cp.SetFixedParameters([]float64{0, 0, 0})
			if !reflect.DeepEqual(v.FixedParameters(), beforeF) {
				t.Fatal("clone aliases")
			}
			var wg sync.WaitGroup
			for n := 0; n < 4; n++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var j Jacobian
					for i := 0; i < 100; i++ {
						v.TransformPoint(p)
						v.JacobianWithRespectToParameters(p, &j)
						v.Parameters()
					}
				}()
			}
			wg.Wait()
		})
	}
}
func TestScaleVersorDomainsAndUpdate(t *testing.T) {
	for _, v := range []*ScaleVersor3D{NewScaleVersor3D()} {
		for _, s := range []float64{0, -1} {
			p := []float64{0, 0, 0, 0, 0, 0, s, 1, 1}
			if err := v.SetParameters(p); err != nil {
				t.Fatal(err)
			}
			if v.Matrix()[0][0] != s {
				t.Fatal("forward scale domain")
			}
			_, err := v.Inverse()
			if s == 0 && !errors.Is(err, ErrSingularMatrix) {
				t.Fatal("singular inverse", err)
			}
		}
		_ = v.SetParameters([]float64{2, 0, 0, 0, 0, 0, 1, 1, 1})
		if math.Abs(v.Parameters()[0]-.9999999999) > 1e-15 {
			t.Fatal("public clamp", v.Parameters())
		}
		v = NewScaleVersor3D()
		u := []float64{.2, 0, 0, 1, -2, 3, .1, .2, .3}
		if err := v.UpdateParameters(u, .5); err != nil {
			t.Fatal(err)
		}
		q, err := VersorFromAxisAngle(render.Vec3{X: 1}, .1)
		if err != nil {
			t.Fatal(err)
		}
		want := q.Matrix()
		want[0][0] += .05
		want[1][1] += .1
		want[2][2] += .15
		for i := 0; i < 3; i++ {
			for k := 0; k < 3; k++ {
				if math.Abs(v.Matrix()[i][k]-want[i][k]) > 1e-14 {
					t.Fatal("update rule")
				}
			}
		}
		before := v.Parameters()
		u[4] = math.MaxFloat64
		if !errors.Is(v.UpdateParameters(u, 2), ErrInvalidParameters) || !reflect.DeepEqual(before, v.Parameters()) {
			t.Fatal("update overflow not atomic")
		}
	}
}
