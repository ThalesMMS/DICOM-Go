package registration

import (
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
)

func itk1345CheckJacobian2(t *testing.T, tr Transform2D, p Point2) {
	t.Helper()
	var j Jacobian2D
	tr.JacobianWithRespectToParameters(p, &j)
	parameters := tr.Parameters()
	if j.Columns != len(parameters) || len(j.Values) != 2*len(parameters) {
		t.Fatal("Jacobian dimensions", j)
	}
	const h = 1e-6
	for k := range parameters {
		plus, minus := tr.Clone(), tr.Clone()
		pp, pm := append([]float64(nil), parameters...), append([]float64(nil), parameters...)
		pp[k] += h
		pm[k] -= h
		if err := plus.SetParameters(pp); err != nil {
			t.Fatal(err)
		}
		if err := minus.SetParameters(pm); err != nil {
			t.Fatal(err)
		}
		want := plus.TransformPoint(p).Sub(minus.TransformPoint(p)).Scale(1 / (2 * h))
		for row := 0; row < 2; row++ {
			if math.Abs(j.At(row, k)-want[row]) > 2e-7 {
				t.Fatalf("Jacobian row%d col%d got%g want%g", row, k, j.At(row, k), want[row])
			}
		}
	}
}

func TestLinear2DContracts(t *testing.T) {
	cases := []struct {
		name              string
		tr                Transform2D
		parameters, fixed []float64
	}{
		{"translation", NewTranslation2D(), []float64{2, -3}, []float64{}},
		{"euler", NewEuler2D(), []float64{.37, 2, -3}, []float64{4, -2}},
		{"similarity", NewSimilarity2D(), []float64{1.4, .37, 2, -3}, []float64{4, -2}},
		{"affine", NewAffine2D(), []float64{1.2, .3, -.2, .8, 2, -3}, []float64{4, -2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.tr.SetFixedParameters(c.fixed); err != nil {
				t.Fatal(err)
			}
			if err := c.tr.SetParameters(c.parameters); err != nil {
				t.Fatal(err)
			}
			m := c.tr.(MatrixTransform2D)
			point := Point2{8, -5}
			want := m.Matrix().MulVec(point.Sub(m.Center())).Add(m.Center()).Add(m.Translation())
			got := c.tr.TransformPoint(point)
			for i := range got {
				if math.Abs(got[i]-want[i]) > 1e-12 {
					t.Fatalf("center/translation mapping got%v want%v", got, want)
				}
			}
			itk1345CheckJacobian2(t, c.tr, point)
			if c.tr.JacobianWithRespectToPosition(point) != m.Matrix() {
				t.Fatal("position Jacobian")
			}
			clone := c.tr.Clone()
			p := clone.Parameters()
			p[len(p)-1]++
			if err := clone.SetParameters(p); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.tr.Parameters(), c.parameters) {
				t.Fatal("clone changed source")
			}
			p = c.tr.Parameters()
			p[0] = math.NaN()
			if err := c.tr.SetParameters(p); !errors.Is(err, ErrInvalidParameters) || !reflect.DeepEqual(c.tr.Parameters(), c.parameters) {
				t.Fatal("nonfinite setter not atomic", err)
			}
			if err := c.tr.SetParameters(c.parameters[:len(c.parameters)-1]); err == nil {
				t.Fatal("short parameters admitted")
			}
			p = c.tr.Parameters()
			p[0] = 99
			if !reflect.DeepEqual(c.tr.Parameters(), c.parameters) {
				t.Fatal("getter aliases source")
			}
			inv, err := c.tr.(interface{ Inverse() (*Affine2D, error) }).Inverse()
			if err != nil {
				t.Fatal(err)
			}
			back := inv.TransformPoint(c.tr.TransformPoint(point))
			for i := range point {
				if math.Abs(back[i]-point[i]) > 1e-12 {
					t.Fatal("inverse", back)
				}
			}
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for n := 0; n < 100; n++ {
						_ = c.tr.TransformPoint(point)
						_ = c.tr.Parameters()
						_ = c.tr.FixedParameters()
						var j Jacobian2D
						c.tr.JacobianWithRespectToParameters(point, &j)
					}
				}()
			}
			wg.Wait()
		})
	}
}

func TestLinear2DForwardDomainsAndAtomicDerivedOverflow(t *testing.T) {
	s := NewSimilarity2D()
	for _, scale := range []float64{0, -2} {
		if err := s.SetParameters([]float64{scale, .3, 1, -2}); err != nil {
			t.Fatal(err)
		}
		_, err := s.Inverse()
		if scale == 0 && !errors.Is(err, ErrSingularMatrix) {
			t.Fatal("zero scale inverse", err)
		}
		if scale != 0 && err != nil {
			t.Fatal(err)
		}
	}
	a := NewAffine2D()
	if err := a.SetMatrix(Matrix2{{2, 0}, {0, 1}}); err != nil {
		t.Fatal(err)
	}
	before := a.Clone()
	if err := a.SetCenter(Point2{math.MaxFloat64, 0}); err == nil || !reflect.DeepEqual(a, before) {
		t.Fatal("derived center overflow not atomic", err)
	}
	if err := a.UpdateParameters([]float64{math.MaxFloat64, 0, 0, 0, 0, 0}, 2); err == nil || !reflect.DeepEqual(a, before) {
		t.Fatal("update overflow not atomic", err)
	}
	tr := NewTranslation2D()
	if err := tr.SetCenter(Point2{1, 0}); err == nil {
		t.Fatal("translation center admitted")
	}
	for _, m := range []Matrix2{{{1, 0}, {0, 0}}, {{1, 0}, {0, 1e-12}}, {{math.Inf(1), 0}, {0, 1}}} {
		if _, err := m.Inverse(); err == nil {
			t.Fatal("invalid/ill-conditioned inverse admitted", m)
		}
	}
	m := Matrix2{{1e200, 0}, {0, 1e200}}
	inverse, err := m.Inverse()
	if err != nil || inverse != (Matrix2{{1e-200, 0}, {0, 1e-200}}) {
		t.Fatal("scaled inverse", inverse, err)
	}
}

type itk1345Custom2D struct{ *Translation2D }

func TestComposite2DOrderOptimizationAndAdmission(t *testing.T) {
	a, b := NewTranslation2D(), NewAffine2D()
	if err := a.SetParameters([]float64{2, -3}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetParameters([]float64{2, .3, -.4, 1.5, 1, -2}); err != nil {
		t.Fatal(err)
	}
	c, err := NewComposite2D(a, b)
	if err != nil {
		t.Fatal(err)
	}
	p := Point2{8, -5}
	want := b.TransformPoint(a.TransformPoint(p))
	if c.TransformPoint(p) != want || c.TransformPoint(p) == a.TransformPoint(b.TransformPoint(p)) {
		t.Fatal("application order")
	}
	itk1345CheckJacobian2(t, c, p)
	if c.JacobianWithRespectToPosition(p) != b.Matrix().Mul(a.Matrix()) {
		t.Fatal("position chain")
	}
	if err := c.SetOptimized(0, false); err != nil {
		t.Fatal(err)
	}
	if c.NumberOfParameters() != b.NumberOfParameters() || !reflect.DeepEqual(c.Parameters(), b.Parameters()) {
		t.Fatal("optimized subset")
	}
	itk1345CheckJacobian2(t, c, p)
	old := c.Parameters()
	bad := append([]float64(nil), old...)
	bad[len(bad)-1] = math.Inf(1)
	if err := c.SetParameters(bad); err == nil || !reflect.DeepEqual(c.Parameters(), old) {
		t.Fatal("composite setter atomic", err)
	}
	clone := c.Clone().(*Composite2D)
	if err := clone.SetOptimized(0, true); err != nil {
		t.Fatal(err)
	}
	if c.Optimized(0) || clone.Transform(0) == a || clone.Transform(1) == b {
		t.Fatal("clone flags/ownership")
	}
	if err := c.SetOptimized(9, true); err == nil {
		t.Fatal("invalid flag index")
	}
	var nilTranslation *Translation2D
	for _, bad := range []Transform2D{nil, nilTranslation, c, &itk1345Custom2D{NewTranslation2D()}} {
		if err := c.Append(bad); !errors.Is(err, ErrUnsupportedTransform) {
			t.Fatal("unsupported component admitted", err)
		}
	}
	empty, err := NewComposite2D()
	if err != nil || empty.TransformPoint(p) != p {
		t.Fatal("empty identity", err)
	}
	for i := 0; i < 1024; i++ {
		if err := empty.Append(NewTranslation2D()); err != nil {
			t.Fatal(err)
		}
	}
	if err := empty.Append(NewTranslation2D()); !errors.Is(err, ErrInvalidParameters) {
		t.Fatal("component budget", err)
	}
}

func TestComposite2DReferenceAliasingRefused(t *testing.T) {
	child := NewTranslation2D()
	if err := child.SetParameters([]float64{1, 2}); err != nil {
		t.Fatal(err)
	}
	before := child.Parameters()
	if c, err := NewComposite2D(child, child); !errors.Is(err, ErrInvalidParameters) || c != nil || !reflect.DeepEqual(child.Parameters(), before) {
		t.Fatal("duplicate constructor reference admitted or changed source", err)
	}
	c, err := NewComposite2D(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Append(child); !errors.Is(err, ErrInvalidParameters) || c.Len() != 1 || !reflect.DeepEqual(child.Parameters(), before) || !reflect.DeepEqual(c.Parameters(), before) {
		t.Fatal("duplicate Append changed state", err)
	}
	independent := child.Clone()
	if err := c.Append(independent); err != nil {
		t.Fatal(err)
	}
	if c.Transform(0) == c.Transform(1) || c.NumberOfParameters() != 4 {
		t.Fatal("independent parameter blocks")
	}
	itk1345CheckJacobian2(t, c, Point2{3, 4})
	if err := c.SetParameters([]float64{3, 4, 5, 6}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(child.Parameters(), []float64{3, 4}) || !reflect.DeepEqual(independent.Parameters(), []float64{5, 6}) || c.TransformPoint(Point2{}) != (Point2{8, 10}) {
		t.Fatal("independent setters/order")
	}
	if err := c.UpdateParameters([]float64{1, 2, 3, 4}, 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(child.Parameters(), []float64{4, 6}) || !reflect.DeepEqual(independent.Parameters(), []float64{8, 10}) {
		t.Fatal("independent update")
	}
	cloned := c.Clone().(*Composite2D)
	if cloned.Transform(0) == cloned.Transform(1) || cloned.Transform(0) == child || cloned.TransformPoint(Point2{}) != c.TransformPoint(Point2{}) {
		t.Fatal("clone parameter mapping/ownership")
	}
	itk1345CheckJacobian2(t, cloned, Point2{3, 4})
}

func TestComposite2DDistinctUpdateAtomicity(t *testing.T) {
	a, b := NewTranslation2D(), NewTranslation2D()
	if err := b.SetParameters([]float64{.75 * math.MaxFloat64, 0}); err != nil {
		t.Fatal(err)
	}
	c, err := NewComposite2D(a, b)
	if err != nil {
		t.Fatal(err)
	}
	before := c.Parameters()
	if err := c.UpdateParameters([]float64{1, 2, .4 * math.MaxFloat64, 0}, 1); !errors.Is(err, ErrInvalidParameters) || !reflect.DeepEqual(c.Parameters(), before) {
		t.Fatal("later component update overflow changed originals", err)
	}
	if a.TransformPoint(Point2{}) != (Point2{}) {
		t.Fatal("first original changed before later validation")
	}
}
