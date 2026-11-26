package dsa

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
)

// Typed errors returned by Module.Plan. Test them with errors.Is.
var (
	// ErrNoMaskModule reports a nil Module or one without items.
	ErrNoMaskModule = errors.New("dsa: no Mask Subtraction Sequence items")
	// ErrInvalidNumberOfFrames reports a Number of Frames below 1.
	ErrInvalidNumberOfFrames = errors.New("dsa: invalid number of frames")
	// ErrFrameOutOfRange reports a requested contrast frame outside the
	// object's frames.
	ErrFrameOutOfRange = errors.New("dsa: frame number out of range")
	// ErrFrameNotApplicable reports a frame that no Mask Subtraction
	// Sequence item covers: it is displayed without subtraction.
	ErrFrameNotApplicable = errors.New("dsa: no mask subtraction item applies to the frame")
	// ErrItemOutOfRange reports an invalid item index for Module.PlanItem.
	ErrItemOutOfRange = errors.New("dsa: mask subtraction item index out of range")
	// ErrUnsupportedMaskOperation reports a Mask Operation other than
	// AVG_SUB, TID or REV_TID. Recovery treats the item as AVG_SUB when it
	// has Mask Frame Numbers.
	ErrUnsupportedMaskOperation = errors.New("dsa: unsupported mask operation")
	// ErrInvalidFrameRange reports an Applicable Frame Range with an odd
	// number of values, a start after its end, or frames outside the
	// object. Recovery drops the unpaired value, swaps reversed pairs and
	// clips pairs to the object's frames.
	ErrInvalidFrameRange = errors.New("dsa: invalid applicable frame range")
	// ErrMissingApplicableFrameRange reports a REV_TID item without an
	// Applicable Frame Range (Type 1C). Recovery uses every frame.
	ErrMissingApplicableFrameRange = errors.New("dsa: REV_TID item has no applicable frame range")
	// ErrMissingMaskFrameNumbers reports an AVG_SUB item without Mask Frame
	// Numbers (Type 1C). There is no recovery.
	ErrMissingMaskFrameNumbers = errors.New("dsa: AVG_SUB item has no mask frame numbers")
	// ErrMaskFrameOutOfRange reports a mask frame outside the object's
	// frames. Recovery drops such AVG_SUB mask frames (failing if none
	// remain) and clamps TID/REV_TID mask frames into the object.
	ErrMaskFrameOutOfRange = errors.New("dsa: mask frame number out of range")
	// ErrMissingTIDOffset reports a TID or REV_TID item without TID Offset
	// (Type 2C). Recovery uses the standard's default of 1.
	ErrMissingTIDOffset = errors.New("dsa: TID item has no TID offset")
	// ErrInvalidContrastFrameAveraging reports a Contrast Frame Averaging of
	// 0. Recovery averages no frames (uses 1).
	ErrInvalidContrastFrameAveraging = errors.New("dsa: invalid contrast frame averaging")
	// ErrContrastAveragingOverrun reports Contrast Frame Averaging frames
	// past the last frame. Recovery averages the frames that exist.
	ErrContrastAveragingOverrun = errors.New("dsa: contrast frame averaging runs past the last frame")
	// ErrInvalidPixelShift reports a Mask Sub-pixel Shift that is not two
	// finite numbers. Recovery uses no shift.
	ErrInvalidPixelShift = errors.New("dsa: invalid mask sub-pixel shift")
	// ErrMissingSubtractionItemID reports a Frame Pixel Shift functional
	// group that cannot be linked to the item because the item has no
	// Subtraction Item ID. Recovery uses the only functional group shift
	// when there is exactly one, otherwise the module's shift.
	ErrMissingSubtractionItemID = errors.New("dsa: mask subtraction item has no subtraction item ID")
	// ErrFrameSizeMismatch reports pixel buffers whose sizes disagree.
	ErrFrameSizeMismatch = errors.New("dsa: frame size mismatch")
)

// ShiftSource tells where a Plan's Shift came from.
type ShiftSource string

const (
	// ShiftNone means no shift is recorded; the zero shift applies.
	ShiftNone ShiftSource = ""
	// ShiftFromModule is the item's Mask Sub-pixel Shift (0028,6114).
	ShiftFromModule ShiftSource = "mask-module"
	// ShiftFromSharedFunctionalGroup is the Frame Pixel Shift functional
	// group of the Shared Functional Groups Sequence.
	ShiftFromSharedFunctionalGroup ShiftSource = "shared-functional-group"
	// ShiftFromPerFrameFunctionalGroup is the Frame Pixel Shift functional
	// group of the contrast frame's Per-Frame Functional Groups item.
	ShiftFromPerFrameFunctionalGroup ShiftSource = "per-frame-functional-group"
)

// PlanOptions controls Module.Plan.
type PlanOptions struct {
	// NumberOfFrames overrides Module.NumberOfFrames when positive.
	NumberOfFrames int
	// Recover opts into the recoveries documented on each typed error.
	// Every recovery applied is reported in Plan.Recoveries; without it the
	// first inconsistency is returned as an error.
	Recover bool
}

// Recovery reports one inconsistency Plan worked around under
// PlanOptions.Recover.
type Recovery struct {
	// Err is the error strict planning returns; it wraps a typed error.
	Err error
	// Action describes what the planner did instead.
	Action string
}

// Plan is the resolved subtraction of one contrast frame. Frame numbers are
// 1-based.
type Plan struct {
	// ContrastFrame is the requested frame.
	ContrastFrame int
	// Item is the index of the Mask Subtraction Sequence item used.
	Item int
	// Operation is the item's Mask Operation.
	Operation string
	// SubtractionItemID is the item's Subtraction Item ID, if any.
	SubtractionItemID    int
	HasSubtractionItemID bool
	// MaskFrames are the frames averaged, in modality values, to form the
	// mask.
	MaskFrames []int
	// ContrastFrames are the frames averaged to form the contrast image: the
	// contrast frame and, under Contrast Frame Averaging, the frames after
	// it.
	ContrastFrames []int
	// Shift is applied to the averaged mask before subtraction.
	Shift       Shift
	ShiftSource ShiftSource
	// Recoveries lists the inconsistencies worked around (PlanOptions.
	// Recover only).
	Recoveries []Recovery
}

// Plan resolves the 1-based contrast frame against the first Mask
// Subtraction Sequence item that applies to it (PS3.3 C.7.6.10.1.1):
//
//   - AVG_SUB: the mask averages Mask Frame Numbers. Without an Applicable
//     Frame Range the item covers frames 1 to NumberOfFrames - Contrast
//     Frame Averaging + 1.
//   - TID: the mask of frame f is frame f - TID Offset. Without a range the
//     item covers the frames whose mask (and averaging window) exists.
//   - REV_TID: the offset starts at TID Offset on the first frame of the
//     Applicable Frame Range (required) and decreases by one per frame, so
//     the k-th frame (0-based) has mask f - (TID Offset - k).
//
// Contrast Frame Averaging averages frames f to f + n - 1. Under TID and
// REV_TID the mask is averaged over the same window from its own first
// frame, as a time-interval difference of frame-averaged images (the
// composition of ITK's FrameAverageVideoFilter and
// FrameDifferenceVideoFilter). The shift is the contrast frame's Frame Pixel
// Shift functional group entry with the item's Subtraction Item ID, which
// prevails over the module's Mask Sub-pixel Shift.
func (m *Module) Plan(frame int, opts PlanOptions) (Plan, error) {
	if m == nil || len(m.Items) == 0 {
		return Plan{}, ErrNoMaskModule
	}
	n, err := m.frameCount(opts)
	if err != nil {
		return Plan{}, err
	}
	if frame < 1 || frame > n {
		return Plan{}, fmt.Errorf("%w: frame %d, object has %d frames", ErrFrameOutOfRange, frame, n)
	}
	for index, item := range m.Items {
		p, err := newPlanner(m, index, n, opts.Recover)
		if err != nil {
			if !item.AppliesToFrame(frame) {
				// An inconsistent item whose explicit range does not cover
				// the frame cannot affect it.
				continue
			}
			return Plan{}, err
		}
		if rangesContain(p.ranges, frame) {
			return p.plan(frame)
		}
	}
	return Plan{}, fmt.Errorf("%w: frame %d", ErrFrameNotApplicable, frame)
}

// PlanItem is Plan restricted to Mask Subtraction Sequence item itemIndex,
// for objects whose items cover the same frame with different operations.
func (m *Module) PlanItem(itemIndex, frame int, opts PlanOptions) (Plan, error) {
	if m == nil || len(m.Items) == 0 {
		return Plan{}, ErrNoMaskModule
	}
	if itemIndex < 0 || itemIndex >= len(m.Items) {
		return Plan{}, fmt.Errorf("%w: %d of %d", ErrItemOutOfRange, itemIndex, len(m.Items))
	}
	n, err := m.frameCount(opts)
	if err != nil {
		return Plan{}, err
	}
	if frame < 1 || frame > n {
		return Plan{}, fmt.Errorf("%w: frame %d, object has %d frames", ErrFrameOutOfRange, frame, n)
	}
	p, err := newPlanner(m, itemIndex, n, opts.Recover)
	if err != nil {
		return Plan{}, err
	}
	if !rangesContain(p.ranges, frame) {
		return Plan{}, fmt.Errorf("%w: frame %d, item %d", ErrFrameNotApplicable, frame, itemIndex)
	}
	return p.plan(frame)
}

func (m *Module) frameCount(opts PlanOptions) (int, error) {
	n := m.NumberOfFrames
	if opts.NumberOfFrames > 0 {
		n = opts.NumberOfFrames
	}
	if n < 1 {
		return 0, fmt.Errorf("%w: %d", ErrInvalidNumberOfFrames, n)
	}
	return n, nil
}

type frameRange struct{ start, end int }

type planner struct {
	module  *Module
	item    MaskSubtractionItem
	index   int
	frames  int
	recover bool
	// Resolved once per item by newPlanner.
	op         string
	averaging  int
	offset     int
	ranges     []frameRange
	recoveries []Recovery
}

func newPlanner(m *Module, index, frames int, allowRecovery bool) (*planner, error) {
	p := &planner{module: m, item: m.Items[index], index: index, frames: frames, recover: allowRecovery}
	var err error
	if p.op, err = p.operation(); err != nil {
		return nil, err
	}
	if p.averaging, err = p.contrastAveraging(); err != nil {
		return nil, err
	}
	if p.op == MaskOperationTID || p.op == MaskOperationReversedTID {
		if p.offset, err = p.tidOffset(); err != nil {
			return nil, err
		}
	}
	if p.ranges, err = p.applicableRanges(); err != nil {
		return nil, err
	}
	return p, nil
}

// issue returns err in strict mode; with recovery it records the action and
// returns nil.
func (p *planner) issue(err error, action string) error {
	err = fmt.Errorf("item %d: %w", p.index, err)
	if !p.recover {
		return err
	}
	p.recoveries = append(p.recoveries, Recovery{Err: err, Action: action})
	return nil
}

func (p *planner) operation() (string, error) {
	op := strings.ToUpper(strings.TrimSpace(p.item.MaskOperation))
	switch op {
	case MaskOperationAverageSubtraction, MaskOperationTID, MaskOperationReversedTID:
		return op, nil
	}
	if len(p.item.MaskFrameNumbers) == 0 {
		return "", fmt.Errorf("item %d: %w: %q", p.index, ErrUnsupportedMaskOperation, p.item.MaskOperation)
	}
	if err := p.issue(fmt.Errorf("%w: %q", ErrUnsupportedMaskOperation, p.item.MaskOperation), "treated as AVG_SUB of its Mask Frame Numbers"); err != nil {
		return "", err
	}
	return MaskOperationAverageSubtraction, nil
}

func (p *planner) contrastAveraging() (int, error) {
	if !p.item.HasContrastFrameAveraging {
		return 1, nil
	}
	if p.item.ContrastFrameAveraging >= 1 {
		return p.item.ContrastFrameAveraging, nil
	}
	if err := p.issue(fmt.Errorf("%w: %d", ErrInvalidContrastFrameAveraging, p.item.ContrastFrameAveraging), "no contrast frame averaging"); err != nil {
		return 0, err
	}
	return 1, nil
}

func (p *planner) tidOffset() (int, error) {
	if p.item.HasTIDOffset {
		return p.item.TIDOffset, nil
	}
	if err := p.issue(ErrMissingTIDOffset, fmt.Sprintf("used the default TID Offset %d", defaultTIDOffset)); err != nil {
		return 0, err
	}
	return defaultTIDOffset, nil
}

// applicableRanges returns the item's explicit Applicable Frame Range,
// validated, or the implicit range of its Mask Operation.
func (p *planner) applicableRanges() ([]frameRange, error) {
	if len(p.item.ApplicableFrameRange) > 0 {
		return p.explicitRanges()
	}
	switch p.op {
	case MaskOperationTID:
		// f, f+n-1, f-T and f-T+n-1 must all be frames of the object.
		start := max(1, 1+p.offset)
		end := min(p.frames, p.frames+p.offset) - p.averaging + 1
		if start > end {
			return nil, nil
		}
		return []frameRange{{start, end}}, nil
	case MaskOperationReversedTID:
		if err := p.issue(ErrMissingApplicableFrameRange, "applied the item to every frame"); err != nil {
			return nil, err
		}
		return []frameRange{{1, p.frames}}, nil
	default:
		end := p.frames - p.averaging + 1
		if end < 1 {
			return nil, nil
		}
		return []frameRange{{1, end}}, nil
	}
}

func (p *planner) explicitRanges() ([]frameRange, error) {
	values := p.item.ApplicableFrameRange
	if len(values)%2 != 0 {
		if err := p.issue(fmt.Errorf("%w: %d values", ErrInvalidFrameRange, len(values)), "dropped the unpaired last value"); err != nil {
			return nil, err
		}
		values = values[:len(values)-1]
	}
	ranges := make([]frameRange, 0, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		r := frameRange{values[i], values[i+1]}
		if r.start > r.end {
			if err := p.issue(fmt.Errorf("%w: start %d after end %d", ErrInvalidFrameRange, r.start, r.end), "swapped the pair"); err != nil {
				return nil, err
			}
			r.start, r.end = r.end, r.start
		}
		if r.start < 1 || r.end > p.frames {
			if err := p.issue(fmt.Errorf("%w: %d-%d outside frames 1-%d", ErrInvalidFrameRange, r.start, r.end, p.frames), "clipped the pair to the object's frames"); err != nil {
				return nil, err
			}
			r.start, r.end = max(r.start, 1), min(r.end, p.frames)
			if r.start > r.end {
				continue
			}
		}
		ranges = append(ranges, r)
	}
	return ranges, nil
}

func (p *planner) plan(frame int) (Plan, error) {
	plan := Plan{
		ContrastFrame:        frame,
		Item:                 p.index,
		Operation:            p.op,
		SubtractionItemID:    p.item.SubtractionItemID,
		HasSubtractionItemID: p.item.HasSubtractionItemID,
	}
	var err error
	if plan.ContrastFrames, err = p.window(frame, p.averaging); err != nil {
		return Plan{}, err
	}
	switch p.op {
	case MaskOperationAverageSubtraction:
		plan.MaskFrames, err = p.averageMaskFrames()
	case MaskOperationTID:
		plan.MaskFrames, err = p.intervalMask(frame - p.offset)
	case MaskOperationReversedTID:
		plan.MaskFrames, err = p.intervalMask(frame - (p.offset - rangeOrdinal(p.ranges, frame)))
	}
	if err != nil {
		return Plan{}, err
	}
	if plan.Shift, plan.ShiftSource, err = p.shift(frame); err != nil {
		return Plan{}, err
	}
	plan.Recoveries = p.recoveries
	return plan, nil
}

// window returns the contrast frames first .. first+count-1.
func (p *planner) window(first, count int) ([]int, error) {
	last := first + count - 1
	if last > p.frames {
		if err := p.issue(fmt.Errorf("%w: frames %d-%d, object has %d", ErrContrastAveragingOverrun, first, last, p.frames), "averaged only the contrast frames that exist"); err != nil {
			return nil, err
		}
		last = p.frames
	}
	frames := make([]int, 0, last-first+1)
	for f := first; f <= last; f++ {
		frames = append(frames, f)
	}
	return frames, nil
}

func (p *planner) averageMaskFrames() ([]int, error) {
	if len(p.item.MaskFrameNumbers) == 0 {
		return nil, fmt.Errorf("item %d: %w", p.index, ErrMissingMaskFrameNumbers)
	}
	frames := make([]int, 0, len(p.item.MaskFrameNumbers))
	for _, f := range p.item.MaskFrameNumbers {
		if f >= 1 && f <= p.frames {
			frames = append(frames, f)
			continue
		}
		if err := p.issue(fmt.Errorf("%w: mask frame %d, object has %d frames", ErrMaskFrameOutOfRange, f, p.frames), "dropped the mask frame"); err != nil {
			return nil, err
		}
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("item %d: %w: no mask frame inside frames 1-%d", p.index, ErrMaskFrameOutOfRange, p.frames)
	}
	return frames, nil
}

// intervalMask is the TID/REV_TID mask: frames first .. first+averaging-1.
func (p *planner) intervalMask(first int) ([]int, error) {
	last := first + p.averaging - 1
	if first >= 1 && last <= p.frames {
		frames := make([]int, 0, p.averaging)
		for f := first; f <= last; f++ {
			frames = append(frames, f)
		}
		return frames, nil
	}
	if err := p.issue(fmt.Errorf("%w: mask frames %d-%d, object has %d frames", ErrMaskFrameOutOfRange, first, last, p.frames), "clamped the mask frames into the object"); err != nil {
		return nil, err
	}
	var frames []int
	for f := first; f <= last; f++ {
		clamped := min(max(f, 1), p.frames)
		if len(frames) == 0 || frames[len(frames)-1] != clamped {
			frames = append(frames, clamped)
		}
	}
	return frames, nil
}

func (p *planner) shift(frame int) (Shift, ShiftSource, error) {
	var perFrame []FramePixelShift
	if frame-1 < len(p.module.PerFramePixelShifts) {
		perFrame = p.module.PerFramePixelShifts[frame-1]
	}
	group, source := perFrame, ShiftFromPerFrameFunctionalGroup
	if len(perFrame) == 0 {
		group, source = p.module.SharedPixelShifts, ShiftFromSharedFunctionalGroup
	} else if len(p.module.SharedPixelShifts) > 0 {
		if err := p.issue(fmt.Errorf("%w: frame %d", object.ErrFunctionalGroupConflict, frame), "used the per-frame Frame Pixel Shift"); err != nil {
			return Shift{}, ShiftNone, err
		}
	}
	if len(group) > 0 {
		values, found, err := p.groupShift(group, frame)
		if err != nil {
			return Shift{}, ShiftNone, err
		}
		if found {
			return p.parseShift(values, source)
		}
	}
	if p.item.MaskSubPixelShift != nil {
		return p.parseShift(p.item.MaskSubPixelShift, ShiftFromModule)
	}
	return Shift{}, ShiftNone, nil
}

func (p *planner) groupShift(group []FramePixelShift, frame int) ([]float64, bool, error) {
	if p.item.HasSubtractionItemID {
		for _, entry := range group {
			if entry.HasSubtractionItemID && entry.SubtractionItemID == p.item.SubtractionItemID {
				return entry.MaskSubPixelShift, true, nil
			}
		}
		return nil, false, nil
	}
	if len(group) == 1 {
		err := p.issue(fmt.Errorf("%w: frame %d has a Frame Pixel Shift", ErrMissingSubtractionItemID, frame), "used the frame's only Frame Pixel Shift")
		return group[0].MaskSubPixelShift, err == nil, err
	}
	err := p.issue(fmt.Errorf("%w: frame %d has %d Frame Pixel Shifts", ErrMissingSubtractionItemID, frame, len(group)), "used the Mask Module's shift")
	return nil, false, err
}

func (p *planner) parseShift(values []float64, source ShiftSource) (Shift, ShiftSource, error) {
	if len(values) == 2 && !math.IsNaN(values[0]) && !math.IsInf(values[0], 0) && !math.IsNaN(values[1]) && !math.IsInf(values[1], 0) {
		return Shift{Row: values[0], Column: values[1]}, source, nil
	}
	if err := p.issue(fmt.Errorf("%w: %v from %s", ErrInvalidPixelShift, values, source), "used no shift"); err != nil {
		return Shift{}, ShiftNone, err
	}
	return Shift{}, ShiftNone, nil
}

func rangesContain(ranges []frameRange, frame int) bool {
	for _, r := range ranges {
		if frame >= r.start && frame <= r.end {
			return true
		}
	}
	return false
}

// rangeOrdinal is frame's 0-based position among the frames of the
// applicable range, in increasing frame order.
func rangeOrdinal(ranges []frameRange, frame int) int {
	ordinal := 0
	for f := 1; f < frame; f++ {
		if rangesContain(ranges, f) {
			ordinal++
		}
	}
	return ordinal
}
