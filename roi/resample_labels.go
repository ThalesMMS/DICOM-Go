package roi

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/render"
)

// Label-map resampling onto any grid of the same Frame of Reference (#1114).
//
// Nearest reads the source voxel whose box holds each target voxel centre.
// LabelGaussian follows the behaviour of ITK's
// LabelImageGaussianInterpolateImageFunction (Apache-2.0; behaviour and test
// case ported, no code copied): every source voxel within CutoffSigmas*Sigma
// of the target point contributes the Gaussian mass of its voxel box (a
// product of error-function differences along the source axes, in mm) to its
// label, and the label with the largest summed weight wins, the background
// included. Unlike ITK, the Gaussian mode then keeps small labels from
// vanishing: a label present in the source under the target but absent from
// the result is written at the target voxel where its smoothed indicator is
// largest (see LabelResampleMode.DisablePreservation).

// LabelSource is a label image on a patient-space grid: label 0 is the
// background. LabelVolume, LabelStack and MaskStack implement it.
type LabelSource interface {
	LabelGrid() Grid
	LabelAt(x, y, z int) uint16
}

// labelBounded is implemented by sources that know the index box holding all
// their non-zero labels (ok is false when there is none), so resampling
// skips the rest of the target.
type labelBounded interface {
	LabelBounds() (box IndexBox, ok bool)
}

// singleLabeled is implemented by two-label sources.
type singleLabeled interface {
	singleLabel() uint16
}

// LabelVolume is a dense label volume, x fastest: Labels[(z*Rows+y)*Columns+x].
type LabelVolume struct {
	Grid   Grid
	Labels []uint16
}

// NewLabelVolume returns an all-zero volume on grid, of at most four times
// DefaultMaxResampleVoxels voxels.
func NewLabelVolume(grid Grid) (*LabelVolume, error) {
	if err := grid.Validate(); err != nil {
		return nil, err
	}
	if grid.VoxelCount() > DefaultMaxResampleVoxels*4 {
		return nil, fmt.Errorf("%w: %d voxels", ErrResampleTooLarge, grid.VoxelCount())
	}
	return &LabelVolume{Grid: grid, Labels: make([]uint16, grid.VoxelCount())}, nil
}

// LabelGrid returns the volume's grid.
func (v *LabelVolume) LabelGrid() Grid { return v.Grid }

// LabelAt returns the label of a voxel.
func (v *LabelVolume) LabelAt(x, y, z int) uint16 {
	return v.Labels[(z*v.Grid.Rows+y)*v.Grid.Columns+x]
}

// Set writes the label of a voxel.
func (v *LabelVolume) Set(x, y, z int, label uint16) {
	v.Labels[(z*v.Grid.Rows+y)*v.Grid.Columns+x] = label
}

// Count returns the number of voxels holding label.
func (v *LabelVolume) Count(label uint16) int {
	n := 0
	for _, value := range v.Labels {
		if value == label {
			n++
		}
	}
	return n
}

// SliceMask returns the pixels of slice z holding label.
func (v *LabelVolume) SliceMask(label uint16, z int) *RasterMask {
	columns, rows := v.Grid.Columns, v.Grid.Rows
	mask := NewRasterMask(columns, rows)
	for y := 0; y < rows; y++ {
		row := v.Labels[(z*rows+y)*columns : (z*rows+y+1)*columns]
		setLabelRuns(mask, y, 0, row, label)
	}
	return mask
}

// setLabelRuns sets the runs of row that hold label, starting at column x0.
func setLabelRuns(mask *RasterMask, y, x0 int, row []uint16, label uint16) {
	start := -1
	for i := 0; i <= len(row); i++ {
		on := i < len(row) && row[i] == label
		if on && start < 0 {
			start = i
		} else if !on && start >= 0 {
			mask.SetRun(y, x0+start, x0+i)
			start = -1
		}
	}
}

// LabelStack is a label image stored as one row-major Columns*Rows slice per
// grid slice (nil for an all-zero slice), such as the frames of a DICOM
// LABELMAP segmentation. The slices are shared, not copied, and must not
// change while the stack is in use.
type LabelStack struct {
	grid   Grid
	slices [][]uint16
	bounds IndexBox
	filled bool
}

// NewLabelStack wraps per-slice label arrays on grid.
func NewLabelStack(grid Grid, slices [][]uint16) (*LabelStack, error) {
	if err := grid.Validate(); err != nil {
		return nil, err
	}
	if len(slices) != grid.Slices {
		return nil, fmt.Errorf("%w: %d label slices for %d grid slices", ErrInvalidResample, len(slices), grid.Slices)
	}
	s := &LabelStack{grid: grid, slices: slices}
	b := newBoundsBuilder()
	for z, values := range slices {
		if values == nil {
			continue
		}
		if len(values) != grid.Columns*grid.Rows {
			return nil, fmt.Errorf("%w: label slice %d has %d values, want %d", ErrInvalidResample, z, len(values), grid.Columns*grid.Rows)
		}
		for i, value := range values {
			if value != 0 {
				b.add(i%grid.Columns, i/grid.Columns, z)
			}
		}
	}
	s.bounds, s.filled = b.box()
	return s, nil
}

func (s *LabelStack) LabelGrid() Grid { return s.grid }

func (s *LabelStack) LabelAt(x, y, z int) uint16 {
	values := s.slices[z]
	if values == nil {
		return 0
	}
	return values[y*s.grid.Columns+x]
}

// LabelBounds returns the index box of the non-zero labels.
func (s *LabelStack) LabelBounds() (IndexBox, bool) { return s.bounds, s.filled }

// MaskStack is a binary label image: one RasterMask per grid slice (nil when
// empty), whose set pixels hold Label. Overlapping segments are resampled as
// separate MaskStacks. The masks are shared and must not change while the
// stack is in use.
type MaskStack struct {
	grid   Grid
	masks  []*RasterMask
	label  uint16
	bounds IndexBox
	filled bool
}

// NewMaskStack wraps per-slice masks on grid; label 0 means 1.
func NewMaskStack(grid Grid, masks []*RasterMask, label uint16) (*MaskStack, error) {
	if err := grid.Validate(); err != nil {
		return nil, err
	}
	if len(masks) != grid.Slices {
		return nil, fmt.Errorf("%w: %d masks for %d grid slices", ErrInvalidResample, len(masks), grid.Slices)
	}
	if label == 0 {
		label = 1
	}
	s := &MaskStack{grid: grid, masks: masks, label: label}
	b := newBoundsBuilder()
	for z, mask := range masks {
		if mask == nil {
			continue
		}
		if mask.Columns != grid.Columns || mask.Rows != grid.Rows {
			return nil, fmt.Errorf("%w: mask %d is %dx%d, want %dx%d", ErrInvalidResample, z, mask.Columns, mask.Rows, grid.Columns, grid.Rows)
		}
		mask.ForEachRun(func(y int, run MaskRun) {
			b.add(run.Start, y, z)
			b.add(run.End-1, y, z)
		})
	}
	s.bounds, s.filled = b.box()
	return s, nil
}

func (s *MaskStack) LabelGrid() Grid { return s.grid }

func (s *MaskStack) LabelAt(x, y, z int) uint16 {
	if s.masks[z].Get(x, y) {
		return s.label
	}
	return 0
}

// LabelBounds returns the index box of the set pixels.
func (s *MaskStack) LabelBounds() (IndexBox, bool) { return s.bounds, s.filled }

func (s *MaskStack) singleLabel() uint16 { return s.label }

type boundsBuilder struct {
	lo, hi [3]int
	filled bool
}

func newBoundsBuilder() *boundsBuilder { return &boundsBuilder{} }

func (b *boundsBuilder) add(x, y, z int) {
	p := [3]int{x, y, z}
	if !b.filled {
		b.lo, b.hi, b.filled = p, p, true
		return
	}
	for i := range p {
		b.lo[i], b.hi[i] = min(b.lo[i], p[i]), max(b.hi[i], p[i])
	}
}

func (b *boundsBuilder) box() (IndexBox, bool) {
	if !b.filled {
		return IndexBox{}, false
	}
	return IndexBox{Min: b.lo, Max: [3]int{b.hi[0] + 1, b.hi[1] + 1, b.hi[2] + 1}}, true
}

// LabelInterpolator selects how labels are read at target points.
type LabelInterpolator int

const (
	// InterpolateNearest reads the source voxel holding the target point.
	InterpolateNearest LabelInterpolator = iota
	// InterpolateLabelGaussian takes the argmax of Gaussian-smoothed label
	// indicators.
	InterpolateLabelGaussian
)

// DefaultGaussianCutoffSigmas is the Gaussian support radius, in sigmas,
// when LabelResampleMode.CutoffSigmas is zero.
const DefaultGaussianCutoffSigmas = 3

// LabelResampleMode configures ResampleLabels. The zero value is nearest
// neighbour with the default voxel budget.
type LabelResampleMode struct {
	Interpolator LabelInterpolator
	// SigmaMM is the Gaussian standard deviation (mm, the same on every
	// axis) of InterpolateLabelGaussian.
	SigmaMM float64
	// CutoffSigmas is the Gaussian support radius in sigmas (ITK's alpha);
	// zero means DefaultGaussianCutoffSigmas.
	CutoffSigmas float64
	// DisablePreservation turns off the Gaussian mode's small-label
	// preservation, giving the plain argmax.
	DisablePreservation bool
	// MaxVoxels bounds the target voxels; zero means
	// DefaultMaxResampleVoxels.
	MaxVoxels int64
	// Mapper, when set, maps target points into the source's Frame of
	// Reference (a registration), so the two grids need not share one; each
	// target voxel centre is mapped on its own and voxels it cannot map stay
	// uncovered. The report then lists no SourceLabels, Preserved or
	// Vanished labels, which would need the inverse mapping.
	Mapper PointMapper
}

// NearestLabels returns the nearest-neighbour mode.
func NearestLabels() LabelResampleMode { return LabelResampleMode{} }

// LabelGaussian returns the Gaussian label mode with sigma in mm.
func LabelGaussian(sigmaMM float64) LabelResampleMode {
	return LabelResampleMode{Interpolator: InterpolateLabelGaussian, SigmaMM: sigmaMM}
}

// String names the mode for provenance.
func (m LabelResampleMode) String() string {
	name := "nearest"
	if m.Interpolator == InterpolateLabelGaussian {
		name = fmt.Sprintf("label Gaussian σ=%g mm", m.SigmaMM)
	}
	if m.Mapper != nil {
		name += " through a registration"
	}
	return name
}

func (m LabelResampleMode) normalized() (LabelResampleMode, error) {
	switch m.Interpolator {
	case InterpolateNearest:
	case InterpolateLabelGaussian:
		if !(m.SigmaMM > 0) || math.IsInf(m.SigmaMM, 0) {
			return m, fmt.Errorf("%w: Gaussian sigma %v mm must be positive and finite", ErrInvalidResample, m.SigmaMM)
		}
		if m.CutoffSigmas == 0 {
			m.CutoffSigmas = DefaultGaussianCutoffSigmas
		}
		if !(m.CutoffSigmas > 0) || math.IsInf(m.CutoffSigmas, 0) {
			return m, fmt.Errorf("%w: Gaussian cutoff %v sigmas must be positive and finite", ErrInvalidResample, m.CutoffSigmas)
		}
	default:
		return m, fmt.Errorf("%w: unknown interpolator %d", ErrInvalidResample, m.Interpolator)
	}
	if m.MaxVoxels == 0 {
		m.MaxVoxels = DefaultMaxResampleVoxels
	}
	if m.MaxVoxels < 0 {
		return m, fmt.Errorf("%w: negative voxel budget", ErrInvalidResample)
	}
	return m, nil
}

// LabelResampleReport describes a label resampling.
type LabelResampleReport struct {
	Coverage ResampleCoverage
	// Counts is the number of target voxels of each non-zero label.
	Counts map[uint16]int
	// SourceLabels lists the non-zero labels with at least one source voxel
	// centre inside the target grid's half-voxel extent.
	SourceLabels []uint16
	// Preserved lists the labels the Gaussian mode wrote back at their
	// strongest target voxel because the argmax had dropped them.
	Preserved []uint16
	// Vanished lists the SourceLabels absent from the result.
	Vanished []uint16
}

// LabelResampleResult is a dense resampled label volume.
type LabelResampleResult struct {
	LabelResampleReport
	Volume *LabelVolume
}

// LabelMaskResult is a resampled label image as per-label slice masks.
type LabelMaskResult struct {
	LabelResampleReport
	Grid Grid
	// Masks[label][z] holds the target pixels of slice z with that label (nil
	// when none); the background is not listed.
	Masks map[uint16][]*RasterMask
}

// ResampleLabels resamples src onto target, both in the same patient space
// unless mode.Mapper maps between them, into a dense label volume. Uncovered target voxels are 0; the coverage
// tells them from background.
func ResampleLabels(ctx context.Context, src LabelSource, target Grid, mode LabelResampleMode) (*LabelResampleResult, error) {
	mode, err := mode.normalized()
	if err != nil {
		return nil, err
	}
	if err := checkTargetBudget(target, mode.MaxVoxels); err != nil {
		return nil, err
	}
	volume, err := NewLabelVolume(target)
	if err != nil {
		return nil, err
	}
	report, err := resampleLabels(ctx, src, target, mode, &denseLabelSink{volume: volume})
	if err != nil {
		return nil, err
	}
	return &LabelResampleResult{LabelResampleReport: report, Volume: volume}, nil
}

// ResampleLabelMasks is ResampleLabels returning run-length masks per label
// and target slice instead of a dense volume, which keeps sparse labels on
// large targets small.
func ResampleLabelMasks(ctx context.Context, src LabelSource, target Grid, mode LabelResampleMode) (*LabelMaskResult, error) {
	mode, err := mode.normalized()
	if err != nil {
		return nil, err
	}
	if err := checkTargetBudget(target, mode.MaxVoxels); err != nil {
		return nil, err
	}
	sink := &maskLabelSink{grid: target, masks: map[uint16][]*RasterMask{}}
	report, err := resampleLabels(ctx, src, target, mode, sink)
	if err != nil {
		return nil, err
	}
	return &LabelMaskResult{LabelResampleReport: report, Grid: target, Masks: sink.masks}, nil
}

func checkTargetBudget(target Grid, budget int64) error {
	if err := target.Validate(); err != nil {
		return err
	}
	if budget > 0 && target.VoxelCount() > budget {
		return fmt.Errorf("%w: %d voxels > %d", ErrResampleTooLarge, target.VoxelCount(), budget)
	}
	return nil
}

// labelSink receives resampled rows; rows are distinct across concurrent
// calls.
type labelSink interface {
	row(y, z, x0 int, labels []uint16)
	relabel(x, y, z int, from, to uint16)
}

type denseLabelSink struct{ volume *LabelVolume }

func (s *denseLabelSink) row(y, z, x0 int, labels []uint16) {
	g := s.volume.Grid
	copy(s.volume.Labels[(z*g.Rows+y)*g.Columns+x0:], labels)
}

func (s *denseLabelSink) relabel(x, y, z int, _, to uint16) { s.volume.Set(x, y, z, to) }

type maskLabelSink struct {
	grid  Grid
	mu    sync.Mutex
	masks map[uint16][]*RasterMask
}

func (s *maskLabelSink) mask(label uint16, z int) *RasterMask {
	s.mu.Lock()
	defer s.mu.Unlock()
	slices := s.masks[label]
	if slices == nil {
		slices = make([]*RasterMask, s.grid.Slices)
		s.masks[label] = slices
	}
	if slices[z] == nil {
		slices[z] = NewRasterMask(s.grid.Columns, s.grid.Rows)
	}
	return slices[z]
}

func (s *maskLabelSink) row(y, z, x0 int, labels []uint16) {
	for i := 0; i < len(labels); {
		label := labels[i]
		j := i + 1
		for j < len(labels) && labels[j] == label {
			j++
		}
		if label != 0 {
			s.mask(label, z).SetRun(y, x0+i, x0+j)
		}
		i = j
	}
}

func (s *maskLabelSink) relabel(x, y, z int, from, to uint16) {
	if from != 0 {
		s.mask(from, z).Set(x, y, false)
	}
	if to != 0 {
		s.mask(to, z).Set(x, y, true)
	}
}

// labelCandidate is the target voxel where a label's smoothed indicator is
// largest.
type labelCandidate struct {
	fraction float64
	index    int64
	x, y, z  int
	current  uint16
}

func (c labelCandidate) better(o labelCandidate) bool {
	return c.fraction > o.fraction || c.fraction == o.fraction && c.index < o.index
}

type labelWorker struct {
	buffer     []uint16
	coords     [][3]float64
	inside     []bool
	counts     map[uint16]int
	candidates map[uint16]labelCandidate
	gauss      gaussianScratch
}

func resampleLabels(ctx context.Context, src LabelSource, target Grid, mode LabelResampleMode, sink labelSink) (LabelResampleReport, error) {
	if src == nil {
		return LabelResampleReport{}, fmt.Errorf("%w: nil source", ErrInvalidResample)
	}
	sourceGrid := src.LabelGrid()
	sm, err := newGridMap(sourceGrid)
	if err != nil {
		return LabelResampleReport{}, err
	}
	full := IndexBox{Max: [3]int{sourceGrid.Columns, sourceGrid.Rows, sourceGrid.Slices}}
	box, hasLabels := full, true
	if bounded, ok := src.(labelBounded); ok {
		box, hasLabels = bounded.LabelBounds()
	}
	gaussian := mode.Interpolator == InterpolateLabelGaussian
	var evalLo, evalHi [3]float64
	if hasLabels {
		evalLo, evalHi = sm.boxExtent(box)
		if gaussian {
			reach := mode.CutoffSigmas * mode.SigmaMM
			for i := range evalLo {
				evalLo[i], evalHi[i] = math.Max(sm.lo[i], evalLo[i]-reach), math.Min(sm.hi[i], evalHi[i]+reach)
			}
		}
	}
	var kernel gaussianKernel
	if gaussian {
		kernel = gaussianKernel{sigma: mode.SigmaMM, reach: mode.CutoffSigmas * mode.SigmaMM}
	}

	rows := newTargetRows(sm, target)
	coverage := newCoverage(target)
	var workersMu sync.Mutex
	var workers []*labelWorker
	err = parallelLines(ctx, target.Rows*target.Slices, func() func(int) {
		w := &labelWorker{
			buffer: make([]uint16, target.Columns), counts: map[uint16]int{},
			candidates: map[uint16]labelCandidate{},
		}
		if mode.Mapper != nil {
			w.coords, w.inside = make([][3]float64, target.Columns), make([]bool, target.Columns)
		}
		workersMu.Lock()
		workers = append(workers, w)
		workersMu.Unlock()
		labelAt := func(x, y, z int, u, v, s float64) uint16 {
			if u < evalLo[0] || u >= evalHi[0] || v < evalLo[1] || v >= evalHi[1] || s < evalLo[2] || s >= evalHi[2] {
				return 0
			}
			if !gaussian {
				return src.LabelAt(sm.axes[0].nearest(u), sm.axes[1].nearest(v), sm.axes[2].nearest(s))
			}
			label := kernel.evaluate(src, sm, box, [3]float64{u, v, s}, &w.gauss)
			index := (int64(z)*int64(target.Rows)+int64(y))*int64(target.Columns) + int64(x)
			for i, candidateLabel := range w.gauss.labels {
				if candidateLabel == 0 || w.gauss.total <= 0 {
					continue
				}
				c := labelCandidate{fraction: w.gauss.weights[i] / w.gauss.total, index: index, x: x, y: y, z: z, current: label}
				if old, ok := w.candidates[candidateLabel]; !ok || c.better(old) {
					w.candidates[candidateLabel] = c
				}
			}
			return label
		}
		return func(line int) {
			y, z := line%target.Rows, line/target.Rows
			var labels []uint16
			x0 := 0
			if mode.Mapper != nil {
				mapRow(sm, target, mode.Mapper, y, z, w.coords, w.inside)
				coverage.setMask(y, z, w.inside)
				if !hasLabels {
					return
				}
				labels = w.buffer[:target.Columns]
				for x := range labels {
					labels[x] = 0
					if w.inside[x] {
						labels[x] = labelAt(x, y, z, w.coords[x][0], w.coords[x][1], w.coords[x][2])
					}
				}
			} else {
				l := rows.line(y, z)
				cx0, cx1 := coveredRun(sm, l, target.Columns)
				coverage.setRow(y, z, cx0, cx1)
				if !hasLabels || cx1 <= cx0 {
					return
				}
				r0, r1 := l.columnRange(target.Columns, evalLo, evalHi)
				r0, r1 = max(r0, cx0), min(r1, cx1)
				if r1 <= r0 {
					return
				}
				labels, x0 = w.buffer[:r1-r0], r0
				for x := r0; x < r1; x++ {
					u, v, s := l.at(x)
					labels[x-r0] = labelAt(x, y, z, u, v, s)
				}
			}
			for i := 0; i < len(labels); {
				j := i + 1
				for j < len(labels) && labels[j] == labels[i] {
					j++
				}
				if labels[i] != 0 {
					w.counts[labels[i]] += j - i
				}
				i = j
			}
			sink.row(y, z, x0, labels)
		}
	})
	if err != nil {
		return LabelResampleReport{}, err
	}
	coverage.finish()

	report := LabelResampleReport{Coverage: coverage.ResampleCoverage, Counts: map[uint16]int{}}
	candidates := map[uint16]labelCandidate{}
	for _, w := range workers {
		for label, n := range w.counts {
			report.Counts[label] += n
		}
		for label, c := range w.candidates {
			if old, ok := candidates[label]; !ok || c.better(old) {
				candidates[label] = c
			}
		}
	}
	if hasLabels && mode.Mapper == nil {
		report.SourceLabels, err = sourceLabelsInside(ctx, src, sm, box, target)
		if err != nil {
			return LabelResampleReport{}, err
		}
	}
	if gaussian && !mode.DisablePreservation {
		taken := map[int64]bool{}
		for _, label := range report.SourceLabels {
			if report.Counts[label] > 0 {
				continue
			}
			c, ok := candidates[label]
			if !ok || c.fraction <= 0 || taken[c.index] {
				continue
			}
			// Never erase another label's last voxel.
			if c.current != 0 && report.Counts[c.current] <= 1 {
				continue
			}
			sink.relabel(c.x, c.y, c.z, c.current, label)
			if c.current != 0 {
				report.Counts[c.current]--
			}
			report.Counts[label] = 1
			taken[c.index] = true
			report.Preserved = append(report.Preserved, label)
		}
	}
	for label, n := range report.Counts {
		if n == 0 {
			delete(report.Counts, label)
		}
	}
	for _, label := range report.SourceLabels {
		if report.Counts[label] == 0 {
			report.Vanished = append(report.Vanished, label)
		}
	}
	return report, ctx.Err()
}

// sourceLabelsInside returns the sorted non-zero labels of src (within box)
// whose voxel centres lie inside the target grid's half-voxel extent.
func sourceLabelsInside(ctx context.Context, src LabelSource, sm *gridMap, box IndexBox, target Grid) ([]uint16, error) {
	tm, err := newGridMap(target)
	if err != nil {
		return nil, err
	}
	sourceGrid := sm.grid
	lines := newTargetRows(tm, sourceGrid)
	single, isSingle := src.(singleLabeled)
	var found atomic.Bool
	var mu sync.Mutex
	set := map[uint16]bool{}
	boxRows, boxSlices := box.Max[1]-box.Min[1], box.Max[2]-box.Min[2]
	err = parallelLines(ctx, boxRows*boxSlices, func() func(int) {
		local := map[uint16]bool{}
		return func(line int) {
			if isSingle && found.Load() {
				return
			}
			y, z := box.Min[1]+line%boxRows, box.Min[2]+line/boxRows
			l := lines.line(y, z)
			x0, x1 := l.columnRange(sourceGrid.Columns, tm.lo, tm.hi)
			x0, x1 = max(x0, box.Min[0]), min(x1, box.Max[0])
			for x := x0; x < x1; x++ {
				if !tm.inside(l.at(x)) {
					continue
				}
				if label := src.LabelAt(x, y, z); label != 0 && !local[label] {
					local[label] = true
					mu.Lock()
					set[label] = true
					mu.Unlock()
					if isSingle && label == single.singleLabel() {
						found.Store(true)
						return
					}
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	out := make([]uint16, 0, len(set))
	for label := range set {
		out = append(out, label)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// gaussianKernel evaluates ITK-style label Gaussian interpolation in mm.
type gaussianKernel struct {
	sigma, reach float64
}

type gaussianScratch struct {
	axisWeights [3][]float64
	labels      []uint16
	weights     []float64
	total       float64
}

// axisWeights fills the Gaussian mass of each voxel box of axis a within
// reach of coordinate c, returning the first index.
func (k gaussianKernel) axisWeights(a gridAxis, c float64, out []float64) (int, []float64) {
	i0 := render.NearestIndex(a.continuous(c - k.reach))
	i1 := render.NearestIndex(a.continuous(c + k.reach))
	i0, i1 = max(i0, 0), min(i1, a.n-1)
	out = out[:0]
	scale := 1 / (math.Sqrt2 * k.sigma)
	for i := i0; i <= i1; i++ {
		lo, hi := a.edges(i)
		out = append(out, 0.5*(math.Erf((hi-c)*scale)-math.Erf((lo-c)*scale)))
	}
	return i0, out
}

// evaluate returns the argmax label at source coordinates p and leaves the
// per-label weights in scratch. Voxels outside box hold the background.
// The running maximum, updated when a label's cumulative weight strictly
// exceeds it in x-fastest order, reproduces ITK's tie-breaking.
func (k gaussianKernel) evaluate(src LabelSource, sm *gridMap, box IndexBox, p [3]float64, scratch *gaussianScratch) uint16 {
	var start [3]int
	for i := range start {
		start[i], scratch.axisWeights[i] = k.axisWeights(sm.axes[i], p[i], scratch.axisWeights[i])
	}
	scratch.labels, scratch.weights, scratch.total = scratch.labels[:0], scratch.weights[:0], 0
	var best uint16
	bestWeight := 0.0
	wx, wy, wz := scratch.axisWeights[0], scratch.axisWeights[1], scratch.axisWeights[2]
	for dz, weightZ := range wz {
		z := start[2] + dz
		for dy, weightY := range wy {
			y := start[1] + dy
			weightZY := weightZ * weightY
			for dx, weightX := range wx {
				x := start[0] + dx
				w := weightZY * weightX
				label := uint16(0)
				if box.contains(x, y, z) {
					label = src.LabelAt(x, y, z)
				}
				scratch.total += w
				slot := -1
				for i, l := range scratch.labels {
					if l == label {
						slot = i
						break
					}
				}
				if slot < 0 {
					scratch.labels = append(scratch.labels, label)
					scratch.weights = append(scratch.weights, 0)
					slot = len(scratch.labels) - 1
				}
				scratch.weights[slot] += w
				if scratch.weights[slot] > bestWeight {
					bestWeight, best = scratch.weights[slot], label
				}
			}
		}
	}
	return best
}
