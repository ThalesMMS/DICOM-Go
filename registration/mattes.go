package registration

import (
	"fmt"
	"math"
	"sync"

	"github.com/ThalesMMS/dicom-go/render"
)

// MattesMutualInformation is the negated mutual information of the fixed
// and mapped moving intensities, for images of different modalities, after
// Mattes et al., "PET-CT image registration in the chest using free-form
// deformations" (IEEE TMI 22(1), 2003), with the conventions of ITK's
// MattesMutualInformationImageToImageMetricv4 so that its values agree:
//
//   - the intensity ranges are those of the whole images (or their masks;
//     of the sampled points for the fixed image) at construction;
//   - each range spans Bins−4 bins, with two bins of padding either side;
//   - a fixed intensity falls in one bin (a zero-order Parzen window), a
//     moving intensity spreads over four with a cubic B-spline window;
//   - the joint and marginal distributions are normalized by the joint
//     histogram's total, and V = −Σ p(i,j)·ln(p(i,j)/(p_f(i)·p_m(j))) over
//     the cells above machine epsilon.
//
// Fixed points whose mapped moving intensity falls outside the moving range
// (a moving mask can make that happen) do not count.
type MattesMutualInformation struct {
	evaluator                     *evaluator
	bins                          int
	movingMin, movingMax          float64
	fixedBinSize, movingBinSize   float64
	fixedNormalMin, movingNormMin float64
	// workspaceBytes overrides MattesWorkspaceBytes when positive (tests).
	workspaceBytes int64
}

// DefaultMattesBins is ITK's default number of histogram bins.
const DefaultMattesBins = 50

// NewMattesMutualInformation compares fixed and moving over domain with
// bins histogram bins per image (DefaultMattesBins when zero, at least 5).
// The joint histogram and each derivative array must individually fit in
// 256 MiB (see ErrMattesWorkspaceLimit); the arrays of one evaluation share
// the MattesWorkspaceBytes budget, which bounds its concurrency.
func NewMattesMutualInformation(fixed, moving *Image, domain MetricDomain, bins int) (*MattesMutualInformation, error) {
	e, err := newEvaluator(fixed, moving, domain)
	if err != nil {
		return nil, err
	}
	if bins == 0 {
		bins = DefaultMattesBins
	}
	if bins < 5 {
		return nil, fmt.Errorf("registration: %d histogram bins, want at least 5", bins)
	}
	if err := checkMattesArraySize(bins, bins); err != nil {
		return nil, err
	}
	fixedMin, fixedMax := fixedRange(fixed, domain)
	movingMin, movingMax := imageRange(moving, domain.MovingMask)
	if !(fixedMax > fixedMin) || !(movingMax > movingMin) {
		return nil, fmt.Errorf("%w: constant intensities; mutual information is undefined", ErrImage)
	}
	m := &MattesMutualInformation{evaluator: e, bins: bins, movingMin: movingMin, movingMax: movingMax}
	m.fixedBinSize = (fixedMax - fixedMin) / float64(bins-4)
	m.movingBinSize = (movingMax - movingMin) / float64(bins-4)
	m.fixedNormalMin = fixedMin/m.fixedBinSize - 2
	m.movingNormMin = movingMin/m.movingBinSize - 2
	return m, nil
}

// imageRange is the intensity range of the samples, or of those whose
// centre is inside mask.
func imageRange(im *Image, mask *Image) (float64, float64) {
	low, high := math.Inf(1), math.Inf(-1)
	for i, v := range im.data {
		if mask != nil {
			x, y, z := i%im.size[0], i/im.size[0]%im.size[1], i/(im.size[0]*im.size[1])
			if !mask.insideMask(im.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})) {
				continue
			}
		}
		low, high = math.Min(low, float64(v)), math.Max(high, float64(v))
	}
	return low, high
}

// fixedRange is the fixed intensity range of the domain: the samples
// nearest the sampled points, else the fixed image (within its mask).
func fixedRange(fixed *Image, domain MetricDomain) (float64, float64) {
	if domain.Points == nil {
		return imageRange(fixed, domain.FixedMask)
	}
	low, high := math.Inf(1), math.Inf(-1)
	for _, point := range domain.Points {
		index := fixed.ContinuousIndex(point)
		x, y, z := int(math.Round(index.X)), int(math.Round(index.Y)), int(math.Round(index.Z))
		if x < 0 || y < 0 || z < 0 || x >= fixed.size[0] || y >= fixed.size[1] || z >= fixed.size[2] {
			continue
		}
		v := float64(fixed.At(x, y, z))
		low, high = math.Min(low, v), math.Max(high, v)
	}
	return low, high
}

// Value is V for transform.
func (m *MattesMutualInformation) Value(transform Transform) (float64, error) {
	value, _, err := m.evaluate(transform, false)
	return value, err
}

// ValueAndDerivative is V and its gradient with respect to the transform
// parameters, from the derivative of the B-spline window: with
// t = m/Δm − normalized minimum the moving bin position, g = ∇m(T(x)) ·
// ∂T/∂p(x) and S the histogram total,
//
//	∂V/∂p = 1/(Δm·S) Σ_(i,j) ln(p(i,j)/p_m(j)) Σ_(x in row i) β3′(j − t)·g
func (m *MattesMutualInformation) ValueAndDerivative(transform Transform) (float64, []float64, error) {
	return m.evaluate(transform, true)
}

type mattesPartial struct {
	count      int
	joint      []float64 // bins×bins, [fixed bin][moving bin]
	fixed      []float64 // fixed marginal counts
	derivative []float64 // bins×bins×parameters
}

func newMattesPartial(bins, parameters int) *mattesPartial {
	return &mattesPartial{
		joint:      make([]float64, bins*bins),
		fixed:      make([]float64, bins),
		derivative: make([]float64, bins*bins*parameters),
	}
}

func (m *MattesMutualInformation) evaluate(transform Transform, derivatives bool) (float64, []float64, error) {
	if derivatives {
		if local, ok := transform.(localSupportTransform); ok && local.localSupport() {
			return m.evaluateLocal(transform, local)
		}
	}
	bins := m.bins
	parameters := 0
	if derivatives {
		parameters = transform.NumberOfParameters()
		if err := checkMattesArraySize(bins*bins, parameters); err != nil {
			return 0, nil, err
		}
	}
	total := newMattesPartial(bins, parameters)
	// Each evaluation owns its scratch. Return a chunk's histogram to the
	// free list only after the ordered merge has consumed it; the chunks in
	// flight, and so the partials allocated, are bounded by the budget.
	floats := bins*bins + bins + bins*bins*parameters
	inFlight := mattesChunksInFlight(m.workspaceBudget(), floats, floats)
	var free []*mattesPartial
	var scratchMu sync.Mutex
	reduceMetricChunks(m.evaluator.pointCount(), m.evaluator.domain.Workers, inFlight, func() func(int, int) *mattesPartial {
		var jacobian Jacobian
		projected := make([]float64, parameters)
		return func(start, end int) *mattesPartial {
			var partial *mattesPartial
			scratchMu.Lock()
			if last := len(free) - 1; last >= 0 {
				partial, free = free[last], free[:last]
			}
			scratchMu.Unlock()
			if partial == nil {
				partial = newMattesPartial(bins, parameters)
			}
			for i := start; i < end; i++ {
				s, ok := m.evaluator.sampleAt(i, transform, derivatives)
				if !ok || s.moving < m.movingMin || s.moving > m.movingMax {
					continue
				}
				partial.count++
				movingTerm := s.moving/m.movingBinSize - m.movingNormMin
				movingBin := clampBin(movingTerm, bins)
				fixedBin := clampBin(s.fixed/m.fixedBinSize-m.fixedNormalMin, bins)
				partial.fixed[fixedBin]++
				if derivatives {
					transform.JacobianWithRespectToParameters(s.fixedPoint, &jacobian)
					for p := range projected {
						projected[p] = s.gradient.X*jacobian.At(0, p) + s.gradient.Y*jacobian.At(1, p) + s.gradient.Z*jacobian.At(2, p)
					}
				}
				for j := movingBin - 1; j <= movingBin+2; j++ {
					argument := float64(j) - movingTerm
					cell := fixedBin*bins + j
					partial.joint[cell] += cubicBSpline(argument)
					if derivatives {
						slope := cubicBSplineDerivative(argument)
						row := partial.derivative[cell*parameters : (cell+1)*parameters]
						for p, g := range projected {
							row[p] += slope * g
						}
					}
				}
			}
			return partial
		}
	}, func(partial *mattesPartial) {
		total.count += partial.count
		for i, v := range partial.joint {
			total.joint[i] += v
		}
		for i, v := range partial.fixed {
			total.fixed[i] += v
		}
		for i, v := range partial.derivative {
			total.derivative[i] += v
		}
		partial.count = 0
		clear(partial.joint)
		clear(partial.fixed)
		clear(partial.derivative)
		scratchMu.Lock()
		free = append(free, partial)
		scratchMu.Unlock()
	})
	if total.count == 0 {
		return 0, nil, ErrTooFewSamples
	}
	var sum float64
	for _, v := range total.joint {
		sum += v
	}
	if sum < epsilon {
		return 0, nil, ErrTooFewSamples
	}
	movingMarginal := make([]float64, bins)
	for i := 0; i < bins; i++ {
		for j := 0; j < bins; j++ {
			movingMarginal[j] += total.joint[i*bins+j] / sum
		}
	}
	var information float64
	var derivative []float64
	if derivatives {
		derivative = make([]float64, parameters)
	}
	for i := 0; i < bins; i++ {
		fixedProbability := total.fixed[i] / sum
		if fixedProbability <= epsilon {
			continue
		}
		logFixed := math.Log(fixedProbability)
		for j := 0; j < bins; j++ {
			joint := total.joint[i*bins+j] / sum
			if movingMarginal[j] <= epsilon || joint <= epsilon {
				continue
			}
			ratio := math.Log(joint / movingMarginal[j])
			information += joint * (ratio - logFixed)
			if derivatives {
				row := total.derivative[(i*bins+j)*parameters : (i*bins+j+1)*parameters]
				for p, d := range row {
					derivative[p] += ratio * d
				}
			}
		}
	}
	for p := range derivative {
		derivative[p] /= m.movingBinSize * sum
	}
	return -information, derivative, nil
}

// workspaceBudget is the byte budget of one evaluation's accumulators.
func (m *MattesMutualInformation) workspaceBudget() int64 {
	if m.workspaceBytes > 0 {
		return m.workspaceBytes
	}
	return MattesWorkspaceBytes
}

// epsilon is the double-precision machine epsilon, ITK's threshold for
// empty histogram cells.
const epsilon = 2.220446049250313e-16

// clampBin truncates a bin position to the bins that leave room for the
// window, [2, bins−3].
func clampBin(term float64, bins int) int {
	bin := int(term)
	if bin < 2 {
		return 2
	}
	if bin > bins-3 {
		return bins - 3
	}
	return bin
}

// cubicBSpline is the cubic B-spline kernel β3.
func cubicBSpline(u float64) float64 {
	a := math.Abs(u)
	switch {
	case a < 1:
		return (4 - 6*a*a + 3*a*a*a) / 6
	case a < 2:
		return (8 - 12*a + 6*a*a - a*a*a) / 6
	}
	return 0
}

// cubicBSplineDerivative is β3′.
func cubicBSplineDerivative(u float64) float64 {
	switch {
	case u >= 0 && u < 1:
		return -2*u + 1.5*u*u
	case u > -1 && u < 0:
		return -2*u - 1.5*u*u
	case u >= 1 && u < 2:
		return -2 + 2*u - 0.5*u*u
	case u > -2 && u <= -1:
		return 2 + 2*u + 0.5*u*u
	}
	return 0
}
