// Package dsa implements the DICOM Mask Module (PS3.3 C.7.6.10) that XA and
// XRF multi-frame objects, including Enhanced XA/XRF, use to describe digital
// subtraction angiography: which frames form the mask, which contrast frames
// it is subtracted from, and the registration shift applied to the mask.
//
// ReadModule parses the Mask Subtraction Sequence (0028,6100), Recommended
// Viewing Mode (0028,1090), Number of Frames and the Frame Pixel Shift
// functional group (0028,9415). Module.Plan resolves one contrast frame into
// a Plan: the frames averaged to form the mask, the frames averaged to form
// the contrast image (Contrast Frame Averaging) and the mask shift, for every
// Mask Operation (AVG_SUB, TID and REV_TID). AverageFrames and ApplyShift do
// the pixel arithmetic in modality values, with the standard's sign
// convention for the shift (see Shift).
//
// Inconsistent inputs fail with typed errors. PlanOptions.Recover opts into
// documented recoveries, each reported in Plan.Recoveries.
package dsa

import (
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Mask Operation (0028,6101) defined terms (PS3.3 C.7.6.10.1.1).
const (
	// MaskOperationAverageSubtraction averages the Mask Frame Numbers into
	// one mask and subtracts it from every contrast frame in the Applicable
	// Frame Range.
	MaskOperationAverageSubtraction = "AVG_SUB"
	// MaskOperationTID is Time Interval Differencing: the mask of frame f is
	// frame f - TID Offset.
	MaskOperationTID = "TID"
	// MaskOperationReversedTID is Reversed Time Interval Differencing: the
	// TID Offset is subtracted from the first contrast frame of the
	// Applicable Frame Range and decremented by one for every later frame.
	MaskOperationReversedTID = "REV_TID"
	// MaskOperationReviewedTID is the former, misspelled name of
	// MaskOperationReversedTID.
	//
	// Deprecated: use MaskOperationReversedTID.
	MaskOperationReviewedTID = MaskOperationReversedTID
)

// defaultTIDOffset is the TID Offset (0028,6120) of a zero-length value.
const defaultTIDOffset = 1

// Recommended Viewing Mode (0028,1090) defined terms.
const (
	RecommendedViewingSubtraction = "SUB"
	RecommendedViewingNative      = "NAT"
)

var (
	tagNumberOfFrames           = core.NewTag(0x0028, 0x0008)
	tagMaskSubtractionSequence  = core.NewTag(0x0028, 0x6100)
	tagMaskOperation            = core.NewTag(0x0028, 0x6101)
	tagApplicableFrameRange     = core.NewTag(0x0028, 0x6102)
	tagMaskFrameNumbers         = core.NewTag(0x0028, 0x6110)
	tagContrastFrameAveraging   = core.NewTag(0x0028, 0x6112)
	tagMaskSubPixelShift        = core.NewTag(0x0028, 0x6114)
	tagTIDOffset                = core.NewTag(0x0028, 0x6120)
	tagMaskOperationExplanation = core.NewTag(0x0028, 0x6190)
	tagFramePixelShiftSequence  = core.NewTag(0x0028, 0x9415)
	tagSubtractionItemID        = core.NewTag(0x0028, 0x9416)
	tagMaskSelectionMode        = core.NewTag(0x0028, 0x9454)
	tagRecommendedViewingMode   = core.NewTag(0x0028, 0x1090)
	tagSharedFunctionalGroups   = core.NewTag(0x5200, 0x9229)
	tagPerFrameFunctionalGroups = core.NewTag(0x5200, 0x9230)
)

// MaskSubtractionItem is one item of the Mask Subtraction Sequence
// (0028,6100). Frame numbers are 1-based, as in DICOM.
type MaskSubtractionItem struct {
	// MaskOperation is Mask Operation (0028,6101), trimmed, or "" if absent.
	MaskOperation string
	// SubtractionItemID is Subtraction Item ID (0028,9416), which links the
	// item to the Frame Pixel Shift functional group. HasSubtractionItemID is
	// false when the element is absent.
	SubtractionItemID    int
	HasSubtractionItemID bool
	// ApplicableFrameRange is Applicable Frame Range (0028,6102): (start,
	// end) inclusive pairs. Empty means the implicit range of the Mask
	// Operation (see Module.Plan).
	ApplicableFrameRange []int
	// MaskFrameNumbers is Mask Frame Numbers (0028,6110): for AVG_SUB, the
	// frames averaged together to generate the mask. They are not paired
	// with contrast frames.
	MaskFrameNumbers []int
	// ContrastFrameAveraging is Contrast Frame Averaging (0028,6112): the
	// number of contrast frames, starting with the current frame, averaged
	// before subtraction. HasContrastFrameAveraging is false when absent
	// (no averaging).
	ContrastFrameAveraging    int
	HasContrastFrameAveraging bool
	// MaskSubPixelShift is Mask Sub-pixel Shift (0028,6114) as stored:
	// (row, column) in DICOM's sign convention. Nil when absent; Plan
	// rejects a value that is not exactly two finite numbers.
	MaskSubPixelShift []float64
	// TIDOffset is TID Offset (0028,6120). HasTIDOffset reports the element
	// is present; a zero-length value is present and defaults to 1
	// (TIDOffsetDefaulted).
	TIDOffset          int
	HasTIDOffset       bool
	TIDOffsetDefaulted bool
	// Explanation is Mask Operation Explanation (0028,6190), or "".
	Explanation string
	// MaskSelectionMode is Mask Selection Mode (0028,9454) (SYSTEM or USER),
	// or "".
	MaskSelectionMode string
}

// AppliesToFrame reports whether the 1-based frameNumber falls within one of
// the item's explicit ApplicableFrameRange pairs. An empty range returns
// true; Module.Plan resolves the Mask Operation's implicit range instead.
func (item MaskSubtractionItem) AppliesToFrame(frameNumber int) bool {
	if len(item.ApplicableFrameRange) == 0 {
		return true
	}
	for i := 0; i+1 < len(item.ApplicableFrameRange); i += 2 {
		start, end := item.ApplicableFrameRange[i], item.ApplicableFrameRange[i+1]
		if frameNumber >= start && frameNumber <= end {
			return true
		}
	}
	return false
}

// FramePixelShift is one item of the Frame Pixel Shift Sequence (0028,9415):
// the Mask Sub-pixel Shift of the Mask Subtraction Sequence item whose
// Subtraction Item ID matches.
type FramePixelShift struct {
	SubtractionItemID    int
	HasSubtractionItemID bool
	// MaskSubPixelShift is the (row, column) value as stored, nil if absent.
	MaskSubPixelShift []float64
}

// Module is the parsed Mask Module of one multi-frame object.
type Module struct {
	Items []MaskSubtractionItem
	// RecommendedViewingMode is Recommended Viewing Mode (0028,1090), or "".
	RecommendedViewingMode string
	// NumberOfFrames is Number of Frames (0028,0008); 1 when absent and 0
	// when it cannot be parsed.
	NumberOfFrames int
	// SharedPixelShifts is the Frame Pixel Shift Sequence of the Shared
	// Functional Groups Sequence, nil when absent.
	SharedPixelShifts []FramePixelShift
	// PerFramePixelShifts[i] is the Frame Pixel Shift Sequence of the
	// Per-Frame Functional Groups item of frame i+1, nil when absent.
	PerFramePixelShifts [][]FramePixelShift
}

// ReadModule reads the Mask Module of obj. ok is false when obj carries no
// Mask Subtraction Sequence items.
func ReadModule(obj *object.Object) (module *Module, ok bool) {
	if obj == nil {
		return nil, false
	}
	seqItems, present := obj.GetSequence(tagMaskSubtractionSequence)
	if !present {
		return nil, false
	}
	module = &Module{RecommendedViewingMode: RecommendedViewingMode(obj), NumberOfFrames: numberOfFrames(obj)}
	for _, seqItem := range seqItems {
		if seqItem == nil {
			continue
		}
		module.Items = append(module.Items, readItem(seqItem))
	}
	if len(module.Items) == 0 {
		return nil, false
	}
	if shared, ok := obj.GetSequence(tagSharedFunctionalGroups); ok && len(shared) > 0 && shared[0] != nil {
		module.SharedPixelShifts = readFramePixelShifts(shared[0])
	}
	if frames, ok := obj.GetSequence(tagPerFrameFunctionalGroups); ok {
		module.PerFramePixelShifts = make([][]FramePixelShift, len(frames))
		for i, frame := range frames {
			if frame != nil {
				module.PerFramePixelShifts[i] = readFramePixelShifts(frame)
			}
		}
	}
	return module, true
}

// ReadMaskSubtraction reads only the Mask Subtraction Sequence (0028,6100)
// items of obj. ok is false when the object has no Mask Module. Use
// ReadModule to plan subtractions.
func ReadMaskSubtraction(obj *object.Object) (items []MaskSubtractionItem, ok bool) {
	module, ok := ReadModule(obj)
	if !ok {
		return nil, false
	}
	return module.Items, true
}

// RecommendedViewingMode returns Recommended Viewing Mode (0028,1090), or ""
// if absent.
func RecommendedViewingMode(obj *object.Object) string {
	if obj == nil {
		return ""
	}
	value, _ := obj.GetString(tagRecommendedViewingMode)
	return strings.TrimSpace(value)
}

func readItem(seqItem *object.Object) MaskSubtractionItem {
	item := MaskSubtractionItem{}
	if op, ok := seqItem.GetString(tagMaskOperation); ok {
		item.MaskOperation = strings.TrimSpace(op)
	}
	if explanation, ok := seqItem.GetString(tagMaskOperationExplanation); ok {
		item.Explanation = explanation
	}
	if mode, ok := seqItem.GetString(tagMaskSelectionMode); ok {
		item.MaskSelectionMode = strings.TrimSpace(mode)
	}
	if id, ok := getUint16s(seqItem, tagSubtractionItemID); ok && len(id) > 0 {
		item.SubtractionItemID, item.HasSubtractionItemID = int(id[0]), true
	}
	if frameRange, ok := getUint16s(seqItem, tagApplicableFrameRange); ok {
		item.ApplicableFrameRange = uint16sToInts(frameRange)
	}
	if frameNumbers, ok := getUint16s(seqItem, tagMaskFrameNumbers); ok {
		item.MaskFrameNumbers = uint16sToInts(frameNumbers)
	}
	if averaging, ok := getUint16s(seqItem, tagContrastFrameAveraging); ok && len(averaging) > 0 {
		item.ContrastFrameAveraging, item.HasContrastFrameAveraging = int(averaging[0]), true
	}
	item.MaskSubPixelShift = getFloat32s(seqItem, tagMaskSubPixelShift)
	if seqItem.Has(tagTIDOffset) {
		item.HasTIDOffset = true
		if offsets := getInt16s(seqItem, tagTIDOffset); len(offsets) > 0 {
			item.TIDOffset = int(offsets[0])
		} else {
			item.TIDOffset, item.TIDOffsetDefaulted = defaultTIDOffset, true
		}
	}
	return item
}

func readFramePixelShifts(group *object.Object) []FramePixelShift {
	items, ok := group.GetSequence(tagFramePixelShiftSequence)
	if !ok {
		return nil
	}
	out := make([]FramePixelShift, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		shift := FramePixelShift{MaskSubPixelShift: getFloat32s(item, tagMaskSubPixelShift)}
		if id, ok := getUint16s(item, tagSubtractionItemID); ok && len(id) > 0 {
			shift.SubtractionItemID, shift.HasSubtractionItemID = int(id[0]), true
		}
		out = append(out, shift)
	}
	return out
}

func numberOfFrames(obj *object.Object) int {
	if !obj.Has(tagNumberOfFrames) {
		return 1
	}
	value, ok := obj.GetString(tagNumberOfFrames)
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func getUint16s(obj *object.Object, tag core.Tag) ([]uint16, bool) {
	raw, ok := obj.GetRaw(tag)
	if !ok || len(raw) < 2 {
		return nil, false
	}
	order := obj.ValueByteOrder()
	out := make([]uint16, len(raw)/2)
	for i := range out {
		out[i] = order.Uint16(raw[i*2 : i*2+2])
	}
	return out, true
}

func getInt16s(obj *object.Object, tag core.Tag) []int16 {
	raw, ok := obj.GetRaw(tag)
	if !ok || len(raw) < 2 {
		return nil
	}
	order := obj.ValueByteOrder()
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(order.Uint16(raw[i*2 : i*2+2]))
	}
	return out
}

func getFloat32s(obj *object.Object, tag core.Tag) []float64 {
	raw, ok := obj.GetRaw(tag)
	if !ok || len(raw) < 4 {
		return nil
	}
	order := obj.ValueByteOrder()
	out := make([]float64, len(raw)/4)
	for i := range out {
		out[i] = float64(math.Float32frombits(order.Uint32(raw[i*4 : i*4+4])))
	}
	return out
}

func uint16sToInts(values []uint16) []int {
	out := make([]int, len(values))
	for i, v := range values {
		out[i] = int(v)
	}
	return out
}
