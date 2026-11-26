package registration

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"testing"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/optimize"
	"github.com/ThalesMMS/dicom-go/render"
)

func boundedFFDForTests(t *testing.T) FreeFormDeformation {
	t.Helper()
	fixed, moving := ffdTestImages(t, [3]int{10, 10, 10}, 2, .4)
	return FreeFormDeformation{Fixed: fixed, Moving: moving, MeshSize: [3]int{1, 1, 1}, Levels: []FFDLevel{{Level: Level{Shrink: 1}}}, BoundedOptimizer: &FFDBoundedOptions{LowerMM: [3]float64{-1, -1, -1}, UpperMM: [3]float64{1, 1, 1}, MaximumDisplacementGradient: .8, Optimizer: optimize.LBFGSB{MaximumIterations: 2}}}
}

func TestFFDBoundedNewMatrixInitials(t *testing.T) {
	translation, scale, skew := NewTranslation3D(), NewScaleVersor3D(), NewScaleSkewVersor3D()
	if err := translation.SetParameters([]float64{.1, -.2, .3}); err != nil {
		t.Fatal(err)
	}
	if err := scale.SetParameters([]float64{.02, -.03, .01, .1, -.2, .3, 1.1, .9, 1.2}); err != nil {
		t.Fatal(err)
	}
	if err := skew.SetParameters([]float64{.02, -.03, .01, .1, -.2, .3, 1.1, .9, 1.2, .02, -.01, .03, .01, -.02, .01}); err != nil {
		t.Fatal(err)
	}
	for _, initial := range []Transform{translation, scale, skew} {
		t.Run(string(initial.Kind()), func(t *testing.T) {
			ctx := context.Background()
			before := initial.Parameters()
			fixed := initial.FixedParameters()
			I, err := ffdInitialCloneBytes(ctx, initial)
			if err != nil || I != 4096 {
				t.Fatal("small concrete Initial debit", I, err)
			}
			var actual int64
			switch v := initial.(type) {
			case *Translation3D:
				actual = int64(unsafe.Sizeof(*v))
			case *ScaleVersor3D:
				actual = int64(unsafe.Sizeof(*v))
			case *ScaleSkewVersor3D:
				actual = int64(unsafe.Sizeof(*v))
			}
			if 2*actual > 2*I {
				t.Fatal("two live clone allocation reserve", actual, I)
			}
			first, second := initial.Clone(), initial.Clone()
			p := first.Parameters()
			p[3%len(p)] += .1
			if err := first.SetParameters(p); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(second.Parameters(), before) || !reflect.DeepEqual(initial.Parameters(), before) {
				t.Fatal("independent initial clones")
			}
			if err := ffdInitialFinite(ctx, initial); err != nil {
				t.Fatal(err)
			}
			lower, err := ffdInitialDeterminantLower(initial)
			det := initial.(MatrixTransform).Matrix().Determinant()
			if err != nil || !(lower > 0) || lower > det {
				t.Fatal("new concrete determinant certificate", lower, det, err)
			}
			f := boundedFFDForTests(t)
			f.Initial = initial
			const baseC int64 = 8*(192+10*192) + 256 + 8192
			f.BoundedOptimizer.MaxControlPointBytes = baseC + 2*I - 1
			if _, err := prepareFFDBounded(ctx, f, f.Levels, f.MeshSize, [3]bool{true, true, true}); !errors.Is(err, ErrFFDResourceLimit) {
				t.Fatal("two initial clones one byte below", err)
			}
			f.BoundedOptimizer.MaxControlPointBytes++
			if _, err := prepareFFDBounded(ctx, f, f.Levels, f.MeshSize, [3]bool{true, true, true}); err != nil {
				t.Fatal("exact initial clone budget", err)
			}
			f.BoundedOptimizer.LowerMM, f.BoundedOptimizer.UpperMM = [3]float64{}, [3]float64{}
			result, err := f.Run(ctx)
			if err != nil || result.JacobianBound == nil || result.JacobianBound.InitialDeterminantLower != lower {
				t.Fatal("public FFD Initial/certificate", result, err)
			}
			point := render.Vec3{X: 3, Y: 4, Z: 5}
			got, want := result.Transform.TransformPoint(point), initial.TransformPoint(point)
			if got.Sub(want).Length() > 1e-12 {
				t.Fatal("zero-box FFD changed Initial mapping", got, want)
			}
			if !reflect.DeepEqual(initial.Parameters(), before) || !reflect.DeepEqual(initial.FixedParameters(), fixed) {
				t.Fatal("source Initial changed")
			}
		})
	}
}

func TestFFDBoundedNewInitialRefusals(t *testing.T) {
	ctx := context.Background()
	var translation *Translation3D
	var scale *ScaleVersor3D
	var skew *ScaleSkewVersor3D
	for _, initial := range []Transform{translation, scale, skew} {
		if _, err := ffdInitialCloneBytes(ctx, initial); !errors.Is(err, ErrFFD) {
			t.Fatal("typed nil admission", err)
		}
		if _, err := ffdInitialDeterminantLower(initial); !errors.Is(err, ErrFFD) {
			t.Fatal("typed nil certificate", err)
		}
	}
	for _, s := range []float64{0, -1} {
		for _, initial := range []Transform{NewScaleVersor3D(), NewScaleSkewVersor3D()} {
			p := initial.Parameters()
			p[6] = s
			if err := initial.SetParameters(p); err != nil {
				t.Fatal("finite forward domain", err)
			}
			if _, err := ffdInitialCloneBytes(ctx, initial); err != nil {
				t.Fatal("forward admission", err)
			}
			if _, err := ffdInitialDeterminantLower(initial); !errors.Is(err, ErrFFD) {
				t.Fatal("nonpositive certificate admitted", err)
			}
			f := boundedFFDForTests(t)
			f.Initial = initial
			if _, err := prepareFFDBounded(ctx, f, f.Levels, f.MeshSize, [3]bool{true, true, true}); !errors.Is(err, ErrFFD) {
				t.Fatal("certified mode nonpositive Initial", err)
			}
			f.BoundedOptimizer.MaximumDisplacementGradient = 0
			if _, err := prepareFFDBounded(ctx, f, f.Levels, f.MeshSize, [3]bool{true, true, true}); err != nil {
				t.Fatal("bounds-only finite singular/reflected Initial", err)
			}
		}
	}
	bad := NewScaleSkewVersor3D()
	bad.matrix[0][0] = math.Inf(1)
	if err := ffdInitialFinite(ctx, bad); !errors.Is(err, ErrFFD) {
		t.Fatal("nonfinite matrix", err)
	}
	badTranslation := NewTranslation3D()
	badTranslation.translation.X = math.NaN()
	if err := ffdInitialFinite(ctx, badTranslation); !errors.Is(err, ErrFFD) {
		t.Fatal("nonfinite offset", err)
	}
	clones := 0
	if _, err := ffdInitialCloneBytes(ctx, &boundedCloneCounter{NewAffine(), &clones}); !errors.Is(err, ErrFFD) || clones != 0 {
		t.Fatal("custom clone invoked", err, clones)
	}
}
func TestFFDBoundedIndependentJacobianAndZeroRegularization(t *testing.T) {
	fixed, moving, scene := ffdTestScene(t, [3]int{32, 28, 8}, 2.5, .75)
	initial := NewAffine()
	shift := render.Vec3{X: 2, Y: -1.5}
	if err := initial.SetTranslation(shift); err != nil {
		t.Fatal(err)
	}
	shifted, err := NewImage(moving.Size(), moving.Data(), moving.Origin().Add(shift), moving.Spacing(), moving.Direction())
	if err != nil {
		t.Fatal(err)
	}
	sourceFixed, sourceMoving := append([]float32(nil), fixed.Data()...), append([]float32(nil), moving.Data()...)
	sourceInitial := initial.Parameters()
	box := FFDBoundedOptions{LowerMM: [3]float64{-1, -1, -1}, UpperMM: [3]float64{1, 1, 1}, MaximumDisplacementGradient: .8, Optimizer: optimize.LBFGSB{MaximumIterations: 100, MaximumEvaluations: 1000, ValueTolerance: 1e-12}}
	original := box
	f := FreeFormDeformation{Fixed: fixed, Moving: shifted, Initial: initial, MeshSize: [3]int{2, 2, 1}, Levels: []FFDLevel{{Level: Level{Shrink: 2, SmoothingSigmaMM: 1}}, {Level: Level{Shrink: 1}, RefineMesh: true}}, Sampling: SampleRandom, Seed: 1344, BoundedOptimizer: &box, Workers: 2}
	f.Levels[0].SamplingPercentage = .75
	f.Levels[1].SamplingPercentage = .75
	identity, err := NewBSplineForImage(fixed, [3]int{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := mappingError(fixed, scene, identity, 2)
	result, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	after, largest := mappingError(fixed, scene, result.BSpline, 2)
	t.Logf("unregularized RMS before %.9g after %.9g max %.9g; mesh%v", before, after, largest, result.BSpline.MeshSize())
	for _, level := range result.Levels {
		t.Logf("level shrink%d iterations%d evals%d value%.9g stop%s", level.Level.Shrink, level.Iterations, level.Evaluations, level.Value, level.Stop)
	}
	if after > .75*before {
		t.Fatalf("RMS reduction <25%%: before%g after%g", before, after)
	}
	if result.JacobianBound == nil || !result.JacobianBound.BSpline.PositiveJacobianCertified || result.JacobianBound.TransformDeterminantLower <= 0 {
		t.Fatalf("missing certificate %+v", result.JacobianBound)
	}
	if result.BSpline.MeshSize() != [3]int{4, 4, 2} {
		t.Fatal("refinement", result.BSpline.MeshSize())
	}
	count := result.BSpline.count()
	for i, v := range result.BSpline.coefficients {
		if v < box.LowerMM[i/count] || v > box.UpperMM[i/count] {
			t.Fatal("parameter outside component box")
		}
	}
	minimum := math.Inf(1)
	for z := 0; z < fixed.size[2]; z++ {
		for y := 0; y < fixed.size[1]; y++ {
			for x := 0; x < fixed.size[0]; x++ {
				point := fixed.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
				index := result.BSpline.physicalToIndex.MulVec(point.Sub(result.BSpline.origin))
				_, want := boundedSplinePolynomial(result.BSpline, index)
				actual := result.Transform.JacobianWithRespectToPosition(point)
				determinant := boundedJacDet(want)
				minimum = math.Min(minimum, determinant)
				if math.Abs(boundedJacDet(actual)-determinant) > 1e-10 || determinant < result.JacobianBound.TransformDeterminantLower-1e-10 {
					t.Fatal("dense Jacobian/certificate mismatch")
				}
			}
		}
	}
	t.Logf("dense minimum det %.9g; declared lower %.9g q %.9g", minimum, result.JacobianBound.TransformDeterminantLower, result.JacobianBound.BSpline.DisplacementGradientNormUpper)
	if !reflect.DeepEqual(fixed.Data(), sourceFixed) || !reflect.DeepEqual(moving.Data(), sourceMoving) || !reflect.DeepEqual(initial.Parameters(), sourceInitial) || !reflect.DeepEqual(box, original) {
		t.Fatal("source/options changed")
	}
}

type boundedCloneCounter struct {
	*Affine
	clones *int
}

func (c *boundedCloneCounter) Clone() Transform { *c.clones++; return c.Affine.Clone() }
func TestFFDBoundedAdmissionBeforeGridAndInitialClone(t *testing.T) {
	f := boundedFFDForTests(t)
	f.Initial = NewAffine()
	evaluations := 0
	f.Progress = func(Progress) { evaluations++ }
	f.BoundedOptimizer.MaxControlPointBytes = 1
	if _, err := f.Run(context.Background()); !errors.Is(err, ErrFFDResourceLimit) || evaluations != 0 {
		t.Fatalf("control admission %v eval%d", err, evaluations)
	}
	f.BoundedOptimizer.MaxControlPointBytes = 0
	f.BoundedOptimizer.Optimizer.Limits.MaxParameters = 1
	if _, err := f.Run(context.Background()); !errors.Is(err, optimize.ErrLBFGSBResourceLimit) || evaluations != 0 {
		t.Fatalf("optimizer admission %v", err)
	}
	f.BoundedOptimizer.Optimizer.Limits.MaxParameters = 0
	f.BoundedOptimizer.LowerMM = [3]float64{-1.5, -1.5, -1.5}
	f.BoundedOptimizer.UpperMM = [3]float64{1.5, 1.5, 1.5}
	f.Levels = append(f.Levels, FFDLevel{Level: Level{Shrink: 1}, RefineMesh: true})
	if _, err := f.Run(context.Background()); !errors.Is(err, ErrFFD) || evaluations != 0 {
		t.Fatalf("unsafe finer box %v eval%d", err, evaluations)
	}
	f = boundedFFDForTests(t)
	f.BoundedOptimizer.MaxLevels = 1
	f.Levels = append(f.Levels, FFDLevel{Level: Level{Shrink: 1}})
	if _, err := f.Run(context.Background()); !errors.Is(err, ErrFFDResourceLimit) {
		t.Fatal("level limit", err)
	}
	f = boundedFFDForTests(t)
	f.MeshSize = [3]int{int(^uint(0) >> 1), 1, 1}
	if _, err := f.Run(context.Background()); !errors.Is(err, ErrFFDResourceLimit) {
		t.Fatal("overflow", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f = boundedFFDForTests(t)
	if _, err := f.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel admission", err)
	}
}
func TestFFDBoundedCertificateInitialAndThreshold(t *testing.T) {
	f := boundedFFDForTests(t)
	reflection := NewAffine()
	if err := reflection.SetMatrix(Matrix3{{-1, 0, 0}, {0, 1, 0}, {0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	f.Initial = reflection
	if _, err := f.Run(context.Background()); !errors.Is(err, ErrFFD) {
		t.Fatalf("reflected Initial %v", err)
	}
	b, err := NewBSplineForImage(f.Fixed, [3]int{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	f.Initial = b
	if _, err := f.Run(context.Background()); !errors.Is(err, ErrFFD) {
		t.Fatal("nonlinear guarantee", err)
	}
	f.Initial = nil
	q, err := ffdBoxGradientNorm(f.Fixed, f.MeshSize, f.BoundedOptimizer.LowerMM, f.BoundedOptimizer.UpperMM)
	if err != nil {
		t.Fatal(err)
	}
	f.BoundedOptimizer.MaximumDisplacementGradient = math.Nextafter(q, 0)
	if _, err := prepareFFDBounded(context.Background(), f, f.Levels, f.MeshSize, [3]bool{true, true, true}); !errors.Is(err, ErrFFD) {
		t.Fatal("threshold below outward q", err)
	}
	f.BoundedOptimizer.MaximumDisplacementGradient = q
	run, err := prepareFFDBounded(context.Background(), f, f.Levels, f.MeshSize, [3]bool{true, true, true})
	if err != nil {
		t.Fatal("threshold equals outward q", err)
	}
	affine := NewAffine()
	if err := affine.SetMatrix(Matrix3{{2, .3, 0}, {0, 3, .1}, {0, 0, 4}}); err != nil {
		t.Fatal(err)
	}
	transform, _ := NewComposite(b, affine)
	certificate, err := run.verify(context.Background(), b, transform)
	if err != nil || certificate.TransformDeterminantLower <= 0 || certificate.TransformDeterminantLower > 24 {
		t.Fatalf("affine composition %+v %v", certificate, err)
	}
}
func TestFFDBoundedOnlyAndCancellable(t *testing.T) {
	f := boundedFFDForTests(t)
	f.BoundedOptimizer.MaximumDisplacementGradient = 0
	f.BoundedOptimizer.LowerMM = [3]float64{}
	f.BoundedOptimizer.UpperMM = [3]float64{}
	initial, err := NewBSplineForImage(f.Fixed, [3]int{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	f.Initial = initial
	result, err := f.Run(context.Background())
	if err != nil || result.JacobianBound != nil {
		t.Fatalf("bounds-only nonlinear Initial %+v %v", result, err)
	}
	for _, v := range result.BSpline.Parameters() {
		if v != 0 {
			t.Fatal("fixed box moved")
		}
	}
	f = boundedFFDForTests(t)
	ctx, cancel := context.WithCancel(context.Background())
	evaluations := 0
	f.Progress = func(Progress) { evaluations++; cancel() }
	if _, err := f.Run(ctx); !errors.Is(err, context.Canceled) || evaluations != 1 {
		t.Fatalf("callback cancellation%v evaluations%d", err, evaluations)
	}
}
func TestFFDBoundedRefinementAndBudgetAccounting(t *testing.T) {
	f := boundedFFDForTests(t)
	f.MeshSize = [3]int{2, 2, 1}
	f.Levels = append(f.Levels, FFDLevel{Level: Level{Shrink: 1}, RefineMesh: true})
	f.BoundedOptimizer.MaximumDisplacementGradient = 0
	// N0=300, N1=735. C includes simultaneous refinement, bounds, start,
	// optimizer result and all retained per-level vectors; Level/header space
	// is measured independently of the planner formula below.
	ceiling := int64(8*(300+735+10*735) + 256*2 + 8192)
	f.BoundedOptimizer.MaxControlPointBytes = ceiling - 1
	if _, err := prepareFFDBounded(context.Background(), f, f.Levels, f.MeshSize, [3]bool{true, true, true}); !errors.Is(err, ErrFFDResourceLimit) {
		t.Fatal("one byte below control ceiling", err)
	}
	f.BoundedOptimizer.MaxControlPointBytes = ceiling
	run, err := prepareFFDBounded(context.Background(), f, f.Levels, f.MeshSize, [3]bool{true, true, true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBSplineForImage(f.Fixed, f.MeshSize)
	if err != nil {
		t.Fatal(err)
	}
	params := b.Parameters()
	for i := range params {
		params[i] = .1 * math.Sin(float64(i))
	}
	if err := b.SetParameters(params); err != nil {
		t.Fatal(err)
	}
	fine, err := b.Refine([3]bool{true, true, true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.verify(context.Background(), fine, fine); err != nil {
		t.Fatal(err)
	}
	for _, p := range []render.Vec3{{X: 2.1, Y: 2.2, Z: 1.4}, {X: 2.4, Y: 2.5, Z: 1.6}} {
		world := boundedGridPoint(b, p)
		if b.TransformPoint(world).Sub(fine.TransformPoint(world)).Length() > 1e-12 {
			t.Fatal("refinement altered mapping")
		}
	}
	live := int64(8)*(300+735+2*(735/3)+2*735+735+735+300+735) + int64(2*unsafe.Sizeof(FFDLevel{})+2*unsafe.Sizeof(LevelResult{})) + 4096
	if live > ceiling {
		t.Fatalf("independent control lifetime count %d >%d", live, ceiling)
	}
}

func TestFFDBoundedInitialAdmissionCapacityAndUnsupported(t *testing.T) {
	ctx := context.Background()
	f := boundedFFDForTests(t)
	f.BoundedOptimizer.MaximumDisplacementGradient = 0
	b, err := NewBSplineForImage(f.Fixed, [3]int{3, 2, 2})
	if err != nil {
		t.Fatal(err)
	}
	field := DisplacementField{Size: [3]int{3, 2, 2}, Spacing: render.Vec3{X: 1, Y: 1, Z: 1}, Direction: Identity3(), Vectors: make([]render.Vec3, 12)}
	dense, err := NewDisplacementFieldTransform(&field)
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := NewComposite(b, dense)
	// Shared children are copied separately by Composite.Clone.
	outer, _ := NewComposite(inner, inner, NewEuler3D(), NewSimilarity3D(), NewVersorRigid3D(), NewAffine())
	I, err := ffdInitialCloneBytes(ctx, outer)
	if err != nil {
		t.Fatal(err)
	}
	var capacity func(Transform) int64
	capacity = func(t Transform) int64 {
		switch v := t.(type) {
		case *BSpline:
			return int64(unsafe.Sizeof(*v)) + 8*int64(cap(v.coefficients))
		case *DisplacementFieldTransform:
			return int64(unsafe.Sizeof(*v)) + int64(unsafe.Sizeof(render.Vec3{}))*int64(cap(v.field.Vectors))
		case *Composite:
			n := int64(unsafe.Sizeof(*v)) + int64(unsafe.Sizeof(Transform(nil)))*int64(cap(v.transforms)) + int64(cap(v.optimize))
			for _, child := range v.transforms {
				n += capacity(child)
			}
			return n
		case *Affine:
			return int64(unsafe.Sizeof(*v))
		case *Euler3D:
			return int64(unsafe.Sizeof(*v))
		case *VersorRigid3D:
			return int64(unsafe.Sizeof(*v))
		case *Similarity3D:
			return int64(unsafe.Sizeof(*v))
		}
		panic("unexpected clone type")
	}
	first, second := outer.Clone(), outer.Clone()
	actual := capacity(first) + capacity(second)
	if actual > 2*I {
		t.Fatalf("two live clones capacities %d >reserve%d", actual, 2*I)
	}
	t.Logf("Initial two clones actual%d <=reserve%d", actual, 2*I)
	f.Initial = outer
	const baseC int64 = 8*(192+10*192) + 256 + 8192
	f.BoundedOptimizer.MaxControlPointBytes = baseC + 2*I - 1
	if _, err := prepareFFDBounded(ctx, f, f.Levels, f.MeshSize, [3]bool{true, true, true}); !errors.Is(err, ErrFFDResourceLimit) {
		t.Fatal("Initial one byte below", err)
	}
	f.BoundedOptimizer.MaxControlPointBytes++
	if _, err := prepareFFDBounded(ctx, f, f.Levels, f.MeshSize, [3]bool{true, true, true}); err != nil {
		t.Fatal("Initial exact reservation", err)
	}
	// Exercise the admitted nonlinear/composite Initial in the public adapter;
	// its coefficients/field are fixed while zero bounds fix the new spline.
	f.BoundedOptimizer.LowerMM, f.BoundedOptimizer.UpperMM = [3]float64{}, [3]float64{}
	result, err := f.Run(ctx)
	if err != nil || result.JacobianBound != nil {
		t.Fatalf("public builtin composite Initial %+v %v", result, err)
	}
	for _, v := range b.coefficients {
		if v != 0 {
			t.Fatal("source Initial spline changed")
		}
	}
	for _, v := range field.Vectors {
		if v != (render.Vec3{}) {
			t.Fatal("source Initial field changed")
		}
	}
	clones := 0
	f.Initial = &boundedCloneCounter{NewAffine(), &clones}
	f.BoundedOptimizer.MaxControlPointBytes = 0
	if _, err := f.Run(ctx); !errors.Is(err, ErrFFD) || clones != 0 {
		t.Fatalf("unaccountable custom Clone %v calls%d", err, clones)
	}
	var typedNil *BSpline
	f.Initial = typedNil
	if _, err := f.Run(ctx); !errors.Is(err, ErrFFD) {
		t.Fatal("typed nil", err)
	}
	cycle, _ := NewComposite(NewAffine())
	if err := cycle.Append(cycle); err != nil {
		t.Fatal(err)
	}
	f.Initial = cycle
	if _, err := f.Run(ctx); !errors.Is(err, ErrFFD) {
		t.Fatal("Composite cycle", err)
	}
	var depth Transform = NewAffine()
	for i := 0; i < 16; i++ {
		depth, _ = NewComposite(depth)
	}
	f.Initial = depth
	if _, err := f.Run(ctx); !errors.Is(err, ErrFFD) {
		t.Fatal("depth17", err)
	}
	many := &Composite{}
	for i := 0; i < 256; i++ {
		if err := many.Append(NewAffine()); err != nil {
			t.Fatal(err)
		}
	}
	f.Initial = many
	if _, err := f.Run(ctx); !errors.Is(err, ErrFFD) {
		t.Fatal("node257", err)
	}
	// Finite checks happen after byte admission and before cloning borrowed fields.
	field.Vectors[0].X = math.NaN()
	f.Initial = dense
	f.BoundedOptimizer.MaxControlPointBytes = 1
	if _, err := f.Run(ctx); !errors.Is(err, ErrFFDResourceLimit) {
		t.Fatal("budget precedes data scan", err)
	}
	f.BoundedOptimizer.MaxControlPointBytes = 0
	if _, err := f.Run(ctx); !errors.Is(err, ErrFFD) {
		t.Fatal("nonfinite dense Initial", err)
	}
}
func TestFFDBoundedLegacyNilUnchanged(t *testing.T) {
	// Float64-bit fixture captured from published f3400a41 with the same public
	// legacy options; probe source/full output are retained in the evidence.
	fixed, moving := ffdTestImages(t, [3]int{10, 10, 10}, 2, .4)
	f := FreeFormDeformation{Fixed: fixed, Moving: moving, MeshSize: [3]int{1, 1, 1}, Levels: []FFDLevel{{Level: Level{Shrink: 1}}}, Workers: 1, Optimizer: optimize.LBFGS{MaximumIterations: 2}}
	r, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	var buffer [8]byte
	put := func(v float64) { binary.LittleEndian.PutUint64(buffer[:], math.Float64bits(v)); h.Write(buffer[:]) }
	put(r.Value)
	for _, v := range r.BSpline.Parameters() {
		put(v)
	}
	for _, l := range r.Levels {
		put(l.Value)
		put(float64(l.Iterations))
		put(float64(l.Evaluations))
		h.Write([]byte(l.Stop))
		for _, v := range l.Parameters {
			put(v)
		}
	}
	want := "09db6705452495c4e84381ab2b90eae3792751bc8bca890a61ff432484e898bb"
	// Recompiled published f3400a41 on this qualified platform, before bounded mode.
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" && runtime.Version() == "go1.26.4" {
		want = "79da89420e000ad082b7de1d3b3c91384cc3068247b2328b2e266eb8ab002e6f"
	}
	if got := fmt.Sprintf("%x", h.Sum(nil)); got != want {
		t.Fatalf("legacy nil mode bits changed: %s", got)
	}
	if r.JacobianBound != nil {
		t.Fatal("legacy certificate populated")
	}
}

func TestFFDBoundedActualControlCapacities(t *testing.T) {
	f := boundedFFDForTests(t)
	f.MeshSize = [3]int{2, 2, 1}
	f.BoundedOptimizer.MaximumDisplacementGradient = 0
	coarse, err := NewBSplineForImage(f.Fixed, f.MeshSize)
	if err != nil {
		t.Fatal(err)
	}
	fine, err := coarse.Refine([3]bool{true, true, true})
	if err != nil {
		t.Fatal(err)
	}
	o, err := (&ffdBoundedRun{options: *f.BoundedOptimizer}).optimizer(context.Background(), fine, f.Fixed)
	if err != nil {
		t.Fatal(err)
	}
	// Last two axis subdivision buffers can overlap with old/new coefficients.
	current, size := coarse.coefficients[:coarse.count()], coarse.size
	var penultimate []float64
	for axis := 0; axis < 3; axis++ {
		penultimate = current
		current, size = refineAxis(current, size, axis)
	}
	composite, _ := NewComposite(fine, NewAffine())
	start := composite.Parameters()
	result := append([]float64(nil), start...)
	levels := make([]FFDLevel, 2)
	retained := make([]LevelResult, 2)
	retained[0].Parameters = make([]float64, coarse.NumberOfParameters())
	retained[1].Parameters = make([]float64, fine.NumberOfParameters())
	numeric := cap(coarse.coefficients) + cap(fine.coefficients) + cap(penultimate) + cap(current) + cap(o.LowerBounds) + cap(o.UpperBounds) + cap(start) + cap(result)
	for _, l := range retained {
		numeric += cap(l.Parameters)
	}
	// Composite.Parameters briefly overlaps its result with a BSpline.Parameters
	// copy. Reserve its actual rounded capacity even though it is released.
	temporary := fine.Parameters()
	numeric += cap(temporary)
	bytes := int64(8*numeric) + int64(cap(levels))*int64(unsafe.Sizeof(FFDLevel{})) + int64(cap(retained))*int64(unsafe.Sizeof(LevelResult{})) + 4096
	N0, N1 := coarse.NumberOfParameters(), fine.NumberOfParameters()
	ceiling := int64(8*(N0+N1+10*N1) + 256*2 + 8192)
	if bytes > ceiling {
		t.Fatalf("actual control capacities%d >ceiling%d", bytes, ceiling)
	}
	t.Logf("actual control backing/header count%d <=ceiling%d (N%d,%d)", bytes, ceiling, N0, N1)
}

func TestFFDBoundedFutureGridPrimitiveCap(t *testing.T) {
	f := boundedFFDForTests(t)
	f.MeshSize = [3]int{256, 256, 256}
	f.Levels = append(f.Levels, FFDLevel{Level: Level{Shrink: 1}, RefineMesh: true})
	f.BoundedOptimizer.MaximumDisplacementGradient = 0
	f.BoundedOptimizer.MaxControlPointBytes = 1 << 40
	f.BoundedOptimizer.Optimizer.Limits = optimize.LBFGSBLimits{MaxParameters: 1 << 30, MaxWorkspaceBytes: 1 << 40}
	calls := 0
	f.Progress = func(Progress) { calls++ }
	if _, err := f.Run(context.Background()); !errors.Is(err, ErrFFDResourceLimit) || calls != 0 {
		t.Fatalf("future primitive cap before grids %v calls%d", err, calls)
	}
}
