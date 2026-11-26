package dsa

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func uint16sBytes(values ...uint16) []byte {
	out := make([]byte, len(values)*2)
	for i, v := range values {
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], v)
	}
	return out
}

func int16sBytes(values ...int16) []byte {
	out := make([]byte, len(values)*2)
	for i, v := range values {
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(v))
	}
	return out
}

func float32sBytes(values ...float32) []byte {
	out := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(out[i*4:i*4+4], math.Float32bits(v))
	}
	return out
}

func sequenceElement(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: core.VRSQ},
		Value:  core.SequenceValue{Items: items},
	}
}

func item(elements ...core.Element) core.DataSet { return core.DataSet{Elements: elements} }

func pixelShiftItem(id uint16, row, column float32) core.DataSet {
	return item(
		core.NewRawElement(tagSubtractionItemID, core.VRUS, uint16sBytes(id)),
		core.NewRawElement(tagMaskSubPixelShift, core.VRFL, float32sBytes(row, column)),
	)
}

func TestReadModuleParsesEveryMaskModuleAttribute(t *testing.T) {
	first := item(
		core.NewRawElement(tagMaskOperation, core.VRCS, []byte("TID ")),
		core.NewRawElement(tagSubtractionItemID, core.VRUS, uint16sBytes(7)),
		core.NewRawElement(tagApplicableFrameRange, core.VRUS, uint16sBytes(3, 10)),
		core.NewRawElement(tagContrastFrameAveraging, core.VRUS, uint16sBytes(2)),
		core.NewRawElement(tagMaskSubPixelShift, core.VRFL, float32sBytes(0.5, -1.25)),
		core.NewRawElement(tagTIDOffset, core.VRSS, int16sBytes(-2)),
		core.NewRawElement(tagMaskOperationExplanation, core.VRST, []byte("Reverse interval")),
		core.NewRawElement(tagMaskSelectionMode, core.VRCS, []byte("USER")),
	)
	second := item(
		core.NewRawElement(tagMaskOperation, core.VRCS, []byte("TID ")),
		core.NewRawElement(tagTIDOffset, core.VRSS, nil),
	)
	obj := object.FromElements([]core.Element{
		core.NewRawElement(tagNumberOfFrames, core.VRIS, []byte("12")),
		sequenceElement(tagMaskSubtractionSequence, first, second),
		core.NewRawElement(tagRecommendedViewingMode, core.VRCS, []byte("SUB ")),
		sequenceElement(tagSharedFunctionalGroups, item(sequenceElement(tagFramePixelShiftSequence, pixelShiftItem(7, 1, 2)))),
		sequenceElement(tagPerFrameFunctionalGroups, item(), item(sequenceElement(tagFramePixelShiftSequence, pixelShiftItem(7, 0.25, -0.75)))),
	}, std.Dictionary)

	module, ok := ReadModule(obj)
	if !ok {
		t.Fatal("ReadModule() ok = false, want the module")
	}
	want := MaskSubtractionItem{
		MaskOperation:             MaskOperationTID,
		SubtractionItemID:         7,
		HasSubtractionItemID:      true,
		ApplicableFrameRange:      []int{3, 10},
		ContrastFrameAveraging:    2,
		HasContrastFrameAveraging: true,
		MaskSubPixelShift:         []float64{0.5, -1.25},
		TIDOffset:                 -2,
		HasTIDOffset:              true,
		Explanation:               "Reverse interval",
		MaskSelectionMode:         "USER",
	}
	if !reflect.DeepEqual(module.Items[0], want) {
		t.Errorf("Items[0] = %#v\nwant %#v", module.Items[0], want)
	}
	if got := module.Items[1]; !got.HasTIDOffset || !got.TIDOffsetDefaulted || got.TIDOffset != 1 {
		t.Errorf("zero-length TID Offset = %d present=%v defaulted=%v, want the default 1", got.TIDOffset, got.HasTIDOffset, got.TIDOffsetDefaulted)
	}
	if module.NumberOfFrames != 12 || module.RecommendedViewingMode != RecommendedViewingSubtraction {
		t.Errorf("NumberOfFrames/RecommendedViewingMode = %d/%q, want 12/SUB", module.NumberOfFrames, module.RecommendedViewingMode)
	}
	wantShared := []FramePixelShift{{SubtractionItemID: 7, HasSubtractionItemID: true, MaskSubPixelShift: []float64{1, 2}}}
	if !reflect.DeepEqual(module.SharedPixelShifts, wantShared) {
		t.Errorf("SharedPixelShifts = %#v, want %#v", module.SharedPixelShifts, wantShared)
	}
	if len(module.PerFramePixelShifts) != 2 || module.PerFramePixelShifts[0] != nil ||
		!reflect.DeepEqual(module.PerFramePixelShifts[1], []FramePixelShift{{SubtractionItemID: 7, HasSubtractionItemID: true, MaskSubPixelShift: []float64{0.25, -0.75}}}) {
		t.Errorf("PerFramePixelShifts = %#v", module.PerFramePixelShifts)
	}
	if items, ok := ReadMaskSubtraction(obj); !ok || len(items) != 2 {
		t.Errorf("ReadMaskSubtraction() = %d items, %v; want 2, true", len(items), ok)
	}
}

func TestReadModuleAbsentModule(t *testing.T) {
	obj := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0060), core.VRCS, []byte("XA")),
	}, std.Dictionary)
	if module, ok := ReadModule(obj); ok || module != nil {
		t.Fatalf("ReadModule() = %#v, %v; want nil, false", module, ok)
	}
	if items, ok := ReadMaskSubtraction(obj); ok || items != nil {
		t.Fatalf("ReadMaskSubtraction() = %#v, %v; want nil, false", items, ok)
	}
	if mode := RecommendedViewingMode(obj); mode != "" {
		t.Errorf("RecommendedViewingMode() = %q, want empty", mode)
	}
	if module, ok := ReadModule(nil); ok || module != nil {
		t.Fatal("ReadModule(nil) must report no module")
	}
}

func TestReadModuleSingleFrameDefaultsNumberOfFrames(t *testing.T) {
	obj := object.FromElements([]core.Element{
		sequenceElement(tagMaskSubtractionSequence, item(core.NewRawElement(tagMaskOperation, core.VRCS, []byte("AVG_SUB ")))),
	}, std.Dictionary)
	module, ok := ReadModule(obj)
	if !ok || module.NumberOfFrames != 1 {
		t.Fatalf("ReadModule() NumberOfFrames = %v (ok=%v), want 1 when (0028,0008) is absent", module, ok)
	}
}

func avgSub(masks ...int) MaskSubtractionItem {
	return MaskSubtractionItem{MaskOperation: MaskOperationAverageSubtraction, MaskFrameNumbers: masks}
}

func tid(offset int) MaskSubtractionItem {
	return MaskSubtractionItem{MaskOperation: MaskOperationTID, TIDOffset: offset, HasTIDOffset: true}
}

func withRange(item MaskSubtractionItem, values ...int) MaskSubtractionItem {
	item.ApplicableFrameRange = values
	return item
}

func withAveraging(item MaskSubtractionItem, n int) MaskSubtractionItem {
	item.ContrastFrameAveraging, item.HasContrastFrameAveraging = n, true
	return item
}

func TestPlanResolvesMaskOperations(t *testing.T) {
	revTID := MaskSubtractionItem{MaskOperation: MaskOperationReversedTID, TIDOffset: 3, HasTIDOffset: true, ApplicableFrameRange: []int{4, 8}}
	tests := []struct {
		name          string
		items         []MaskSubtractionItem
		frames        int
		frame         int
		wantMask      []int
		wantContrast  []int
		wantItem      int
		wantErr       error
		wantRecovered bool
	}{
		// AVG_SUB: Mask Frame Numbers are averaged into one shared mask.
		{name: "AVG_SUB single mask", items: []MaskSubtractionItem{avgSub(1)}, frames: 5, frame: 3, wantMask: []int{1}, wantContrast: []int{3}},
		{name: "AVG_SUB averages every mask frame", items: []MaskSubtractionItem{avgSub(1, 2, 3)}, frames: 8, frame: 4, wantMask: []int{1, 2, 3}, wantContrast: []int{4}},
		{name: "AVG_SUB same mask for a later frame", items: []MaskSubtractionItem{avgSub(1, 2, 3)}, frames: 8, frame: 8, wantMask: []int{1, 2, 3}, wantContrast: []int{8}},
		{name: "AVG_SUB contrast averaging", items: []MaskSubtractionItem{withAveraging(avgSub(1, 2), 2)}, frames: 5, frame: 4, wantMask: []int{1, 2}, wantContrast: []int{4, 5}},
		{name: "AVG_SUB implicit range ends at N-averaging+1", items: []MaskSubtractionItem{withAveraging(avgSub(1), 2)}, frames: 5, frame: 5, wantErr: ErrFrameNotApplicable},
		{name: "AVG_SUB explicit range overrun", items: []MaskSubtractionItem{withAveraging(withRange(avgSub(1), 3, 5), 2)}, frames: 5, frame: 5, wantErr: ErrContrastAveragingOverrun},
		{name: "AVG_SUB explicit range overrun recovered", items: []MaskSubtractionItem{withAveraging(withRange(avgSub(1), 3, 5), 2)}, frames: 5, frame: 5, wantMask: []int{1}, wantContrast: []int{5}, wantRecovered: true},
		{name: "discontinuous range outside", items: []MaskSubtractionItem{withRange(avgSub(1), 2, 3, 6, 7)}, frames: 8, frame: 4, wantErr: ErrFrameNotApplicable},
		{name: "discontinuous range second pair", items: []MaskSubtractionItem{withRange(avgSub(1), 2, 3, 6, 7)}, frames: 8, frame: 6, wantMask: []int{1}, wantContrast: []int{6}},
		{name: "first applicable item wins", items: []MaskSubtractionItem{withRange(avgSub(1), 2, 4), withRange(avgSub(5), 3, 8)}, frames: 8, frame: 6, wantMask: []int{5}, wantContrast: []int{6}, wantItem: 1},
		{name: "AVG_SUB mask frame outside the object", items: []MaskSubtractionItem{avgSub(1, 9)}, frames: 5, frame: 3, wantErr: ErrMaskFrameOutOfRange},
		{name: "AVG_SUB mask frame outside recovered", items: []MaskSubtractionItem{avgSub(1, 9)}, frames: 5, frame: 3, wantMask: []int{1}, wantContrast: []int{3}, wantRecovered: true},
		{name: "AVG_SUB without mask frames", items: []MaskSubtractionItem{avgSub()}, frames: 5, frame: 3, wantErr: ErrMissingMaskFrameNumbers},
		// TID: mask of frame f is f - TID Offset.
		{name: "TID first frame without mask", items: []MaskSubtractionItem{tid(2)}, frames: 6, frame: 2, wantErr: ErrFrameNotApplicable},
		{name: "TID first valid frame", items: []MaskSubtractionItem{tid(2)}, frames: 6, frame: 3, wantMask: []int{1}, wantContrast: []int{3}},
		{name: "TID last frame", items: []MaskSubtractionItem{tid(2)}, frames: 6, frame: 6, wantMask: []int{4}, wantContrast: []int{6}},
		{name: "TID negative offset uses a later mask", items: []MaskSubtractionItem{tid(-2)}, frames: 6, frame: 4, wantMask: []int{6}, wantContrast: []int{4}},
		{name: "TID negative offset at the sequence end", items: []MaskSubtractionItem{tid(-2)}, frames: 6, frame: 5, wantErr: ErrFrameNotApplicable},
		{name: "TID averages mask and contrast windows", items: []MaskSubtractionItem{withAveraging(tid(2), 2)}, frames: 6, frame: 3, wantMask: []int{1, 2}, wantContrast: []int{3, 4}},
		{name: "TID averaging shortens the implicit range", items: []MaskSubtractionItem{withAveraging(tid(2), 2)}, frames: 6, frame: 6, wantErr: ErrFrameNotApplicable},
		{name: "TID explicit range before the first frame", items: []MaskSubtractionItem{withRange(tid(2), 1, 6)}, frames: 6, frame: 1, wantErr: ErrMaskFrameOutOfRange},
		{name: "TID explicit range before the first frame recovered", items: []MaskSubtractionItem{withRange(tid(2), 1, 6)}, frames: 6, frame: 1, wantMask: []int{1}, wantContrast: []int{1}, wantRecovered: true},
		{name: "TID explicit range after the last frame", items: []MaskSubtractionItem{withRange(tid(-1), 3, 6)}, frames: 6, frame: 6, wantErr: ErrMaskFrameOutOfRange},
		{name: "TID explicit range after the last frame recovered", items: []MaskSubtractionItem{withRange(tid(-1), 3, 6)}, frames: 6, frame: 6, wantMask: []int{6}, wantContrast: []int{6}, wantRecovered: true},
		{name: "TID without offset", items: []MaskSubtractionItem{{MaskOperation: MaskOperationTID}}, frames: 6, frame: 4, wantErr: ErrMissingTIDOffset},
		{name: "TID without offset recovered", items: []MaskSubtractionItem{{MaskOperation: MaskOperationTID}}, frames: 6, frame: 4, wantMask: []int{3}, wantContrast: []int{4}, wantRecovered: true},
		{name: "TID zero-length offset defaults to 1", items: []MaskSubtractionItem{{MaskOperation: MaskOperationTID, HasTIDOffset: true, TIDOffset: 1, TIDOffsetDefaulted: true}}, frames: 6, frame: 4, wantMask: []int{3}, wantContrast: []int{4}},
		// REV_TID: offset 3 on the first frame of 4-8, decremented per frame.
		{name: "REV_TID first frame", items: []MaskSubtractionItem{revTID}, frames: 10, frame: 4, wantMask: []int{1}, wantContrast: []int{4}},
		{name: "REV_TID second frame", items: []MaskSubtractionItem{revTID}, frames: 10, frame: 5, wantMask: []int{3}, wantContrast: []int{5}},
		{name: "REV_TID offset reaches zero", items: []MaskSubtractionItem{revTID}, frames: 10, frame: 7, wantMask: []int{7}, wantContrast: []int{7}},
		{name: "REV_TID offset turns negative", items: []MaskSubtractionItem{revTID}, frames: 10, frame: 8, wantMask: []int{9}, wantContrast: []int{8}},
		{name: "REV_TID ordinal counts discontinuous ranges", items: []MaskSubtractionItem{withRange(revTID, 2, 3, 6, 7)}, frames: 10, frame: 6, wantMask: []int{5}, wantContrast: []int{6}},
		{name: "REV_TID without range", items: []MaskSubtractionItem{{MaskOperation: MaskOperationReversedTID, TIDOffset: 1, HasTIDOffset: true}}, frames: 6, frame: 3, wantErr: ErrMissingApplicableFrameRange},
		{name: "REV_TID without range recovered", items: []MaskSubtractionItem{{MaskOperation: MaskOperationReversedTID, TIDOffset: 1, HasTIDOffset: true}}, frames: 6, frame: 3, wantMask: []int{4}, wantContrast: []int{3}, wantRecovered: true},
		// Inconsistent inputs.
		{name: "unsupported operation", items: []MaskSubtractionItem{{MaskOperation: "MAX_SUB", MaskFrameNumbers: []int{1}}}, frames: 4, frame: 2, wantErr: ErrUnsupportedMaskOperation},
		{name: "unsupported operation recovered as AVG_SUB", items: []MaskSubtractionItem{{MaskOperation: "MAX_SUB", MaskFrameNumbers: []int{1}}}, frames: 4, frame: 2, wantMask: []int{1}, wantContrast: []int{2}, wantRecovered: true},
		{name: "odd applicable range", items: []MaskSubtractionItem{withRange(avgSub(1), 2, 4, 6)}, frames: 8, frame: 3, wantErr: ErrInvalidFrameRange},
		{name: "odd applicable range recovered", items: []MaskSubtractionItem{withRange(avgSub(1), 2, 4, 6)}, frames: 8, frame: 3, wantMask: []int{1}, wantContrast: []int{3}, wantRecovered: true},
		{name: "reversed applicable range recovered", items: []MaskSubtractionItem{withRange(avgSub(1), 4, 2)}, frames: 8, frame: 3, wantMask: []int{1}, wantContrast: []int{3}, wantRecovered: true},
		{name: "range past the last frame", items: []MaskSubtractionItem{withRange(avgSub(1), 2, 20)}, frames: 8, frame: 3, wantErr: ErrInvalidFrameRange},
		{name: "inconsistent item not covering the frame is skipped", items: []MaskSubtractionItem{withRange(avgSub(), 5, 6), withRange(avgSub(1), 2, 4)}, frames: 8, frame: 3, wantMask: []int{1}, wantContrast: []int{3}, wantItem: 1},
		{name: "zero contrast frame averaging", items: []MaskSubtractionItem{withAveraging(avgSub(1), 0)}, frames: 4, frame: 2, wantErr: ErrInvalidContrastFrameAveraging},
		{name: "zero contrast frame averaging recovered", items: []MaskSubtractionItem{withAveraging(avgSub(1), 0)}, frames: 4, frame: 2, wantMask: []int{1}, wantContrast: []int{2}, wantRecovered: true},
		{name: "frame before the first", items: []MaskSubtractionItem{avgSub(1)}, frames: 4, frame: 0, wantErr: ErrFrameOutOfRange},
		{name: "frame after the last", items: []MaskSubtractionItem{avgSub(1)}, frames: 4, frame: 5, wantErr: ErrFrameOutOfRange},
		{name: "no items", frames: 4, frame: 2, wantErr: ErrNoMaskModule},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := &Module{Items: tt.items, NumberOfFrames: tt.frames}
			plan, err := module.Plan(tt.frame, PlanOptions{Recover: tt.wantRecovered})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Plan(%d) error = %v, want %v", tt.frame, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Plan(%d) error = %v", tt.frame, err)
			}
			if !reflect.DeepEqual(plan.MaskFrames, tt.wantMask) || !reflect.DeepEqual(plan.ContrastFrames, tt.wantContrast) || plan.Item != tt.wantItem {
				t.Fatalf("Plan(%d) = mask %v contrast %v item %d; want %v %v %d", tt.frame, plan.MaskFrames, plan.ContrastFrames, plan.Item, tt.wantMask, tt.wantContrast, tt.wantItem)
			}
			if recovered := len(plan.Recoveries) > 0; recovered != tt.wantRecovered {
				t.Fatalf("Recoveries = %v, want recovered=%v", plan.Recoveries, tt.wantRecovered)
			}
			for _, recovery := range plan.Recoveries {
				if recovery.Err == nil || recovery.Action == "" {
					t.Fatalf("recovery %+v must carry the typed error and the action", recovery)
				}
			}
		})
	}
}

func TestPlanRecoveryReportsTheStrictError(t *testing.T) {
	module := &Module{Items: []MaskSubtractionItem{withAveraging(withRange(avgSub(1, 9), 3, 5), 2)}, NumberOfFrames: 5}
	plan, err := module.Plan(5, PlanOptions{Recover: true})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	var sawOverrun, sawMask bool
	for _, recovery := range plan.Recoveries {
		sawOverrun = sawOverrun || errors.Is(recovery.Err, ErrContrastAveragingOverrun)
		sawMask = sawMask || errors.Is(recovery.Err, ErrMaskFrameOutOfRange)
	}
	if !sawOverrun || !sawMask {
		t.Fatalf("Recoveries = %+v, want both the averaging overrun and the mask frame recovery", plan.Recoveries)
	}
}

func TestPlanItemSelectsAnOverlappingItem(t *testing.T) {
	module := &Module{Items: []MaskSubtractionItem{avgSub(1), tid(1)}, NumberOfFrames: 6}
	plan, err := module.PlanItem(1, 4, PlanOptions{})
	if err != nil || plan.Operation != MaskOperationTID || !reflect.DeepEqual(plan.MaskFrames, []int{3}) {
		t.Fatalf("PlanItem(1, 4) = %+v, %v; want TID mask [3]", plan, err)
	}
	if _, err := module.PlanItem(2, 4, PlanOptions{}); !errors.Is(err, ErrItemOutOfRange) {
		t.Fatalf("PlanItem(2) error = %v, want ErrItemOutOfRange", err)
	}
	if _, err := module.PlanItem(1, 1, PlanOptions{}); !errors.Is(err, ErrFrameNotApplicable) {
		t.Fatalf("PlanItem(1, 1) error = %v, want ErrFrameNotApplicable", err)
	}
	if _, err := (&Module{Items: []MaskSubtractionItem{avgSub(1)}}).Plan(1, PlanOptions{}); !errors.Is(err, ErrInvalidNumberOfFrames) {
		t.Fatalf("Plan() without frames error = %v, want ErrInvalidNumberOfFrames", err)
	}
	if plan, err := (&Module{Items: []MaskSubtractionItem{avgSub(1)}}).Plan(3, PlanOptions{NumberOfFrames: 3}); err != nil || plan.ContrastFrame != 3 {
		t.Fatalf("Plan() with PlanOptions.NumberOfFrames = %+v, %v", plan, err)
	}
	var nilModule *Module
	if _, err := nilModule.Plan(1, PlanOptions{}); !errors.Is(err, ErrNoMaskModule) {
		t.Fatalf("nil Module Plan() error = %v, want ErrNoMaskModule", err)
	}
}

func TestPlanShiftResolution(t *testing.T) {
	withID := func(item MaskSubtractionItem, id int) MaskSubtractionItem {
		item.SubtractionItemID, item.HasSubtractionItemID = id, true
		return item
	}
	withShift := func(item MaskSubtractionItem, values ...float64) MaskSubtractionItem {
		item.MaskSubPixelShift = values
		return item
	}
	fg := func(id int, row, column float64) FramePixelShift {
		return FramePixelShift{SubtractionItemID: id, HasSubtractionItemID: true, MaskSubPixelShift: []float64{row, column}}
	}
	tests := []struct {
		name       string
		module     Module
		frame      int
		recover    bool
		wantShift  Shift
		wantSource ShiftSource
		wantErr    error
	}{
		{name: "no shift", module: Module{Items: []MaskSubtractionItem{avgSub(1)}}, frame: 2},
		{name: "module shift", module: Module{Items: []MaskSubtractionItem{withShift(avgSub(1), 0.5, -1.25)}}, frame: 2, wantShift: Shift{Row: 0.5, Column: -1.25}, wantSource: ShiftFromModule},
		{
			name: "per-frame functional group prevails over the module",
			module: Module{
				Items:               []MaskSubtractionItem{withID(withShift(avgSub(1), 0.5, -1.25), 100)},
				PerFramePixelShifts: [][]FramePixelShift{nil, {fg(100, 1.3, 2.4)}, {fg(100, 1.9, 3.0)}},
			},
			frame: 3, wantShift: Shift{Row: 1.9, Column: 3.0}, wantSource: ShiftFromPerFrameFunctionalGroup,
		},
		{
			name: "functional group for another subtraction item falls back to the module",
			module: Module{
				Items:               []MaskSubtractionItem{withID(withShift(avgSub(1), 0.5, -1.25), 100)},
				PerFramePixelShifts: [][]FramePixelShift{nil, {fg(200, 1.3, 2.4)}},
			},
			frame: 2, wantShift: Shift{Row: 0.5, Column: -1.25}, wantSource: ShiftFromModule,
		},
		{
			name: "shared functional group",
			module: Module{
				Items:             []MaskSubtractionItem{withID(avgSub(1), 5)},
				SharedPixelShifts: []FramePixelShift{fg(4, 9, 9), fg(5, -1, 0.5)},
			},
			frame: 2, wantShift: Shift{Row: -1, Column: 0.5}, wantSource: ShiftFromSharedFunctionalGroup,
		},
		{
			name: "shared and per-frame conflict",
			module: Module{
				Items:               []MaskSubtractionItem{withID(avgSub(1), 5)},
				SharedPixelShifts:   []FramePixelShift{fg(5, 9, 9)},
				PerFramePixelShifts: [][]FramePixelShift{nil, {fg(5, 1, 1)}},
			},
			frame: 2, wantErr: object.ErrFunctionalGroupConflict,
		},
		{
			name: "shared and per-frame conflict recovered with the per-frame value",
			module: Module{
				Items:               []MaskSubtractionItem{withID(avgSub(1), 5)},
				SharedPixelShifts:   []FramePixelShift{fg(5, 9, 9)},
				PerFramePixelShifts: [][]FramePixelShift{nil, {fg(5, 1, 1)}},
			},
			frame: 2, recover: true, wantShift: Shift{Row: 1, Column: 1}, wantSource: ShiftFromPerFrameFunctionalGroup,
		},
		{
			name:   "functional group without subtraction item id on the item",
			module: Module{Items: []MaskSubtractionItem{avgSub(1)}, PerFramePixelShifts: [][]FramePixelShift{nil, {fg(5, 1, 1)}}},
			frame:  2, wantErr: ErrMissingSubtractionItemID,
		},
		{
			name:   "single unlinked functional group recovered",
			module: Module{Items: []MaskSubtractionItem{avgSub(1)}, PerFramePixelShifts: [][]FramePixelShift{nil, {fg(5, 1, -1)}}},
			frame:  2, recover: true, wantShift: Shift{Row: 1, Column: -1}, wantSource: ShiftFromPerFrameFunctionalGroup,
		},
		{
			name:   "several unlinked functional groups recovered with the module shift",
			module: Module{Items: []MaskSubtractionItem{withShift(avgSub(1), 2, 3)}, PerFramePixelShifts: [][]FramePixelShift{nil, {fg(5, 1, -1), fg(6, 0, 0)}}},
			frame:  2, recover: true, wantShift: Shift{Row: 2, Column: 3}, wantSource: ShiftFromModule,
		},
		{name: "shift with one value", module: Module{Items: []MaskSubtractionItem{withShift(avgSub(1), 2)}}, frame: 2, wantErr: ErrInvalidPixelShift},
		{name: "non-finite shift recovered", module: Module{Items: []MaskSubtractionItem{withShift(avgSub(1), math.NaN(), 1)}}, frame: 2, recover: true},
		{
			name: "contrast averaging takes the shift of the current frame",
			module: Module{
				Items:               []MaskSubtractionItem{withAveraging(withID(avgSub(1), 1), 2)},
				PerFramePixelShifts: [][]FramePixelShift{nil, {fg(1, 0.5, 0.5)}, {fg(1, 7, 7)}},
			},
			frame: 2, wantShift: Shift{Row: 0.5, Column: 0.5}, wantSource: ShiftFromPerFrameFunctionalGroup,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.module.NumberOfFrames = 3
			plan, err := tt.module.Plan(tt.frame, PlanOptions{Recover: tt.recover})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Plan() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Plan() error = %v", err)
			}
			if plan.Shift != tt.wantShift || plan.ShiftSource != tt.wantSource {
				t.Fatalf("Shift = %+v from %q, want %+v from %q", plan.Shift, plan.ShiftSource, tt.wantShift, tt.wantSource)
			}
			if tt.recover && len(plan.Recoveries) == 0 {
				t.Fatal("recovered plan must report its recovery")
			}
		})
	}
}

// TestEnhancedXAPerFramePixelShift encodes the example of PS3.3 Figure
// C.7.6.16-8: frame 1 is the mask of frames 2 and 3, each with its own Frame
// Pixel Shift linked through Subtraction Item ID 100.
func TestEnhancedXAPerFramePixelShift(t *testing.T) {
	frameGroup := func(row, column float32) core.DataSet {
		return item(sequenceElement(tagFramePixelShiftSequence, pixelShiftItem(100, row, column)))
	}
	obj := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.12.1.1\x00")),
		core.NewRawElement(tagNumberOfFrames, core.VRIS, []byte("3 ")),
		sequenceElement(tagMaskSubtractionSequence, item(
			core.NewRawElement(tagMaskOperation, core.VRCS, []byte("AVG_SUB ")),
			core.NewRawElement(tagSubtractionItemID, core.VRUS, uint16sBytes(100)),
			core.NewRawElement(tagApplicableFrameRange, core.VRUS, uint16sBytes(2, 3)),
			core.NewRawElement(tagMaskFrameNumbers, core.VRUS, uint16sBytes(1)),
			core.NewRawElement(tagMaskSubPixelShift, core.VRFL, float32sBytes(9, 9)),
		)),
		sequenceElement(tagPerFrameFunctionalGroups, item(), frameGroup(1.3, 2.4), frameGroup(1.9, 3.0)),
	}, std.Dictionary)
	module, ok := ReadModule(obj)
	if !ok {
		t.Fatal("ReadModule() found no module")
	}
	for frame, want := range map[int]Shift{2: {Row: 1.3, Column: 2.4}, 3: {Row: 1.9, Column: 3.0}} {
		plan, err := module.Plan(frame, PlanOptions{})
		if err != nil {
			t.Fatalf("Plan(%d) error = %v", frame, err)
		}
		if !reflect.DeepEqual(plan.MaskFrames, []int{1}) || plan.ShiftSource != ShiftFromPerFrameFunctionalGroup ||
			math.Abs(plan.Shift.Row-want.Row) > 1e-6 || math.Abs(plan.Shift.Column-want.Column) > 1e-6 {
			t.Fatalf("Plan(%d) = %+v, want mask [1] and per-frame shift %+v", frame, plan, want)
		}
	}
	if _, err := module.Plan(1, PlanOptions{}); !errors.Is(err, ErrFrameNotApplicable) {
		t.Fatalf("Plan(1) error = %v, want the mask frame outside the applicable range", err)
	}
}

func TestShiftSignConvention(t *testing.T) {
	// C.7.6.10.1.2: positive row offset moves the mask down, positive
	// column offset moves it toward the left-hand columns.
	if dx, dy := (Shift{Row: 2, Column: 3}).Displacement(); dx != -3 || dy != 2 {
		t.Fatalf("Displacement() = %v, %v; want -3, 2", dx, dy)
	}
	if got := ShiftFromDisplacement(1.5, -0.5); got != (Shift{Row: -0.5, Column: -1.5}) {
		t.Fatalf("ShiftFromDisplacement() = %+v", got)
	}
	if got := (Shift{Row: 1, Column: 2}).Add(Shift{Row: 0.5, Column: -3}); got != (Shift{Row: 1.5, Column: -1}) {
		t.Fatalf("Add() = %+v", got)
	}

	const columns, rows = 7, 5
	impulse := make([]float64, columns*rows)
	impulse[2*columns+4] = 100
	tests := []struct {
		shift  Shift
		x, y   int
		reason string
	}{
		{Shift{Column: 1}, 3, 2, "positive column offset moves left"},
		{Shift{Column: -2}, 6, 2, "negative column offset moves right"},
		{Shift{Row: 1}, 4, 3, "positive row offset moves down"},
		{Shift{Row: -2}, 4, 0, "negative row offset moves up"},
	}
	for _, tt := range tests {
		out, err := ApplyShift(impulse, columns, rows, tt.shift)
		if err != nil {
			t.Fatalf("ApplyShift(%+v) error = %v", tt.shift, err)
		}
		if out[tt.y*columns+tt.x] != 100 || out[2*columns+4] != 0 {
			t.Fatalf("%s: ApplyShift(%+v) peak not at (%d,%d)", tt.reason, tt.shift, tt.x, tt.y)
		}
	}
}

func TestApplyShiftAnalyticPattern(t *testing.T) {
	// A linear ramp is reproduced exactly by bilinear interpolation, so the
	// shifted frame must equal the ramp evaluated at the source position.
	const columns, rows = 9, 7
	ramp := func(x, y float64) float64 { return 3*x + 5*y + 7 }
	mask := make([]float64, columns*rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			mask[y*columns+x] = ramp(float64(x), float64(y))
		}
	}
	shift := Shift{Row: 0.25, Column: 1.5}
	out, err := ApplyShift(mask, columns, rows, shift)
	if err != nil {
		t.Fatalf("ApplyShift() error = %v", err)
	}
	for y := 1; y < rows; y++ {
		for x := 0; x < columns-2; x++ {
			// Mask moves 1.5 px left and 0.25 px down: out(x,y) = mask(x+1.5, y-0.25).
			want := ramp(float64(x)+1.5, float64(y)-0.25)
			if got := out[y*columns+x]; math.Abs(got-want) > 1e-9 {
				t.Fatalf("out(%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
	// Samples shifted in from outside repeat the edge.
	if got, want := out[0*columns+columns-1], ramp(columns-1, 0); math.Abs(got-want) > 1e-9 {
		t.Fatalf("edge sample = %v, want the replicated edge %v", got, want)
	}
	if same, err := ApplyShift(mask, columns, rows, Shift{}); err != nil || &same[0] != &mask[0] {
		t.Fatal("zero shift must return the mask itself")
	}
	if _, err := ApplyShift(mask, columns, rows+1, shift); !errors.Is(err, ErrFrameSizeMismatch) {
		t.Fatalf("size mismatch error = %v", err)
	}
	if _, err := ApplyShift(mask, columns, rows, Shift{Row: math.Inf(1)}); !errors.Is(err, ErrInvalidPixelShift) {
		t.Fatalf("infinite shift error = %v", err)
	}
}

func TestAverageFrames(t *testing.T) {
	got, err := AverageFrames([]float64{1, 2, 3}, []float64{3, 4, 5}, []float64{5, 6, 13})
	if err != nil || !reflect.DeepEqual(got, []float64{3, 4, 7}) {
		t.Fatalf("AverageFrames() = %v, %v; want [3 4 7]", got, err)
	}
	single := []float64{1, 2}
	if got, err := AverageFrames(single); err != nil || &got[0] != &single[0] {
		t.Fatal("AverageFrames of one frame must return it")
	}
	if _, err := AverageFrames(); !errors.Is(err, ErrNoFrames) {
		t.Fatalf("AverageFrames() error = %v, want ErrNoFrames", err)
	}
	if _, err := AverageFrames([]float64{1}, []float64{1, 2}); !errors.Is(err, ErrFrameSizeMismatch) {
		t.Fatalf("AverageFrames(mismatch) error = %v, want ErrFrameSizeMismatch", err)
	}
}

func TestMaskSubtractionItemAppliesToFrame(t *testing.T) {
	item := withRange(avgSub(1), 2, 4, 8, 9)
	for frame, want := range map[int]bool{1: false, 2: true, 4: true, 5: false, 8: true, 10: false} {
		if got := item.AppliesToFrame(frame); got != want {
			t.Errorf("AppliesToFrame(%d) = %v, want %v", frame, got, want)
		}
	}
	if !(MaskSubtractionItem{}).AppliesToFrame(100) {
		t.Error("an empty range must apply to every frame")
	}
}
