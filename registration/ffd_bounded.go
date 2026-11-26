package registration

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/optimize"
	"github.com/ThalesMMS/dicom-go/render"
)

// ErrFFDResourceLimit reports admission of bounded FFD's owned control buffers.
var ErrFFDResourceLimit = errors.New("registration: bounded FFD resource limit")

// FFDBoundedOptions selects genuine L-BFGS-B. Lower/UpperMM bound absolute world
// x/y/z coefficient displacements, not iterative step sizes. Zero must be in
// each finite interval. Optimizer bound slices must be nil; the adapter expands
// the component box anew for each resolution. The legacy Optimizer is unused.
// MaximumDisplacementGradient zero requests bounds only. A value in (0,1)
// additionally admits a conservative sufficient positive-Jacobian condition for
// every box/resolution, before allocating grids. It is limited to the spline's
// full-support region and a supported orientation-preserving matrix Initial.
// Initial admits only built-in Affine/Euler3D/VersorRigid3D/Similarity3D,
// Translation3D/ScaleVersor3D/ScaleSkewVersor3D,
// BSpline, DisplacementFieldTransform and Composite of these (depth <=16,
// <=256 node occurrences, no cycles); custom transforms are rejected before
// Clone. MaxControlPointBytes includes two simultaneously live Initial clones.
type FFDBoundedOptions struct {
	LowerMM, UpperMM            [3]float64
	Optimizer                   optimize.LBFGSB
	MaximumDisplacementGradient float64
	MaxLevels                   int   // zero: 16
	MaxControlPointBytes        int64 // zero: 256 MiB, excludes input/smoothing/metric buffers
}

// FFDJacobianBound describes a sufficient positive-Jacobian condition in the
// BSpline bound's domain, including the final fixed matrix composition.
type FFDJacobianBound struct {
	BSpline                   BSplineJacobianBound
	InitialDeterminantLower   float64
	TransformDeterminantLower float64
}

type ffdBoundedRun struct{ options FFDBoundedOptions }

func prepareFFDBounded(ctx context.Context, f FreeFormDeformation, levels []FFDLevel, mesh [3]int, refine [3]bool) (*ffdBoundedRun, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: bounded FFD nil context", ErrFFD)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o := *f.BoundedOptimizer
	if o.MaxLevels < 0 || o.MaxControlPointBytes < 0 || o.Optimizer.LowerBounds != nil || o.Optimizer.UpperBounds != nil || math.IsNaN(o.MaximumDisplacementGradient) || math.IsInf(o.MaximumDisplacementGradient, 0) || o.MaximumDisplacementGradient < 0 || o.MaximumDisplacementGradient >= 1 || math.IsNaN(f.Regularization) || math.IsInf(f.Regularization, 0) {
		return nil, fmt.Errorf("%w: bounded FFD options", ErrFFD)
	}
	maximumLevels := o.MaxLevels
	if maximumLevels == 0 {
		maximumLevels = 16
	}
	if len(levels) > maximumLevels {
		return nil, ErrFFDResourceLimit
	}
	for d := 0; d < 3; d++ {
		lo, hi := o.LowerMM[d], o.UpperMM[d]
		if math.IsNaN(lo) || math.IsNaN(hi) || math.IsInf(lo, 0) || math.IsInf(hi, 0) || lo > 0 || hi < 0 || lo > hi {
			return nil, fmt.Errorf("%w: bounded FFD component box", ErrFFD)
		}
	}
	initialBytes, err := ffdInitialCloneBytes(ctx, f.Initial)
	if err != nil {
		return nil, err
	}
	initialLower := 1.0
	if o.MaximumDisplacementGradient > 0 {
		var err error
		initialLower, err = ffdInitialDeterminantLower(f.Initial)
		if err != nil {
			return nil, err
		}
	}
	maximumBytes := o.MaxControlPointBytes
	if maximumBytes == 0 {
		maximumBytes = 256 << 20
	}
	var total, largest int64
	optimizer := o.Optimizer
	for index, level := range levels {
		if !finite(level.SmoothingSigmaMM) {
			return nil, fmt.Errorf("%w: non-finite bounded FFD smoothing", ErrFFD)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if index > 0 && level.RefineMesh {
			for a := 0; a < 3; a++ {
				if refine[a] {
					if mesh[a] > int(^uint(0)>>1)/2 {
						return nil, ErrFFDResourceLimit
					}
					mesh[a] *= 2
				}
			}
		}
		n := int64(3)
		for _, v := range mesh {
			if v < 1 || v > int(^uint(0)>>1)-3 {
				return nil, ErrFFDResourceLimit
			}
			size := int64(v + 3)
			if n > math.MaxInt64/size {
				return nil, ErrFFDResourceLimit
			}
			n *= size
		}
		if n > int64(int(^uint(0)>>1)) {
			return nil, ErrFFDResourceLimit
		}
		// NewBSpline has a fixed 2^26 control-point ceiling independently of
		// configurable optimizer/control byte caps; admit every future level.
		if n > 3*(1<<26) {
			return nil, ErrFFDResourceLimit
		}
		if _, err := optimizer.WorkspaceBytes(int(n)); err != nil {
			return nil, err
		}
		if total > math.MaxInt64-n {
			return nil, ErrFFDResourceLimit
		}
		total += n
		largest = max(largest, n)
		if o.MaximumDisplacementGradient > 0 {
			q, err := ffdBoxGradientNorm(f.Fixed, mesh, o.LowerMM, o.UpperMM)
			if err != nil {
				return nil, err
			}
			if !(q <= o.MaximumDisplacementGradient) || jacobianMultiplyLower(initialLower, jacobianDeterminantLowerFromNorm(q)) == 0 {
				return nil, fmt.Errorf("%w: component box cannot certify level %d", ErrFFD, index)
			}
		}
	}
	if initialBytes > (math.MaxInt64-8192)/2 {
		return nil, ErrFFDResourceLimit
	}
	overhead := int64(8192) + 2*initialBytes
	if int64(len(levels)) > (math.MaxInt64-overhead)/256 {
		return nil, ErrFFDResourceLimit
	}
	overhead += 256 * int64(len(levels))
	if largest > (math.MaxInt64-overhead)/80 || total > (math.MaxInt64-overhead-80*largest)/8 {
		return nil, ErrFFDResourceLimit
	}
	bytes := 8*total + 80*largest + overhead
	if bytes > maximumBytes {
		return nil, ErrFFDResourceLimit
	}
	if err := ffdInitialFinite(ctx, f.Initial); err != nil {
		return nil, err
	}
	return &ffdBoundedRun{options: o}, nil
}

// Same geometry primitive and arithmetic as NewBSplineForImage/Domain, without
// allocating coefficients. This avoids assuming orthogonal image directions.
func ffdBoxGradientNorm(im *Image, mesh [3]int, lo, hi [3]float64) (float64, error) {
	origin := im.Point(render.Vec3{X: -.5 - bsplineDomainMargin, Y: -.5 - bsplineDomainMargin, Z: -.5 - bsplineDomainMargin})
	sp := render.Vec3{X: (float64(im.size[0]) + 2*bsplineDomainMargin) * im.spacing.X / float64(mesh[0]), Y: (float64(im.size[1]) + 2*bsplineDomainMargin) * im.spacing.Y / float64(mesh[1]), Z: (float64(im.size[2]) + 2*bsplineDomainMargin) * im.spacing.Z / float64(mesh[2])}
	gridOrigin := origin.Add(im.direction.MulVec(sp.Scale(-1)))
	geometry, err := newImageGeometry([3]int{1, 1, 1}, gridOrigin, sp, im.direction)
	if err != nil {
		return 0, err
	}
	var delta [3][3]float64
	for d := 0; d < 3; d++ {
		for a := 0; a < 3; a++ {
			delta[d][a] = jacobianAbsDifferenceUpper(hi[d], lo[d])
		}
	}
	return jacobianGradientNormUpper(delta, geometry.physicalToIndex), nil
}
func (r *ffdBoundedRun) optimizer(ctx context.Context, b *BSpline, virtual *Image) (optimize.LBFGSB, error) {
	o := r.options.Optimizer
	n := b.NumberOfParameters()
	if _, err := o.WorkspaceBytes(n); err != nil {
		return o, err
	}
	if err := ctx.Err(); err != nil {
		return o, err
	}
	o.LowerBounds = make([]float64, n)
	o.UpperBounds = make([]float64, n)
	count := n / 3
	for d := 0; d < 3; d++ {
		for i := d * count; i < (d+1)*count; i++ {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return o, err
				}
			}
			o.LowerBounds[i] = r.options.LowerMM[d]
			o.UpperBounds[i] = r.options.UpperMM[d]
		}
	}
	if o.ProjectedGradientTolerance == 0 {
		o.ProjectedGradientTolerance = 1e-9
	}
	if o.InitialStepLength == 0 {
		o.InitialStepLength = MaximumStepSize(virtual)
	}
	return o, nil
}
func (r *ffdBoundedRun) verify(ctx context.Context, b *BSpline, t Transform) (*FFDJacobianBound, error) {
	count := b.count()
	for i, v := range b.coefficients {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		d := i / count
		if v < r.options.LowerMM[d] || v > r.options.UpperMM[d] || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("%w: refined/accepted coefficient outside box", ErrFFD)
		}
	}
	if r.options.MaximumDisplacementGradient == 0 {
		return nil, nil
	}
	bound, err := b.JacobianBound(ctx)
	if err != nil {
		return nil, err
	}
	if !bound.PositiveJacobianCertified || bound.DisplacementGradientNormUpper > r.options.MaximumDisplacementGradient {
		return nil, fmt.Errorf("%w: uncertified accepted spline", ErrFFD)
	}
	var initial Transform
	if c, ok := t.(*Composite); ok {
		if c.Len() != 2 {
			return nil, fmt.Errorf("%w: bounded composite shape", ErrFFD)
		}
		initial = c.Transform(1)
	}
	lower, err := ffdInitialDeterminantLower(initial)
	if err != nil {
		return nil, err
	}
	combined := jacobianMultiplyLower(lower, bound.DeterminantLower)
	if !(combined > 0) || math.IsInf(combined, 0) {
		return nil, fmt.Errorf("%w: unrepresentable composition bound", ErrFFD)
	}
	return &FFDJacobianBound{BSpline: bound, InitialDeterminantLower: lower, TransformDeterminantLower: combined}, nil
}
func ffdInitialDeterminantLower(t Transform) (float64, error) {
	if t == nil {
		return 1, nil
	}
	var m MatrixTransform
	switch v := t.(type) {
	case *Affine:
		if v != nil {
			m = v
		}
	case *Euler3D:
		if v != nil {
			m = v
		}
	case *VersorRigid3D:
		if v != nil {
			m = v
		}
	case *Similarity3D:
		if v != nil {
			m = v
		}
	case *Translation3D:
		if v != nil {
			m = v
		}
	case *ScaleVersor3D:
		if v != nil {
			m = v
		}
	case *ScaleSkewVersor3D:
		if v != nil {
			m = v
		}
	}
	if m == nil {
		return 0, fmt.Errorf("%w: guaranteed bounded FFD requires matrix Initial", ErrFFD)
	}
	if !finiteVec(m.Offset()) {
		return 0, fmt.Errorf("%w: non-finite Initial offset", ErrFFD)
	}
	lower := jacobianMatrixDeterminantLower(m.Matrix())
	if !(lower > 0) || math.IsInf(lower, 0) {
		return 0, fmt.Errorf("%w: Initial orientation is not certified positive", ErrFFD)
	}
	return lower, nil
}

// Clone of a caller's custom Transform has no finite allocation contract. Admit
// concrete built-ins by their stored buffers, without invoking caller methods.
// Composite aliases are counted once for each occurrence because Clone copies
// each occurrence. The fixed path array catches cycles with no heap-sized graph.
type ffdInitialAdmission struct {
	ctx       context.Context
	nodes     int
	bytes     int64
	ancestors [16]*Composite
}

func ffdInitialCloneBytes(ctx context.Context, t Transform) (int64, error) {
	if t == nil {
		return 0, nil
	}
	a := ffdInitialAdmission{ctx: ctx}
	if err := a.count(t, 1); err != nil {
		return 0, err
	}
	return a.bytes, nil
}
func (a *ffdInitialAdmission) add(n int64) error {
	if n < 0 || a.bytes > math.MaxInt64-n {
		return ErrFFDResourceLimit
	}
	a.bytes += n
	return nil
}
func (a *ffdInitialAdmission) count(t Transform, depth int) error {
	if err := a.ctx.Err(); err != nil {
		return err
	}
	if depth > len(a.ancestors) || a.nodes == 256 {
		return fmt.Errorf("%w: Initial depth/node limit", ErrFFD)
	}
	a.nodes++
	if err := a.add(4096); err != nil {
		return err
	}
	numeric := 0
	switch v := t.(type) {
	case *Affine:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
	case *Euler3D:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
	case *VersorRigid3D:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
	case *Similarity3D:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
	case *Translation3D:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
	case *ScaleVersor3D:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
	case *ScaleSkewVersor3D:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
	case *BSpline:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
		numeric = len(v.coefficients)
	case *DisplacementFieldTransform:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
		if len(v.field.Vectors) > int(^uint(0)>>1)/3 {
			return ErrFFDResourceLimit
		}
		numeric = 3 * len(v.field.Vectors)
	case *Composite:
		if v == nil {
			return fmt.Errorf("%w: nil Initial", ErrFFD)
		}
		for i := 0; i < depth-1; i++ {
			if a.ancestors[i] == v {
				return fmt.Errorf("%w: cyclic Initial", ErrFFD)
			}
		}
		a.ancestors[depth-1] = v
		if len(v.transforms) != len(v.optimize) || len(v.transforms) > 256-a.nodes {
			return fmt.Errorf("%w: Initial composite size", ErrFFD)
		}
		if err := a.add(32 * int64(len(v.transforms))); err != nil {
			return err
		}
		for _, child := range v.transforms {
			if err := a.count(child, depth+1); err != nil {
				return err
			}
		}
		a.ancestors[depth-1] = nil
	default:
		return fmt.Errorf("%w: unaccountable Initial type %T", ErrFFD, t)
	}
	if int64(numeric) > math.MaxInt64/16 {
		return ErrFFDResourceLimit
	}
	return a.add(16 * int64(numeric))
}
func ffdInitialFinite(ctx context.Context, t Transform) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	invalid := false
	switch v := t.(type) {
	case nil:
		return nil
	case *BSpline:
		for i, c := range v.coefficients {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if !finite(c) {
				invalid = true
				break
			}
		}
	case *DisplacementFieldTransform:
		for i, c := range v.field.Vectors {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if !finiteVec(c) {
				invalid = true
				break
			}
		}
	case *Composite:
		for _, child := range v.transforms {
			if err := ffdInitialFinite(ctx, child); err != nil {
				return err
			}
		}
	default:
		m := t.(MatrixTransform) // count admitted only concrete built-ins above
		invalid = !m.Matrix().Finite() || !finiteVec(m.Offset())
	}
	if invalid {
		return fmt.Errorf("%w: non-finite Initial", ErrFFD)
	}
	return nil
}
