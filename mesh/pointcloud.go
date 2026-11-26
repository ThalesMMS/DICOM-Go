package mesh

import (
	"context"
	"fmt"
	"math"
	"unsafe"
)

// PointCloud is an unconnected set of DICOM patient LPS points. Units must be
// "mm"; points have the same float32 coordinates as Mesh.Vertices. No triangle,
// normal orientation, closure or topology is inferred. Optional attributes have
// one value per point. Protected is a caller-declared preservation mask for rare
// structures; density alone cannot identify anatomical noise.
//
// Calls borrow the input for their duration. The caller must keep it stable.
type PointCloud struct {
	Units     string
	Points    [][3]float32
	Normals   [][3]float32
	Colors    [][4]uint8
	Scalars   []VertexScalarAttribute
	Protected []bool
}

// PointCloudOptions selects an opt-in statistical proposal followed by a
// greedy minimum-distance subset. Neighbors=0 disables statistical analysis.
// Otherwise, the score is the mean distance to Neighbors other points and the
// candidate threshold is global mean + SigmaFactor * population deviation.
// Candidates are removed only when RemoveOutliers is explicitly true, and
// protected points are never removed. A sparse point is a candidate, not a
// diagnosis of noise. MinDistanceMM=0 disables reduction. Accepted separations
// are at least MinDistanceMM-DistanceToleranceMM, evaluated in float64 on the
// original coordinates. The seed controls a specified SplitMix64/Fisher-Yates
// order; results are deterministic for the same input/options.
//
// Zero resource limits select finite defaults. MaxBytes reserves worst-case
// array payload including output, index, maps and scratch before allocation;
// caller input, Go allocator/runtime overhead and call stack are excluded.
// MaxWork bounds validation, selection comparisons, index visits and copies.
type PointCloudOptions struct {
	Neighbors                          int
	SigmaFactor                        float64
	RemoveOutliers                     bool
	MinDistanceMM, DistanceToleranceMM float64
	Seed                               uint64
	MaxPoints, MaxBytes, MaxWork       int64
	MaxNeighbors                       int
}

// PointRemovalReason describes the applied action, independently of a
// statistical candidate flag. Zero means that the point was kept.
type PointRemovalReason uint8

const (
	PointKept PointRemovalReason = iota
	PointStatisticalOutlier
	PointDistanceSuppressed
)

type PointCloudReport struct {
	Complete                                         bool
	State                                            string
	Options                                          PointCloudOptions
	InputPoints, OutputPoints, Candidates, Protected int
	OutliersRemoved, DistanceRemoved                 int
	MeanDistanceMM, StandardDeviationMM, ThresholdMM float64
	ReservedBytes, Work                              int64
}

// PointCloudResult has a detached cloud in source order, a map of each output
// point to its source index and the inverse (-1 for removed points). Normals,
// RGBA colors, named finite scalar values/units and protection flags are copied
// exactly. Candidate flags/scores describe the original cloud. On any error,
// Cloud and all arrays are nil and Report.Complete is false.
type PointCloudResult struct {
	Cloud            *PointCloud
	OriginalIndices  []uint32
	InputToOutput    []int64
	Removed          []PointRemovalReason
	OutlierCandidate []bool
	LocalMeanMM      []float64
	Report           PointCloudReport
}

// PointCloudLimitError is a typed resource failure, never a partial subset.
type PointCloudLimitError struct {
	Resource    string
	Used, Limit int64
}

func (e *PointCloudLimitError) Error() string {
	return fmt.Sprintf("mesh point cloud: %s limit (%d > %d)", e.Resource, e.Used, e.Limit)
}
func (e *PointCloudLimitError) Unwrap() error { return ErrBudgetExceeded }

type pointCloudRun struct {
	ctx context.Context
	r   *PointCloudReport
}

func (r *pointCloudRun) step() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if r.r.Work >= r.r.Options.MaxWork {
		return &PointCloudLimitError{"work", r.r.Work + 1, r.r.Options.MaxWork}
	}
	r.r.Work++
	return nil
}

func pointCloudOptions(o PointCloudOptions) (PointCloudOptions, error) {
	for _, f := range []float64{o.SigmaFactor, o.MinDistanceMM, o.DistanceToleranceMM} {
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return o, fmt.Errorf("%w: invalid point cloud distance/factor", ErrInvalidField)
		}
	}
	if o.Neighbors < 0 || o.MaxNeighbors < 0 || (o.RemoveOutliers && o.Neighbors == 0) ||
		(o.DistanceToleranceMM > 0 && o.DistanceToleranceMM >= o.MinDistanceMM) {
		return o, fmt.Errorf("%w: invalid point cloud neighborhood/tolerance", ErrInvalidField)
	}
	if o.MaxNeighbors == 0 {
		o.MaxNeighbors = 64
	}
	if o.Neighbors > o.MaxNeighbors {
		return o, &PointCloudLimitError{"neighbors", int64(o.Neighbors), int64(o.MaxNeighbors)}
	}
	limits := []*int64{&o.MaxPoints, &o.MaxBytes, &o.MaxWork}
	defaults := []int64{1_000_000, 256 << 20, 100_000_000}
	for i, p := range limits {
		if *p < 0 {
			return o, fmt.Errorf("%w: negative point cloud limit", ErrInvalidField)
		}
		if *p == 0 {
			*p = defaults[i]
		}
	}
	return o, nil
}

// ProcessPointCloud performs bounded statistical cleanup and distance reduction.
// Protection has priority. Two protected points closer than the permitted
// separation cause an error instead of silently losing a point or violating
// the separation contract. Analysis without RemoveOutliers is nondestructive.
func ProcessPointCloud(ctx context.Context, c *PointCloud, opts PointCloudOptions) (result *PointCloudResult, err error) {
	o, err := pointCloudOptions(opts)
	result = &PointCloudResult{Report: PointCloudReport{Options: o, State: "invalid"}}
	r := &result.Report
	defer func() {
		if err != nil {
			if ctx != nil && ctx.Err() != nil {
				r.State = "canceled"
			} else if _, ok := err.(*PointCloudLimitError); ok {
				r.State = "limited"
			}
			// A successful-looking subset must never escape a canceled/limited run.
			result = &PointCloudResult{Report: *r}
		}
	}()
	if err != nil {
		return result, err
	}
	if ctx == nil || c == nil || c.Units != "mm" {
		return result, fmt.Errorf("%w: point cloud requires context, input and mm units", ErrInvalidField)
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	n := len(c.Points)
	r.InputPoints = n
	if int64(n) > o.MaxPoints {
		return result, &PointCloudLimitError{"points", int64(n), o.MaxPoints}
	}
	if uint64(n) > math.MaxInt32 || (o.Neighbors > 0 && o.Neighbors >= n) {
		return result, fmt.Errorf("%w: point cloud index/neighborhood range", ErrInvalidField)
	}
	if (c.Normals != nil && len(c.Normals) != n) || (c.Colors != nil && len(c.Colors) != n) ||
		(c.Protected != nil && len(c.Protected) != n) || len(c.Scalars) > MaxVertexScalarAttributes {
		return result, fmt.Errorf("%w: point cloud attribute count", ErrInvalidField)
	}
	// Index nodes+permutation, candidate/active/reasons/scores/maps/output IDs,
	// permutation for sampling and worst-case detached attribute payloads.
	perPoint := int64(unsafe.Sizeof(pointCloudNode{})) + 4 + 1 + 1 + 1 + 8 + 8 + 4 + 4 + 12
	if c.Normals != nil {
		perPoint += 12
	}
	if c.Colors != nil {
		perPoint += 4
	}
	if c.Protected != nil {
		perPoint++
	}
	perPoint += int64(len(c.Scalars)) * 8
	constant := int64(o.Neighbors)*int64(unsafe.Sizeof(pointCloudNeighbor{})) +
		int64(len(c.Scalars))*int64(unsafe.Sizeof(VertexScalarAttribute{}))
	if int64(n) > (math.MaxInt64-constant)/perPoint {
		return result, &PointCloudLimitError{"bytes", math.MaxInt64, o.MaxBytes}
	}
	r.ReservedBytes = int64(n)*perPoint + constant
	if r.ReservedBytes > o.MaxBytes {
		return result, &PointCloudLimitError{"bytes", r.ReservedBytes, o.MaxBytes}
	}
	run := pointCloudRun{ctx, r}
	for i, p := range c.Points {
		if err = run.step(); err != nil {
			return result, err
		}
		for _, f := range p {
			if !finite32(f) {
				return result, fmt.Errorf("%w: nonfinite cloud point %d", ErrInvalidField, i)
			}
		}
		if c.Normals != nil {
			for _, f := range c.Normals[i] {
				if !finite32(f) {
					return result, fmt.Errorf("%w: nonfinite cloud normal %d", ErrInvalidField, i)
				}
			}
		}
		if c.Protected != nil && c.Protected[i] {
			r.Protected++
		}
	}
	// Validate metadata separately, then bound/cancel every scalar value visit.
	for ai, a := range c.Scalars {
		if err = run.step(); err != nil {
			return result, err
		}
		// Reject large caller metadata before a shared validator could quote it
		// into an unbounded diagnostic. Length inspection is constant work.
		if len(a.Name) > 64 || len(a.Units) > 64 {
			return result, fmt.Errorf("%w: point cloud scalar metadata length", ErrInvalidField)
		}
		metadata := VertexScalarAttribute{Name: a.Name, Units: a.Units}
		if err = validateVertexScalars([]VertexScalarAttribute{metadata}, 0, false); err != nil {
			return result, err
		}
		for _, previous := range c.Scalars[:ai] {
			if previous.Name == a.Name {
				return result, fmt.Errorf("%w: duplicate point cloud scalar", ErrInvalidField)
			}
		}
		if len(a.Values) != n {
			return result, fmt.Errorf("%w: point cloud scalar count", ErrInvalidField)
		}
		for _, f := range a.Values {
			if err = run.step(); err != nil {
				return result, err
			}
			if !finiteCurvature(f) {
				return result, fmt.Errorf("%w: nonfinite point cloud scalar", ErrInvalidField)
			}
		}
	}
	var tree *pointCloudTree
	if o.Neighbors > 0 || o.MinDistanceMM > 0 {
		tree, err = buildPointCloudTree(c.Points, &run)
		if err != nil {
			return result, err
		}
	}
	result.OutlierCandidate = make([]bool, n)
	result.LocalMeanMM = make([]float64, n)
	result.Removed = make([]PointRemovalReason, n)
	active := make([]bool, n)
	if o.Neighbors > 0 {
		heap := make([]pointCloudNeighbor, 0, o.Neighbors)
		mean, m2 := 0.0, 0.0
		for i, p := range c.Points {
			heap, err = tree.nearest(pointCloud64(p), i, o.Neighbors, heap[:0], &run)
			if err != nil {
				return result, err
			}
			score := 0.0
			for _, v := range heap {
				if err = run.step(); err != nil {
					return result, err
				}
				score += math.Sqrt(v.distance2) / float64(o.Neighbors)
			}
			result.LocalMeanMM[i] = score
			delta := score - mean
			mean += delta / float64(i+1)
			m2 += delta * (score - mean)
		}
		r.MeanDistanceMM = mean
		r.StandardDeviationMM = math.Sqrt(math.Max(0, m2/float64(n)))
		r.ThresholdMM = mean + o.SigmaFactor*r.StandardDeviationMM
		if math.IsInf(r.ThresholdMM, 0) {
			return result, fmt.Errorf("%w: cloud threshold overflow", ErrInvalidField)
		}
		for i, score := range result.LocalMeanMM {
			if err = run.step(); err != nil {
				return result, err
			}
			if score > r.ThresholdMM {
				result.OutlierCandidate[i] = true
				r.Candidates++
				if o.RemoveOutliers && !cloudProtected(c, i) {
					result.Removed[i] = PointStatisticalOutlier
					r.OutliersRemoved++
				}
			}
		}
	}
	if o.MinDistanceMM > 0 {
		separation := o.MinDistanceMM - o.DistanceToleranceMM
		for i, p := range c.Points {
			if err = run.step(); err != nil {
				return result, err
			}
			if cloudProtected(c, i) {
				var close bool
				close, err = tree.within(pointCloud64(p), separation, active, &run)
				if err != nil {
					return result, err
				}
				if close {
					return result, fmt.Errorf("%w: protected cloud point %d conflicts with minimum distance", ErrInvalidField, i)
				}
				active[i] = true
			}
		}
		order := make([]uint32, n)
		for i := range order {
			if err = run.step(); err != nil {
				return result, err
			}
			order[i] = uint32(i)
		}
		seed := o.Seed
		for i := n - 1; i > 0; i-- {
			if err = run.step(); err != nil {
				return result, err
			}
			j := int(pointCloudRandom(&seed) % uint64(i+1))
			order[i], order[j] = order[j], order[i]
		}
		for _, id := range order {
			if err = run.step(); err != nil {
				return result, err
			}
			if cloudProtected(c, int(id)) || result.Removed[id] != PointKept {
				continue
			}
			var close bool
			close, err = tree.within(pointCloud64(c.Points[id]), separation, active, &run)
			if err != nil {
				return result, err
			}
			if close {
				result.Removed[id] = PointDistanceSuppressed
				r.DistanceRemoved++
			} else {
				active[id] = true
			}
		}
	}
	count := n - r.OutliersRemoved - r.DistanceRemoved
	out := &PointCloud{Units: "mm", Points: make([][3]float32, count)}
	if c.Normals != nil {
		out.Normals = make([][3]float32, count)
	}
	if c.Colors != nil {
		out.Colors = make([][4]uint8, count)
	}
	if c.Protected != nil {
		out.Protected = make([]bool, count)
	}
	out.Scalars = make([]VertexScalarAttribute, len(c.Scalars))
	for i, a := range c.Scalars {
		out.Scalars[i] = VertexScalarAttribute{Name: a.Name, Units: a.Units, Values: make([]float64, count)}
	}
	result.OriginalIndices = make([]uint32, count)
	result.InputToOutput = make([]int64, n)
	k := 0
	for i, reason := range result.Removed {
		if err = run.step(); err != nil {
			return result, err
		}
		result.InputToOutput[i] = -1
		if reason != PointKept {
			continue
		}
		result.InputToOutput[i] = int64(k)
		result.OriginalIndices[k] = uint32(i)
		out.Points[k] = c.Points[i]
		if out.Normals != nil {
			out.Normals[k] = c.Normals[i]
		}
		if out.Colors != nil {
			out.Colors[k] = c.Colors[i]
		}
		if out.Protected != nil {
			out.Protected[k] = c.Protected[i]
		}
		for j, a := range c.Scalars {
			if err = run.step(); err != nil {
				return result, err
			}
			out.Scalars[j].Values[k] = a.Values[i]
		}
		k++
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Cloud = out
	r.OutputPoints = count
	r.State, r.Complete = "complete", true
	return result, nil
}

func cloudProtected(c *PointCloud, i int) bool { return c.Protected != nil && c.Protected[i] }
func pointCloud64(p [3]float32) [3]float64 {
	return [3]float64{float64(p[0]), float64(p[1]), float64(p[2])}
}
func pointCloudRandom(seed *uint64) uint64 {
	*seed += 0x9e3779b97f4a7c15
	x := *seed
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}
