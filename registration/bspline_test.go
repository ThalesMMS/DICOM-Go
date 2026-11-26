package registration

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// obliqueRotation is a rotation of about 20° around (1, 2, 3).
func obliqueRotation(t *testing.T) Matrix3 {
	t.Helper()
	v := NewVersorRigid3D()
	if err := v.SetParameters([]float64{0.05, 0.1, 0.15, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	return v.Matrix()
}

func randomBSpline(t testing.TB, size [3]int, origin, spacing render.Vec3, direction Matrix3, amplitude float64, seed int64) *BSpline {
	t.Helper()
	b, err := NewBSpline(size, origin, spacing, direction)
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(seed))
	parameters := make([]float64, b.NumberOfParameters())
	for i := range parameters {
		parameters[i] = amplitude * (2*random.Float64() - 1)
	}
	if err := b.SetParameters(parameters); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestBSplineDomainLayoutIsITKs: the grid of a transform domain and the
// eighteen fixed parameters follow ITK's SetFixedParametersFromTransform-
// DomainInformation.
func TestBSplineDomainLayoutIsITKs(t *testing.T) {
	direction := Matrix3{{0, -1, 0}, {1, 0, 0}, {0, 0, 1}}
	b, err := NewBSplineForDomain(render.Vec3{X: -10, Y: 5, Z: 2}, render.Vec3{X: 40, Y: 30, Z: 20}, direction, [3]int{4, 3, 2})
	if err != nil {
		t.Fatal(err)
	}
	// Spacing 10 per axis; the grid starts one spacing before the domain
	// along the direction's columns: (−10, 5, 2) + D·(−10, −10, −10).
	want := []float64{7, 6, 5, -10 + 10, 5 - 10, 2 - 10, 10, 10, 10, 0, -1, 0, 1, 0, 0, 0, 0, 1}
	got := b.FixedParameters()
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Fatalf("fixed parameters %v, want %v", got, want)
		}
	}
	if b.NumberOfParameters() != 3*7*6*5 || b.MeshSize() != [3]int{4, 3, 2} {
		t.Fatalf("%d parameters, mesh %v", b.NumberOfParameters(), b.MeshSize())
	}
	other := NewBSplineIdentityForTest(t)
	if err := other.SetFixedParameters(got); err != nil {
		t.Fatal(err)
	}
	if other.GridSize() != b.GridSize() || other.GridOrigin() != b.GridOrigin() || other.GridSpacing() != b.GridSpacing() || other.GridDirection() != b.GridDirection() {
		t.Fatalf("fixed parameters do not round-trip")
	}
	if other.NumberOfParameters() != b.NumberOfParameters() {
		t.Fatalf("resized to %d parameters", other.NumberOfParameters())
	}
}

func NewBSplineIdentityForTest(t *testing.T) *BSpline {
	t.Helper()
	b, err := NewBSpline([3]int{4, 4, 4}, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestBSplineTransformPointIsTheKernelSum: a point maps by the sum of every
// control point's displacement weighted by the tensor cubic kernel, and
// stays in place outside the valid region.
func TestBSplineTransformPointIsTheKernelSum(t *testing.T) {
	size := [3]int{6, 7, 5}
	b := randomBSpline(t, size, render.Vec3{X: -3, Y: 4, Z: 1}, render.Vec3{X: 5, Y: 4, Z: 6}, obliqueRotation(t), 3, 1)
	geometry, err := newImageGeometry(size, b.GridOrigin(), b.GridSpacing(), b.GridDirection())
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(2))
	parameters := b.Parameters()
	count := size[0] * size[1] * size[2]
	inside, outside := 0, 0
	for trial := 0; trial < 2000; trial++ {
		index := render.Vec3{X: random.Float64()*7 - 1, Y: random.Float64()*8 - 1, Z: random.Float64()*6 - 1}
		p := geometry.Point(index)
		valid := index.X >= 1 && index.X < 4 && index.Y >= 1 && index.Y < 5 && index.Z >= 1 && index.Z < 3
		var want render.Vec3
		if valid {
			inside++
			for k := 0; k < count; k++ {
				x, y, z := k%size[0], k/size[0]%size[1], k/(size[0]*size[1])
				w := cubicBSpline(index.X-float64(x)) * cubicBSpline(index.Y-float64(y)) * cubicBSpline(index.Z-float64(z))
				want = want.Add(render.Vec3{X: parameters[k], Y: parameters[count+k], Z: parameters[2*count+k]}.Scale(w))
			}
		} else {
			outside++
		}
		got := b.TransformPoint(p).Sub(p)
		if got.Sub(want).Length() > 1e-12 {
			t.Fatalf("index %v (valid %v): displacement %v, want %v", index, valid, got, want)
		}
	}
	if inside < 50 || outside < 50 {
		t.Fatalf("%d inside, %d outside", inside, outside)
	}
}

// TestBSplineJacobiansAreAnalytic: the parameter Jacobian is exact (the
// mapping is linear in the parameters) and the position Jacobian matches
// central differences.
func TestBSplineJacobiansAreAnalytic(t *testing.T) {
	b := randomBSpline(t, [3]int{6, 6, 6}, render.Vec3{X: -10, Y: -10, Z: -10}, render.Vec3{X: 8, Y: 9, Z: 10}, obliqueRotation(t), 4, 3)
	geometry, _ := newImageGeometry([3]int{6, 6, 6}, b.GridOrigin(), b.GridSpacing(), b.GridDirection())
	random := rand.New(rand.NewSource(4))
	var jacobian Jacobian
	for trial := 0; trial < 20; trial++ {
		p := geometry.Point(render.Vec3{X: 1.05 + 2.9*random.Float64(), Y: 1.05 + 2.9*random.Float64(), Z: 1.05 + 2.9*random.Float64()})
		b.JacobianWithRespectToParameters(p, &jacobian)
		base := b.TransformPoint(p)
		for _, parameter := range []int{random.Intn(b.NumberOfParameters()), random.Intn(b.NumberOfParameters())} {
			moved := b.Clone()
			delta := make([]float64, b.NumberOfParameters())
			delta[parameter] = 1
			if err := moved.UpdateParameters(delta, 1); err != nil {
				t.Fatal(err)
			}
			change := moved.TransformPoint(p).Sub(base)
			want := render.Vec3{X: jacobian.At(0, parameter), Y: jacobian.At(1, parameter), Z: jacobian.At(2, parameter)}
			if change.Sub(want).Length() > 1e-12 {
				t.Fatalf("parameter %d: change %v, Jacobian %v", parameter, change, want)
			}
		}
		position := b.JacobianWithRespectToPosition(p)
		const h = 1e-5
		for axis := 0; axis < 3; axis++ {
			var step render.Vec3
			switch axis {
			case 0:
				step.X = h
			case 1:
				step.Y = h
			case 2:
				step.Z = h
			}
			numeric := b.TransformPoint(p.Add(step)).Sub(b.TransformPoint(p.Sub(step))).Scale(1 / (2 * h))
			for row, v := range []float64{numeric.X, numeric.Y, numeric.Z} {
				if math.Abs(position[row][axis]-v) > 1e-7 {
					t.Fatalf("∂T%d/∂x%d = %v, numeric %v", row, axis, position[row][axis], v)
				}
			}
		}
	}
}

// TestBSplineRefineKeepsTheMapping: doubling the mesh along all or some
// axes maps every point of the domain as before.
func TestBSplineRefineKeepsTheMapping(t *testing.T) {
	b := randomBSpline(t, [3]int{6, 5, 7}, render.Vec3{X: -12, Y: 3, Z: -4}, render.Vec3{X: 7, Y: 6, Z: 5}, obliqueRotation(t), 5, 5)
	geometry, _ := newImageGeometry(b.GridSize(), b.GridOrigin(), b.GridSpacing(), b.GridDirection())
	for _, axes := range [][3]bool{{true, true, true}, {true, false, false}, {false, true, true}} {
		fine, err := b.Refine(axes)
		if err != nil {
			t.Fatal(err)
		}
		for axis := 0; axis < 3; axis++ {
			want := b.MeshSize()[axis]
			if axes[axis] {
				want *= 2
			}
			if fine.MeshSize()[axis] != want {
				t.Fatalf("axes %v: mesh %v", axes, fine.MeshSize())
			}
		}
		random := rand.New(rand.NewSource(6))
		for trial := 0; trial < 300; trial++ {
			// The domain is index [1, size − 2) of the coarse grid.
			index := render.Vec3{X: 1 + 2.999*random.Float64(), Y: 1 + 1.999*random.Float64(), Z: 1 + 3.999*random.Float64()}
			p := geometry.Point(index)
			if d := fine.TransformPoint(p).Sub(b.TransformPoint(p)).Length(); d > 1e-11 {
				t.Fatalf("axes %v, index %v: refined mapping differs by %g mm", axes, index, d)
			}
		}
	}
}

func TestBSplineRejectsInvalidGrids(t *testing.T) {
	if _, err := NewBSpline([3]int{3, 4, 4}, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3()); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("grid of 3: %v", err)
	}
	if _, err := NewBSpline([3]int{4, 4, 4}, render.Vec3{}, render.Vec3{X: 1, Y: 0, Z: 1}, Identity3()); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("zero spacing: %v", err)
	}
	if _, err := NewBSplineForDomain(render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3(), [3]int{0, 1, 1}); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("zero mesh: %v", err)
	}
	b := NewBSplineIdentityForTest(t)
	fixed := b.FixedParameters()
	fixed[0] = 4.5
	if err := b.SetFixedParameters(fixed); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("fractional size: %v", err)
	}
	if err := b.SetParameters(make([]float64, 5)); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("short parameters: %v", err)
	}
	bad := make([]float64, b.NumberOfParameters())
	bad[7] = math.NaN()
	if err := b.SetParameters(bad); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("NaN parameter: %v", err)
	}
}

// TestCompositeSparseJacobianMatchesTheDenseOne: a B-spline followed by a
// frozen affine has local support, and its sparse Jacobian equals the
// composite's chain-rule Jacobian.
func TestCompositeSparseJacobianMatchesTheDenseOne(t *testing.T) {
	b := randomBSpline(t, [3]int{5, 6, 5}, render.Vec3{X: -10, Y: -10, Z: -10}, render.Vec3{X: 10, Y: 9, Z: 11}, Identity3(), 3, 7)
	affine := NewAffine()
	if err := affine.SetParameters([]float64{1.1, 0.1, -0.05, 0.02, 0.95, 0.1, -0.03, 0.04, 1.05, 3, -2, 1}); err != nil {
		t.Fatal(err)
	}
	c, err := NewComposite(b, affine)
	if err != nil {
		t.Fatal(err)
	}
	if c.localSupport() {
		t.Fatal("two optimized components reported local support")
	}
	if err := c.SetOptimized(1, false); err != nil {
		t.Fatal(err)
	}
	if !c.localSupport() {
		t.Fatal("B-spline then frozen affine has no local support")
	}
	var dense Jacobian
	var sparse sparseJacobian
	for _, p := range []render.Vec3{{X: 3, Y: 4, Z: 5}, {X: 12.5, Y: 5, Z: 20}, {X: 0, Y: 0, Z: 0}} {
		c.JacobianWithRespectToParameters(p, &dense)
		c.sparseJacobianAt(p, &sparse)
		got := NewJacobian(c.NumberOfParameters())
		for k := 0; k < sparse.n; k++ {
			for d := 0; d < 3; d++ {
				column := sparse.post.MulVec(render.Vec3{X: b2f(d == 0), Y: b2f(d == 1), Z: b2f(d == 2)}).Scale(sparse.weights[k])
				got.setColumn(d*sparse.stride+sparse.indices[k], column)
			}
		}
		for i := range dense.Values {
			if math.Abs(dense.Values[i]-got.Values[i]) > 1e-12 {
				t.Fatalf("point %v: Jacobian entry %d: dense %v, sparse %v", p, i, dense.Values[i], got.Values[i])
			}
		}
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func TestMembraneEnergyGradient(t *testing.T) {
	b := randomBSpline(t, [3]int{5, 4, 6}, render.Vec3{}, render.Vec3{X: 3, Y: 4, Z: 5}, Identity3(), 2, 8)
	energy, gradient := b.MembraneEnergy(true)
	if !(energy > 0) {
		t.Fatalf("energy %v", energy)
	}
	const h = 1e-6
	for _, i := range []int{0, 17, 60, b.NumberOfParameters() - 1} {
		up, down := b.Clone().(*BSpline), b.Clone().(*BSpline)
		up.coefficients[i] += h
		down.coefficients[i] -= h
		eu, _ := up.MembraneEnergy(false)
		ed, _ := down.MembraneEnergy(false)
		if numeric := (eu - ed) / (2 * h); math.Abs(numeric-gradient[i]) > 1e-6 {
			t.Fatalf("parameter %d: gradient %v, numeric %v", i, gradient[i], numeric)
		}
	}
	zero := NewBSplineIdentityForTest(t)
	if e, _ := zero.MembraneEnergy(false); e != 0 {
		t.Fatalf("identity energy %v", e)
	}
}

// denseOnly hides the local support of a transform, so that the Mattes
// metric takes its dense derivative path.
type denseOnly struct{ Transform }

// TestMattesLocalDerivativeMatchesTheDensePath: the two-pass sparse
// derivative of a B-spline equals the dense bins × bins × parameters one
// and central differences of the value.
func TestMattesLocalDerivativeMatchesTheDensePath(t *testing.T) {
	fixed, moving := ffdTestImages(t, [3]int{20, 18, 10}, 1, 0.8)
	metric, err := NewMattesMutualInformation(fixed, moving, MetricDomain{}, 24)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBSplineForImage(fixed, [3]int{3, 3, 2})
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(9))
	parameters := make([]float64, b.NumberOfParameters())
	for i := range parameters {
		parameters[i] = 1.5 * (2*random.Float64() - 1)
	}
	if err := b.SetParameters(parameters); err != nil {
		t.Fatal(err)
	}
	value, sparse, err := metric.ValueAndDerivative(b)
	if err != nil {
		t.Fatal(err)
	}
	denseValue, dense, err := metric.ValueAndDerivative(denseOnly{b})
	if err != nil {
		t.Fatal(err)
	}
	if value != denseValue {
		t.Fatalf("values %v and %v", value, denseValue)
	}
	var largest float64
	for _, g := range dense {
		largest = math.Max(largest, math.Abs(g))
	}
	for i := range dense {
		if math.Abs(dense[i]-sparse[i]) > 1e-12*largest+1e-15 {
			t.Fatalf("parameter %d: sparse %v, dense %v", i, sparse[i], dense[i])
		}
	}
	// The metric is piecewise smooth; differences of a small step agree
	// with the derivative to a few per cent on the largest components.
	order := 0
	for i := range dense {
		if math.Abs(dense[i]) > math.Abs(dense[order]) {
			order = i
		}
	}
	const h = 1e-4
	up, down := b.Clone(), b.Clone()
	delta := make([]float64, b.NumberOfParameters())
	delta[order] = h
	_ = up.UpdateParameters(delta, 1)
	_ = down.UpdateParameters(delta, -1)
	vu, _ := metric.Value(up)
	vd, _ := metric.Value(down)
	if numeric := (vu - vd) / (2 * h); math.Abs(numeric-sparse[order]) > 0.05*math.Abs(sparse[order]) {
		t.Fatalf("parameter %d: derivative %v, central difference %v", order, sparse[order], numeric)
	}
}

func TestTransformDisplacementFieldSamplesTheMapping(t *testing.T) {
	b := randomBSpline(t, [3]int{6, 6, 6}, render.Vec3{X: -20, Y: -20, Z: -20}, render.Vec3{X: 10, Y: 10, Z: 10}, Identity3(), 3, 10)
	grid := Grid{Size: [3]int{8, 7, 6}, Origin: render.Vec3{X: -5, Y: -4, Z: -3}, Spacing: render.Vec3{X: 2, Y: 2.5, Z: 3}, Direction: Identity3()}
	field, err := TransformDisplacementField(context.Background(), b, grid)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range [][3]int{{0, 0, 0}, {7, 6, 5}, {3, 2, 4}} {
		p := grid.Point(render.Vec3{X: float64(index[0]), Y: float64(index[1]), Z: float64(index[2])})
		if d := field.At(index[0], index[1], index[2]).Sub(b.Displacement(p)).Length(); d > 1e-12 {
			t.Fatalf("sample %v differs by %g", index, d)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := TransformDisplacementField(ctx, b, grid); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}
