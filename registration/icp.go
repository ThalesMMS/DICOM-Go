package registration

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/render"
)

// ICPOptions configures IterativeClosestPoint.
type ICPOptions struct {
	// Mode is the transform family refitted at every iteration; rigid by
	// default.
	Mode LandmarkMode
	// MaxIterations bounds the iterations; 50 when zero.
	MaxIterations int
	// Tolerance is the absolute improvement threshold in mm for the mean
	// closest-point distance, measured before and after the returned refit.
	// A nonnegative improvement below it (including zero) stops successfully;
	// an increase stops with ICPStopWorsened instead. This is not a bound on
	// the residual distance. The default is 1e-4 when zero.
	Tolerance float64
	// MaxFixedPoints subsamples the fixed cloud evenly to at most this many
	// points per iteration; 20000 when zero.
	MaxFixedPoints int
	// Initial, when set, is the starting fixed-to-moving transform;
	// otherwise the centroids are matched first.
	Initial Transform
}

// ICPStopReason identifies the termination of an ICP run. Errors return no
// result; these reasons describe a computed final transform, not registration accuracy.
type ICPStopReason string

const (
	// ICPStopInitiallyAligned means the initial matrix transform already maps
	// every sampled point exactly onto a moving point. Input/fit degeneracy was
	// checked; no refit was applied. The result owns a clone of that initial
	// transform, admissible to Mode, and Iterations is zero.
	ICPStopInitiallyAligned ICPStopReason = "initially-aligned"
	// ICPStopTolerance means the last mean-distance improvement was nonnegative
	// and below Tolerance. It includes an objective plateau and does not prove
	// a small residual or a global/local geometric minimum.
	ICPStopTolerance ICPStopReason = "tolerance"
	// ICPStopWorsened means the returned refit increased the mean closest-point
	// distance. Landmark fitting minimizes squared error, so its mean absolute
	// distance need not decrease even when RMS does. The refit is not rolled back.
	ICPStopWorsened ICPStopReason = "worsened"
	// ICPStopMaximumIterations means the iteration budget was exhausted before
	// either of the above criteria held.
	ICPStopMaximumIterations ICPStopReason = "maximum-iterations"
)

// ICPResult is the outcome of IterativeClosestPoint.
type ICPResult struct {
	// Transform maps the fixed cloud onto the moving cloud.
	Transform MatrixTransform
	// MeanDistance and RMS are the closest-point distances (mm) of the
	// fixed points sampled by MaxFixedPoints, evaluated at Transform.
	MeanDistance, RMS float64
	Iterations        int
	// Converged is true for ICPStopInitiallyAligned or ICPStopTolerance. It describes convergence
	// of the monitored mean-distance objective, not registration accuracy.
	Converged  bool
	StopReason ICPStopReason
}

// IterativeClosestPoint registers two point clouds, such as the vertices of
// two meshes of the same surface: every iteration pairs each fixed point
// with the moving point closest to its current image and refits the
// transform to those pairs (Besl and McKay 1992), as vtkIterativeClosest
// PointTransform does. Results depend on the starting alignment and need not
// be a geometric optimum: stopping monitors mean absolute distance whereas
// landmark fitting minimizes squared error. The clouds should start roughly
// aligned (the centroids are matched first). See ICPStopReason.
func IterativeClosestPoint(ctx context.Context, fixed, moving []render.Vec3, opts ICPOptions) (ICPResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.MaxIterations == 0 {
		opts.MaxIterations = 50
	}
	if opts.Tolerance == 0 {
		opts.Tolerance = 1e-4
	}
	if opts.MaxFixedPoints == 0 {
		opts.MaxFixedPoints = 20000
	}
	if opts.MaxIterations < 0 || opts.Tolerance < 0 || math.IsNaN(opts.Tolerance) || math.IsInf(opts.Tolerance, 0) || opts.MaxFixedPoints < 4 {
		return ICPResult{}, fmt.Errorf("%w: invalid ICP options", ErrLandmarks)
	}
	if len(fixed) < 4 || len(moving) < 4 {
		return ICPResult{}, fmt.Errorf("%w: ICP needs at least 4 points per cloud", ErrLandmarks)
	}
	if err := ctx.Err(); err != nil {
		return ICPResult{}, err
	}
	for _, cloud := range [][]render.Vec3{fixed, moving} {
		for i, p := range cloud {
			if i%256 == 0 {
				if err := ctx.Err(); err != nil {
					return ICPResult{}, err
				}
			}
			if !finiteVec(p) {
				return ICPResult{}, fmt.Errorf("%w: point %d is not finite", ErrLandmarks, i+1)
			}
		}
	}
	sample := fixed
	if len(fixed) > opts.MaxFixedPoints {
		sample = make([]render.Vec3, opts.MaxFixedPoints)
		for i := range sample {
			sample[i] = fixed[i*len(fixed)/opts.MaxFixedPoints]
		}
	}
	tree := newKDTree(moving)
	var current Transform = opts.Initial
	if current == nil {
		shift := NewAffine()
		if err := shift.SetOffset(centroid(moving).Sub(centroid(fixed))); err != nil {
			return ICPResult{}, err
		}
		current = shift
	}
	targets := make([]render.Vec3, len(sample))
	previous, _, err := icpDistances(ctx, tree, sample, current, targets)
	if err != nil {
		return ICPResult{}, err
	}
	result := ICPResult{}
	for iteration := 0; iteration < opts.MaxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return ICPResult{}, err
		}
		fit, err := RegisterLandmarks(sample, targets, opts.Mode)
		if err != nil {
			return ICPResult{}, err
		}
		if iteration == 0 && previous == 0 {
			if clone, ok := icpCloneAlignedInitial(current, opts.Mode); ok {
				mean, rms, err := icpDistances(ctx, tree, sample, clone, nil)
				if err != nil {
					return ICPResult{}, err
				}
				if mean == 0 && rms == 0 {
					return ICPResult{Transform: clone, MeanDistance: mean, RMS: rms, Converged: true, StopReason: ICPStopInitiallyAligned}, nil
				}
			}
		}
		mean, rms, err := icpDistances(ctx, tree, sample, fit.Transform, targets)
		if err != nil {
			return ICPResult{}, err
		}
		result = ICPResult{Transform: fit.Transform, MeanDistance: mean, RMS: rms,
			Iterations: iteration + 1, StopReason: ICPStopMaximumIterations}
		improvement := previous - mean
		if improvement < 0 {
			result.StopReason = ICPStopWorsened
			break
		}
		if improvement < opts.Tolerance {
			result.StopReason = ICPStopTolerance
			result.Converged = true
			break
		}
		previous = mean
	}
	return result, nil
}

// An exact initial alignment can only be returned when representable by the
// requested family. This checks existing matrix contracts without rounding or
// replacing the coefficients of the caller's initial transform.
func icpCloneAlignedInitial(initial Transform, mode LandmarkMode) (MatrixTransform, bool) {
	matrix, ok := initial.(MatrixTransform)
	if !ok {
		return nil, false
	}
	m := matrix.Matrix()
	switch mode {
	case LandmarkRigid:
		if !m.Finite() || !m.orthogonal(DefaultOrthogonalityTolerance) || m.Determinant() <= 0 {
			return nil, false
		}
	case LandmarkSimilarity:
		if _, _, err := similarityFromMatrix(m, DefaultOrthogonalityTolerance); err != nil {
			return nil, false
		}
	case LandmarkAffine:
		if !m.Finite() {
			return nil, false
		}
	default:
		return nil, false
	}
	clone, ok := initial.Clone().(MatrixTransform)
	return clone, ok
}

// icpDistances evaluates the same deterministic sample at the transform it
// reports. The new closest pairs become the next refit's targets. Cancellation
// is checked in bounded chunks; RegisterLandmarks itself is not preempted.
func icpDistances(ctx context.Context, tree *kdTree, sample []render.Vec3, transform Transform, targets []render.Vec3) (float64, float64, error) {
	sum, sum2 := 0.0, 0.0
	for i, p := range sample {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
		}
		image := transform.TransformPoint(p)
		if !finiteVec(image) {
			return 0, 0, fmt.Errorf("%w: ICP mapped point %d is not finite", ErrLandmarks, i+1)
		}
		q, d2 := tree.nearest(image)
		if math.IsInf(d2, 0) || math.IsNaN(d2) {
			return 0, 0, fmt.Errorf("%w: ICP distance %d is not finite", ErrLandmarks, i+1)
		}
		if targets != nil {
			targets[i] = q
		}
		sum += math.Sqrt(d2)
		sum2 += d2
	}
	if math.IsInf(sum, 0) || math.IsInf(sum2, 0) {
		return 0, 0, fmt.Errorf("%w: ICP distance sum is not finite", ErrLandmarks)
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	return sum / float64(len(sample)), math.Sqrt(sum2 / float64(len(sample))), nil
}

// kdTree is a static 3-d tree for nearest-neighbour queries.
type kdTree struct {
	points []render.Vec3
	nodes  []kdNode
}

type kdNode struct {
	point       int
	axis        int
	left, right int // -1 when absent
}

func newKDTree(points []render.Vec3) *kdTree {
	t := &kdTree{points: points, nodes: make([]kdNode, 0, len(points))}
	indices := make([]int, len(points))
	for i := range indices {
		indices[i] = i
	}
	t.build(indices, 0)
	return t
}

func coordinate(p render.Vec3, axis int) float64 {
	switch axis {
	case 0:
		return p.X
	case 1:
		return p.Y
	default:
		return p.Z
	}
}

func (t *kdTree) build(indices []int, depth int) int {
	if len(indices) == 0 {
		return -1
	}
	axis := depth % 3
	sort.Slice(indices, func(a, b int) bool {
		return coordinate(t.points[indices[a]], axis) < coordinate(t.points[indices[b]], axis)
	})
	mid := len(indices) / 2
	node := len(t.nodes)
	t.nodes = append(t.nodes, kdNode{point: indices[mid], axis: axis})
	left := t.build(indices[:mid], depth+1)
	right := t.build(indices[mid+1:], depth+1)
	t.nodes[node].left, t.nodes[node].right = left, right
	return node
}

// nearest returns the closest stored point and its squared distance.
func (t *kdTree) nearest(q render.Vec3) (render.Vec3, float64) {
	best, bestD2 := -1, math.Inf(1)
	var search func(node int)
	search = func(node int) {
		if node < 0 {
			return
		}
		n := t.nodes[node]
		p := t.points[n.point]
		if d2 := p.Sub(q).Dot(p.Sub(q)); d2 < bestD2 {
			best, bestD2 = n.point, d2
		}
		diff := coordinate(q, n.axis) - coordinate(p, n.axis)
		near, far := n.left, n.right
		if diff > 0 {
			near, far = far, near
		}
		search(near)
		if diff*diff < bestD2 {
			search(far)
		}
	}
	search(0)
	if best < 0 {
		return render.Vec3{}, math.Inf(1)
	}
	return t.points[best], bestD2
}
