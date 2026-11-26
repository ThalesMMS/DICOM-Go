package registration

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// The chain applies a translation followed by two centred affines in the
// given order. Written as equations, its stages are:
// A(x,y,z) = (x+2, y-3, z+1)
// B(x,y,z) = (x+2y, -y+z+8, 2z+1)
// C(x,y,z) = (-y, 2x+9, y+z+1).
func itkCompositeFixture(t *testing.T) (*Composite, []Transform) {
	t.Helper()
	first := NewTranslation3D()
	must(t, first.SetParameters([]float64{2, -3, 1}))
	middle := NewAffine()
	must(t, middle.SetFixedParameters([]float64{1, 2, -2}))
	must(t, middle.SetParameters([]float64{1, 2, 0, 0, -1, 1, 0, 0, 2, 4, 2, -1}))
	last := NewAffine()
	must(t, last.SetFixedParameters([]float64{-2, 3, 1}))
	must(t, last.SetParameters([]float64{0, -1, 0, 2, 0, 0, 0, 1, 1, -1, 2, 4}))
	stages := []Transform{first, middle, last}
	chain, err := NewComposite(stages...)
	must(t, err)
	return chain, stages
}

// itkCompositeTransformTest exercises parameter order and the chain rule
// when intermediary transforms are held fixed during optimization.
func TestITKCompositeActiveStages(t *testing.T) {
	point := render.Vec3{X: 1, Y: 2, Z: 3}
	wantPoint := render.Vec3{X: -13, Y: 11, Z: 23}
	wantPosition := Matrix3{{0, 1, -1}, {2, 4, 0}, {0, -1, 3}}
	// The centered input to B is (2,-3,6). Its columns are carried through C.
	// The centered input to C is (3,10,8).
	columns := [][][3]float64{
		{{0, 2, 0}, {1, 4, -1}, {-1, 0, 3}},
		{{0, 4, 0}, {0, -6, 0}, {0, 12, 0}, {-2, 0, 2}, {3, 0, -3}, {-6, 0, 6},
			{0, 0, 2}, {0, 0, -3}, {0, 0, 6}, {0, 2, 0}, {-1, 0, 1}, {0, 0, 1}},
		{{3, 0, 0}, {10, 0, 0}, {8, 0, 0}, {0, 3, 0}, {0, 10, 0}, {0, 8, 0},
			{0, 0, 3}, {0, 0, 10}, {0, 0, 8}, {1, 0, 0}, {0, 1, 0}, {0, 0, 1}},
	}
	tests := []struct {
		name   string
		active [3]bool
	}{
		{"none", [3]bool{}},
		{"first", [3]bool{true, false, false}},
		{"middle", [3]bool{false, true, false}},
		{"last", [3]bool{false, false, true}},
		{"first-and-middle", [3]bool{true, true, false}},
		{"first-and-last", [3]bool{true, false, true}},
		{"middle-and-last", [3]bool{false, true, true}},
		{"all", [3]bool{true, true, true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chain, stages := itkCompositeFixture(t)
			var before [3][]float64
			var wantParameters []float64
			var wantColumns [][3]float64
			for i, active := range test.active {
				must(t, chain.SetOptimized(i, active))
				before[i] = stages[i].Parameters()
				if active {
					wantParameters = append(wantParameters, before[i]...)
					wantColumns = append(wantColumns, columns[i]...)
				}
			}
			if got := chain.TransformPoint(point); got != wantPoint {
				t.Fatalf("mapped point %+v, want %+v", got, wantPoint)
			}
			if got := chain.JacobianWithRespectToPosition(point); got != wantPosition {
				t.Fatalf("position Jacobian %v, want %v", got, wantPosition)
			}
			if got := chain.Parameters(); !slices.Equal(got, wantParameters) {
				t.Fatalf("active parameters %v, want %v", got, wantParameters)
			}
			if got := chain.FixedParameters(); !slices.Equal(got, []float64{1, 2, -2, -2, 3, 1}) {
				t.Fatalf("fixed parameter layout %v", got)
			}
			var jac Jacobian
			chain.JacobianWithRespectToParameters(point, &jac)
			if jac.Columns != len(wantColumns) || len(jac.Values) != 3*len(wantColumns) {
				t.Fatalf("parameter Jacobian has %d columns and %d values, want 3 × %d", jac.Columns, len(jac.Values), len(wantColumns))
			}
			for column, want := range wantColumns {
				got := [3]float64{jac.At(0, column), jac.At(1, column), jac.At(2, column)}
				if got != want {
					t.Fatalf("column %d: %v, want %v", column, got, want)
				}
			}

			update := make([]float64, len(wantParameters))
			for i := range update {
				update[i] = float64(i + 1)
				wantParameters[i] += update[i] / 2
			}
			must(t, chain.UpdateParameters(update, .5))
			if got := chain.Parameters(); !slices.Equal(got, wantParameters) {
				t.Fatalf("updated parameters %v, want %v", got, wantParameters)
			}
			for i, stage := range stages {
				if !test.active[i] && !slices.Equal(stage.Parameters(), before[i]) {
					t.Fatalf("update changed frozen stage %d: %v, want %v", i, stage.Parameters(), before[i])
				}
			}
		})
	}
}

// itkTransformCloneTest checks component types and optimization flags after
// cloning. Mutating a nested clone must leave the original stages independent.
func TestITKCompositeCloneOwnsNestedStages(t *testing.T) {
	inner, original := itkCompositeFixture(t)
	must(t, inner.SetOptimized(1, false))
	outer, err := NewComposite(inner, NewTranslation3D())
	must(t, err)
	must(t, outer.SetOptimized(1, false))

	point := render.Vec3{X: 1, Y: 2, Z: 3}
	before := outer.TransformPoint(point)
	copyOfOuter, ok := outer.Clone().(*Composite)
	if !ok || copyOfOuter.Len() != 2 || !copyOfOuter.Optimized(0) || copyOfOuter.Optimized(1) {
		t.Fatalf("cloned outer chain lost shape or optimization flags: %+v", copyOfOuter)
	}
	copyOfInner, ok := copyOfOuter.Transform(0).(*Composite)
	if !ok || copyOfInner.Len() != 3 || copyOfInner == inner || copyOfInner.Optimized(1) {
		t.Fatalf("cloned inner chain lost independence, shape or frozen stage: %+v", copyOfInner)
	}
	for i, stage := range original {
		cloned := copyOfInner.Transform(i)
		if cloned == stage || cloned.Kind() != stage.Kind() || !slices.Equal(cloned.Parameters(), stage.Parameters()) ||
			!slices.Equal(cloned.FixedParameters(), stage.FixedParameters()) || copyOfInner.Optimized(i) != inner.Optimized(i) {
			t.Fatalf("stage %d was shared or lost parameters or its flag", i)
		}
	}
	if got := copyOfOuter.TransformPoint(point); got != before {
		t.Fatalf("cloned chain maps to %+v, want %+v", got, before)
	}
	must(t, copyOfInner.SetOptimized(1, true))
	must(t, copyOfInner.Transform(1).SetFixedParameters([]float64{1, 1, -2}))
	must(t, copyOfInner.Transform(2).(*Affine).SetTranslation(render.Vec3{X: -1, Y: 6, Z: 4}))
	if got := copyOfOuter.TransformPoint(point); got != (render.Vec3{X: -11, Y: 19, Z: 21}) {
		t.Fatalf("updated clone maps to %+v", got)
	}
	if inner.Optimized(1) || !slices.Equal(original[1].FixedParameters(), []float64{1, 2, -2}) || outer.TransformPoint(point) != before {
		t.Fatal("mutating nested clone changed original flags, centre or mapping")
	}
}

func TestITKCompositeRejectsLateInvalidParametersAtomically(t *testing.T) {
	tests := []struct {
		name    string
		invalid func(*Composite) error
	}{
		{"parameter-count", func(c *Composite) error { return c.SetParameters([]float64{7, 8, 9}) }},
		{"late-parameter", func(c *Composite) error {
			p := c.Parameters()
			p[0], p[len(p)-1] = 7, math.NaN()
			return c.SetParameters(p)
		}},
		{"late-fixed-parameter", func(c *Composite) error {
			p := c.FixedParameters()
			p[0], p[len(p)-1] = 7, math.Inf(1)
			return c.SetFixedParameters(p)
		}},
		{"late-update-overflow", func(c *Composite) error {
			p := make([]float64, c.NumberOfParameters())
			p[0], p[len(p)-1] = 1, math.MaxFloat64
			return c.UpdateParameters(p, 2)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chain, _ := itkCompositeFixture(t)
			before, fixed := chain.Parameters(), chain.FixedParameters()
			point := render.Vec3{X: -2, Y: 4, Z: 1}
			want := chain.TransformPoint(point)
			if err := test.invalid(chain); !errors.Is(err, ErrInvalidParameters) {
				t.Fatalf("write returned %v, want ErrInvalidParameters", err)
			}
			if !slices.Equal(chain.Parameters(), before) || !slices.Equal(chain.FixedParameters(), fixed) || chain.TransformPoint(point) != want {
				t.Fatal("rejected write partially changed the chain")
			}
		})
	}
}
