package iod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func fgTestElement(n uint32, vr core.VR, v core.Value) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: fgTag(n), VR: vr}, Value: v}
}
func fgTestSQ(n uint32, items ...core.DataSet) core.Element {
	return fgTestElement(n, core.VRSQ, core.SequenceValue{Items: items})
}
func fgTestObject(profile string, frames int) *object.Object {
	root := object.FromElements([]core.Element{fgTestElement(0x00280008, core.VRIS, core.StringValue{fmt.Sprint(frames)}), fgTestElement(0x00080008, core.VRCS, core.StringValue{"DERIVED", "PRIMARY"}), fgTestElement(0x00280004, core.VRCS, core.StringValue{"MONOCHROME2"}), fgTestElement(0x00620001, core.VRCS, core.StringValue{"BINARY"})}, nil)
	rules, _ := fgRules(profile)
	var shared []core.Element
	var per []core.DataSet
	for _, r := range rules {
		required, known := r.required(root, 0)
		if known && required && r.tag != fgFrameContent {
			shared = append(shared, fgTestSQ(uint32(r.tag.Group)<<16|uint32(r.tag.Element), core.DataSet{}))
		}
	}
	for i := 0; i < frames; i++ {
		var elements []core.Element
		if profile != "Segmentation" {
			elements = []core.Element{fgTestSQ(0x00209111, core.DataSet{})}
		}
		per = append(per, core.DataSet{Elements: elements})
	}
	root.Put(fgTestSQ(0x52009229, core.DataSet{Elements: shared}))
	root.Put(fgTestSQ(0x52009230, per...))
	return root
}
func TestFunctionalGroupsProfilesValidAndMissing(t *testing.T) {
	for _, profile := range []string{"Enhanced CT Image", "Enhanced MR Image", "Segmentation"} {
		t.Run(profile, func(t *testing.T) {
			obj := fgTestObject(profile, 2)
			r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: profile})
			if err != nil || len(r.Errors()) != 0 {
				t.Fatalf("valid: %+v %v", r.Errors(), err)
			}
			shared, _ := obj.GetSequence(fgShared)
			shared[0].Put(fgTestSQ(0x00209111, core.DataSet{}))
			obj.Put(fgTestSQ(0x52009229, shared[0].ToDataSet()))
			r, err = ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: profile})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range r.Findings {
				if f.Rule == "frame-content-location" {
					found = true
					if f.Source == nil || f.Source.Edition != "PS3.3 2026d" || len(f.Source.SHA256) != 64 {
						t.Fatal("source")
					}
				}
			}
			if !found {
				t.Fatal("Shared Frame Content accepted")
			}
		})
	}
}
func TestFunctionalGroupsCardinalityAndFrameCount(t *testing.T) {
	for _, test := range []struct {
		name  string
		tag   uint32
		items []core.DataSet
	}{{"empty shared", 0x52009229, nil}, {"two shared", 0x52009229, []core.DataSet{{}, {}}}, {"short frames", 0x52009230, []core.DataSet{{}}}, {"long frames", 0x52009230, []core.DataSet{{}, {}, {}}}} {
		t.Run(test.name, func(t *testing.T) {
			obj := fgTestObject("Enhanced CT Image", 2)
			obj.Put(fgTestSQ(test.tag, test.items...))
			r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
			if err != nil || len(r.Errors()) != 1 {
				t.Fatalf("%v %+v", err, r.Errors())
			}
		})
	}
}
func TestFunctionalGroupsConflictUsesExistingResolverExactlyOnce(t *testing.T) {
	obj := fgTestObject("Enhanced MR Image", 2)
	items, _ := obj.GetSequence(fgPerFrame)
	items[0].Put(fgTestSQ(0x00289110, core.DataSet{}))
	obj.Put(fgTestSQ(0x52009230, items[0].ToDataSet(), items[1].ToDataSet()))
	r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced MR Image"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, f := range r.Findings {
		if f.Kind == FunctionalGroupConflict {
			count++
		}
	}
	if count != 1 || len(obj.FunctionalGroupConflicts()) != 1 {
		t.Fatal(count)
	}
}
func TestFunctionalGroupsRawDuplicateBeforeLastWins(t *testing.T) {
	obj := fgTestObject("Enhanced CT Image", 1)
	ds := obj.ToDataSet()
	ds.Elements = append(ds.Elements, fgTestSQ(0x52009229, core.DataSet{}))
	r, err := ValidateFunctionalGroupDataSet(context.Background(), ds, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
	if err != nil || len(r.Errors()) != 1 || r.Errors()[0].Rule != "duplicate-tag" {
		t.Fatalf("%v %+v", err, r.Errors())
	}
}
func TestFunctionalGroupsNestedDuplicateAndWrongSequenceVR(t *testing.T) {
	obj := fgTestObject("Enhanced CT Image", 1)
	dup := fgTestSQ(0x00209111, core.DataSet{})
	obj.Put(fgTestSQ(0x52009230, core.DataSet{Elements: []core.Element{dup, dup}}))
	r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
	if err != nil || len(r.Errors()) != 1 || r.Errors()[0].Rule != "duplicate-tag" {
		t.Fatalf("%v %+v", err, r.Errors())
	}
	obj.Put(fgTestElement(0x52009230, core.VRLO, core.SequenceValue{Items: []core.DataSet{{}}}))
	r, err = ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
	if err != nil || len(r.Errors()) != 1 || r.Errors()[0].Rule != "sequence-vr" {
		t.Fatalf("%v %+v", err, r.Errors())
	}
}
func TestFunctionalGroupsLimitsAndCancellation(t *testing.T) {
	obj := fgTestObject("Enhanced CT Image", 2)
	for _, opts := range []FunctionalGroupOptions{{MaxFrames: 1}, {MaxElements: 1}, {MaxItems: 1}, {MaxOperations: 1}} {
		opts.IOD = "Enhanced CT Image"
		_, err := ValidateFunctionalGroups(context.Background(), obj, opts)
		if !errors.Is(err, ErrFunctionalGroupLimit) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ValidateFunctionalGroups(ctx, obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err = ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image", MaxFrames: -1})
	if !errors.Is(err, ErrFunctionalGroupOptions) {
		t.Fatal(err)
	}
}
func TestFunctionalGroupsSegLabelmapOmittedPerFrame(t *testing.T) {
	obj := fgTestObject("Segmentation", 1)
	obj.Put(fgTestElement(0x00620001, core.VRCS, core.StringValue{"LABELMAP"}))
	elements := obj.Elements()
	var keep []core.Element
	for _, e := range elements {
		if e.Tag() != fgPerFrame {
			keep = append(keep, e)
		}
	}
	obj = object.FromElements(keep, nil)
	r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Segmentation"})
	if err != nil || len(r.Errors()) != 0 {
		t.Fatalf("%v %+v", err, r.Errors())
	}
}
func TestFunctionalGroupsDimensionPointerResolution(t *testing.T) {
	obj := fgTestObject("Enhanced CT Image", 1)
	obj.Put(fgTestSQ(0x00209222, core.DataSet{Elements: []core.Element{fgTestElement(0x00209165, core.VRAT, core.TagValue{fgTag(0x00209057)}), fgTestElement(0x00209167, core.VRAT, core.TagValue{fgFrameContent})}}))
	r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range r.Findings {
		if f.Rule == "dimension-pointer-absent" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing-value ordinal context not recorded")
	}
}

func TestFunctionalGroupsRequiredMacroAndProfileLocation(t *testing.T) {
	for _, profile := range []string{"Enhanced CT Image", "Enhanced MR Image", "Segmentation"} {
		t.Run(profile, func(t *testing.T) {
			obj := fgTestObject(profile, 2)
			macro := fgTag(0x00289110)
			if profile == "Segmentation" {
				macro = fgTag(0x0062000A)
			}
			shared, _ := obj.GetSequence(fgShared)
			var keep []core.Element
			for _, e := range shared[0].Elements() {
				if e.Tag() != macro {
					keep = append(keep, e)
				}
			}
			obj.Put(fgTestSQ(0x52009229, core.DataSet{Elements: keep}))
			r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: profile})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, f := range r.Findings {
				if f.Kind == MissingFunctionalGroup && f.Tag == macro {
					count++
				}
			}
			if count != 2 {
				t.Fatalf("missing per frame %d", count)
			}
		})
	}
	obj := fgTestObject("Enhanced CT Image", 1)
	per, _ := obj.GetSequence(fgPerFrame)
	per[0].Put(fgTestSQ(0x00189226, core.DataSet{}))
	obj.Put(fgTestSQ(0x52009230, per[0].ToDataSet()))
	r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range r.Findings {
		if f.Rule == "macro-profile-location" {
			found = true
		}
	}
	if !found {
		t.Fatal("MR macro accepted as permitted CT macro")
	}
}
func TestFunctionalGroupsEmptyType2DerivationIsNotAbsent(t *testing.T) {
	obj := fgTestObject("Enhanced CT Image", 1)
	shared, _ := obj.GetSequence(fgShared)
	shared[0].Put(fgTestSQ(0x00089124))
	obj.Put(fgTestSQ(0x52009229, shared[0].ToDataSet()))
	r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Errors() {
		if f.Tag == fgTag(0x00089124) {
			t.Fatalf("Type2 empty macro treated as absent: %+v", f)
		}
	}
}
func TestFunctionalGroupsFindingsBoundAndBulkDataUnresolved(t *testing.T) {
	obj := fgTestObject("Enhanced CT Image", 1)
	obj.Put(fgTestElement(0x7FE00010, core.VROB, core.BulkDataValue{URI: "never-resolve://synthetic-pixel"}))
	r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image", MaxFindings: 1})
	if err != nil || len(r.Findings) > 1 {
		t.Fatalf("%v %+v", err, r)
	}
	e, _ := obj.Get(fgTag(0x7FE00010))
	if _, ok := e.Value.(core.BulkDataValue); !ok {
		t.Fatal("bulk data changed")
	}
}
func TestFunctionalGroupsMRDiffusionDatasetCondition(t *testing.T) {
	obj := fgTestObject("Enhanced MR Image", 1)
	obj.Put(fgTestElement(0x00080008, core.VRCS, core.StringValue{"ORIGINAL", "PRIMARY", "VOLUME", "NONE"}))
	shared, _ := obj.GetSequence(fgShared)
	shared[0].Put(fgTestSQ(0x00189226, core.DataSet{Elements: []core.Element{fgTestElement(0x00089209, core.VRCS, core.StringValue{"DIFFUSION"})}}))
	obj.Put(fgTestSQ(0x52009229, shared[0].ToDataSet()))
	r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced MR Image"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range r.Findings {
		if f.Tag == fgTag(0x00189117) && f.Kind == MissingFunctionalGroup {
			found = true
		}
	}
	if !found {
		t.Fatal("dataset-decidable MR diffusion incorrectly manual")
	}
}

func TestFunctionalGroupsActualValueBudgetAndUnknownType(t *testing.T) {
	for _, value := range []core.Value{core.RawValue(make([]byte, MaxConstraintValueBytes+1)), core.StringValue{strings.Repeat("1", MaxConstraintValueBytes+1)}, fgHostileValue{}} {
		obj := fgTestObject("Enhanced CT Image", 1)
		e := fgTestElement(0x00280008, core.VRIS, value)
		e.Header.LengthSet = true
		e.Header.Length = 1
		obj.Put(e)
		_, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
		if !errors.Is(err, ErrFunctionalGroupLimit) && !errors.Is(err, ErrFunctionalGroupOptions) {
			t.Fatalf("unadmitted value reached decoder: %T %v", value, err)
		}
	}
}

type fgHostileValue struct{}

func (fgHostileValue) Kind() core.ValueKind               { panic("custom Kind must not be called") }
func (fgHostileValue) EncodedLength() (core.Length, bool) { panic("custom length must not be called") }
func TestFunctionalGroupsSEGFrameOfReferenceGeometry(t *testing.T) {
	obj := fgTestObject("Segmentation", 1)
	obj.Put(fgTestElement(0x00200052, core.VRUI, core.StringValue{"1.2.826.0.1.3680043.10.999.1"}))
	r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Segmentation"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range r.Errors() {
		if f.Rule == "seg-frame-of-reference-geometry" {
			found = true
		}
	}
	if !found {
		t.Fatal("dataset-decidable FOR geometry incorrectly manual")
	}
}
func TestFunctionalGroupsMissingOrdinalCannotCollideWithPresent(t *testing.T) {
	for _, missingFirst := range []bool{true, false} {
		obj := fgTestObject("Enhanced CT Image", 2)
		frames, _ := obj.GetSequence(fgPerFrame)
		for i, f := range frames {
			content := []core.Element{fgTestElement(0x00209157, core.VRUL, core.Uint32Value{1})}
			if (i == 0) != missingFirst {
				content = append(content, fgTestElement(0x00209057, core.VRUL, core.Uint32Value{3}))
			}
			f.Put(fgTestSQ(0x00209111, core.DataSet{Elements: content}))
		}
		obj.Put(fgTestSQ(0x52009230, frames[0].ToDataSet(), frames[1].ToDataSet()))
		obj.Put(fgTestSQ(0x00209222, core.DataSet{Elements: []core.Element{fgTestElement(0x00209165, core.VRAT, core.TagValue{fgTag(0x00209057)}), fgTestElement(0x00209167, core.VRAT, core.TagValue{fgFrameContent})}}))
		r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range r.Errors() {
			if f.Rule == "dimension-missing-ordinal-collision" {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing/present ordinal collision accepted: missingFirst=%v", missingFirst)
		}
	}
}

func TestFunctionalGroupsSEGGeometryBranches(t *testing.T) {
	for _, name := range []string{"patient", "slide", "implicit-slide", "tiled-alone", "invalid-origin"} {
		t.Run(name, func(t *testing.T) {
			obj := fgTestObject("Segmentation", 1)
			obj.Put(fgTestElement(0x00200052, core.VRUI, core.StringValue{"1.2.826.0.1.3680043.10.999.1"}))
			shared, _ := obj.GetSequence(fgShared)
			shared[0].Put(fgTestSQ(0x00289110, core.DataSet{}))
			switch name {
			case "patient":
				shared[0].Put(fgTestSQ(0x00209113, core.DataSet{}))
				shared[0].Put(fgTestSQ(0x00209116, core.DataSet{}))
			case "slide":
				shared[0].Put(fgTestSQ(0x0048021A, core.DataSet{}))
			default:
				obj.Put(fgTestElement(0x00209311, core.VRCS, core.StringValue{"TILED_FULL"}))
				obj.Put(fgTestElement(0x00620001, core.VRCS, core.StringValue{"LABELMAP"}))
				if name != "tiled-alone" {
					obj.Put(fgTestElement(0x00480102, core.VRDS, core.StringValue{"1", "0", "0", "0", "1", "0"}))
					if name == "invalid-origin" {
						obj.Put(fgTestSQ(0x00480008))
					} else {
						obj.Put(fgTestSQ(0x00480008, core.DataSet{}))
					}
				}
			}
			obj.Put(fgTestSQ(0x52009229, shared[0].ToDataSet()))
			r, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Segmentation"})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range r.Errors() {
				if f.Rule == "seg-frame-of-reference-geometry" {
					found = true
				}
			}
			want := name == "tiled-alone" || name == "invalid-origin"
			if found != want {
				t.Fatalf("geometry finding=%v want=%v: %+v", found, want, r.Errors())
			}
		})
	}
}
func TestFunctionalGroupsIndexedEmptinessIgnoresHeader(t *testing.T) {
	for _, value := range []core.Value{core.Uint32Value{3}, core.RawValue{3, 0, 0, 0}, core.StringValue{"3"}} {
		e := fgTestElement(0x00209057, core.VRUL, value)
		e.Header.LengthSet = true
		e.Header.Length = 0
		if fgIndexedEmpty(e) {
			t.Fatalf("nonempty actual payload masked by header: %T", value)
		}
	}
	e := fgTestElement(0x00209057, core.VRUL, core.Uint32Value{})
	e.Header.LengthSet = true
	e.Header.Length = 4
	if !fgIndexedEmpty(e) {
		t.Fatal("empty actual payload masked by header")
	}
}

// Matrix controls below qualify top-level FG structure only. Nonempty synthetic
// macro items avoid mistaking absent items for valid content; the separate peer
// corpus qualifies the supported common inner attributes through getters.
func fgMatrixItem(tag uint32) core.DataSet {
	fields := map[uint32][]core.Element{
		0x00289110: {fgTestElement(0x00280030, core.VRDS, core.StringValue{"1", "1"}), fgTestElement(0x00180050, core.VRDS, core.StringValue{"1"})},
		0x00209111: {fgTestElement(0x00209156, core.VRUS, core.Uint16Value{1})},
		0x00209113: {fgTestElement(0x00200032, core.VRDS, core.StringValue{"0", "0", "0"})},
		0x00209116: {fgTestElement(0x00200037, core.VRDS, core.StringValue{"1", "0", "0", "0", "1", "0"})},
		0x00209071: {fgTestElement(0x00209072, core.VRCS, core.StringValue{"U"})},
		0x00189301: {fgTestElement(0x00189302, core.VRCS, core.StringValue{"SPIRAL"})},
		0x00189329: {fgTestElement(0x00089007, core.VRCS, core.StringValue{"DERIVED", "PRIMARY", "VOLUME", "NONE"})},
		0x00189226: {fgTestElement(0x00089007, core.VRCS, core.StringValue{"DERIVED", "PRIMARY", "VOLUME", "NONE"}), fgTestElement(0x00089209, core.VRCS, core.StringValue{"T1"})},
		0x00189006: {fgTestElement(0x00189027, core.VRCS, core.StringValue{"NONE"})},
		0x00289145: {fgTestElement(0x00281052, core.VRDS, core.StringValue{"0"}), fgTestElement(0x00281053, core.VRDS, core.StringValue{"1"}), fgTestElement(0x00281054, core.VRLO, core.StringValue{"US"})},
		0x0062000A: {fgTestElement(0x0062000B, core.VRUS, core.Uint16Value{1})},
		0x00189341: {fgTestElement(0x00189337, core.VRUS, core.Uint16Value{1})},
		0x00189042: {fgTestElement(0x00181250, core.VRSH, core.StringValue{"SYNTHETIC"}), fgTestElement(0x00189043, core.VRCS, core.StringValue{"BODY"})},
		0x00189197: {fgTestElement(0x00189090, core.VRFD, core.Float64Value{1, 0, 0})},
		0x00189251: {fgTestElement(0x00189252, core.VRLO, core.StringValue{"SYNTHETIC"})},
		0x00189360: {fgTestElement(0x00180060, core.VRDS, core.StringValue{"120"})},
		0x00189107: {fgTestElement(0x00189104, core.VRFD, core.Float64Value{1})},
		0x00081140: {fgTestElement(0x00081150, core.VRUI, core.StringValue{"1.2.840.10008.5.1.4.1.1.2"}), fgTestElement(0x00081155, core.VRUI, core.StringValue{"1.2.826.0.1.3680043.10.999.10"})},
		0x00089124: {fgTestElement(0x00082111, core.VRST, core.StringValue{"Synthetic derivation"})},
		0x00409096: {fgTestElement(0x00409210, core.VRSH, core.StringValue{"SYNTHETIC"}), fgTestElement(0x00409224, core.VRFD, core.Float64Value{0}), fgTestElement(0x00409225, core.VRFD, core.Float64Value{1})},
	}
	if e, ok := fields[tag]; ok {
		return core.DataSet{Elements: e}
	}
	// Other profile macros are cardinality-only controls: a materialized item
	// with a harmless optional acquisition number, never an inner-attribute pass.
	return core.DataSet{Elements: []core.Element{fgTestElement(0x00200012, core.VRIS, core.StringValue{"1"})}}
}
func fgMatrixPut(obj *object.Object, tag uint32, count int) {
	items := make([]core.DataSet, count)
	for i := range items {
		items[i] = fgMatrixItem(tag)
	}
	if tag == 0x00209111 {
		per, _ := obj.GetSequence(fgPerFrame)
		for _, frame := range per {
			frame.Put(fgTestSQ(tag, items...))
		}
		ds := make([]core.DataSet, len(per))
		for i := range per {
			ds[i] = per[i].ToDataSet()
		}
		obj.Put(fgTestSQ(0x52009230, ds...))
		return
	}
	shared, _ := obj.GetSequence(fgShared)
	shared[0].Put(fgTestSQ(tag, items...))
	obj.Put(fgTestSQ(0x52009229, shared[0].ToDataSet()))
}
func fgMatrixRemove(obj *object.Object, tag uint32) {
	for _, parent := range []core.Tag{fgShared, fgPerFrame} {
		seq, ok := obj.GetSequence(parent)
		if !ok {
			continue
		}
		ds := make([]core.DataSet, len(seq))
		for i := range seq {
			for _, e := range seq[i].Elements() {
				if e.Tag() != fgTag(tag) {
					ds[i].Elements = append(ds[i].Elements, e)
				}
			}
		}
		obj.Put(fgTestSQ(uint32(parent.Group)<<16|uint32(parent.Element), ds...))
	}
}
func fgMatrixFind(r Report, tag uint32, kind Kind, rule string) []Finding {
	var found []Finding
	for _, f := range r.Findings {
		if f.Tag == fgTag(tag) && f.Kind == kind && (rule == "" || f.Rule == rule) {
			found = append(found, f)
		}
	}
	return found
}
func TestFunctionalGroupsNormativeCardinalityMatrix(t *testing.T) {
	// Cardinalities independently bound to PS3.3 top-level macro rows: zero-or-
	// more references/derivation/spatial bands; one-or-more RWVM/contrast/coils/
	// velocity/ASL/additional source; CT four multi-energy sequences conditional.
	zero := map[uint32]bool{0x00081140: true, 0x00089124: true, 0x00189107: true}
	multi := map[uint32]bool{0x00409096: true, 0x00189341: true, 0x00189042: true, 0x00189197: true, 0x00189251: true, 0x00189360: true}
	dynamic := map[uint32]bool{0x00189304: true, 0x00189312: true, 0x00189321: true, 0x00189325: true}
	for _, profile := range []string{"Enhanced CT Image", "Enhanced MR Image", "Segmentation"} {
		rules, _ := fgRules(profile)
		for _, rule := range rules {
			tag := uint32(rule.tag.Group)<<16 | uint32(rule.tag.Element)
			for _, count := range []int{0, 1, 2} {
				t.Run(fmt.Sprintf("%s/%08X/%d", profile, tag, count), func(t *testing.T) {
					obj := fgTestObject(profile, 2)
					fgMatrixPut(obj, tag, count)
					result, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: profile})
					if err != nil {
						t.Fatal(err)
					}
					want := (count == 0 && !zero[tag]) || (count == 2 && !zero[tag] && !multi[tag])
					got := fgMatrixFind(result, tag, InvalidFunctionalGroup, "macro-cardinality")
					if (len(got) > 0) != want {
						t.Fatalf("count %d want invalid %v findings %+v", count, want, result.Findings)
					}
					for _, f := range got {
						if f.Source == nil || len(f.Source.SHA256) != 64 {
							t.Fatal("missing normative binding")
						}
					}
					direct := map[uint32]struct{ section, sha string }{
						0x00189477: {"C.7.6.16.2.18", "7e6c176242d323b9295862bbade84ea42f4eb4b5f8a6076c93c0ea47410a83fe"},
						0x00189329: {"C.8.15.3.1", "f9e29df26e1ff74aca9b544a6a8a83d0b9aedec978bdd62403d270754f2a7e7b"},
						0x00289145: {"C.8.15.3.10", "82dc06ccc539c875c53316c635ae7cfa0a4e8bae68cfb53f9be2fd41275ac45f"},
					}
					if profile == "Enhanced CT Image" {
						if binding, ok := direct[tag]; ok {
							for _, f := range got {
								if f.Source.Section != binding.section || f.Source.SHA256 != binding.sha {
									t.Fatalf("wrong cardinality source %+v", f.Source)
								}
							}
						}
					}

				})
			}
			if dynamic[tag] {
				for _, value := range []string{"YES", "NO", "INVALID"} {
					t.Run(fmt.Sprintf("%s/%08X/energy-%s", profile, tag, value), func(t *testing.T) {
						obj := fgTestObject(profile, 2)
						obj.Put(fgTestElement(0x00189361, core.VRCS, core.StringValue{value}))
						fgMatrixPut(obj, tag, 2)
						result, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: profile})
						if err != nil {
							t.Fatal(err)
						}
						if (len(fgMatrixFind(result, tag, InvalidFunctionalGroup, "macro-cardinality")) > 0) != (value == "NO") {
							t.Fatalf("multi-energy %+v", result.Findings)
						}
						if (len(fgMatrixFind(result, tag, ManualFunctionalGroup, "manual-macro-cardinality")) > 0) != (value == "INVALID") {
							t.Fatal("unknown condition lost")
						}
					})
				}
			}
		}
	}
}
func TestFunctionalGroupsConditionalMatrix(t *testing.T) {
	type tc struct {
		name, profile    string
		tag              uint32
		root             []core.Element
		macros           map[uint32]core.DataSet
		required, manual bool
	}
	original := fgTestElement(0x00080008, core.VRCS, core.StringValue{"ORIGINAL", "PRIMARY", "VOLUME", "NONE"})
	cases := []tc{
		{"ct-original", "Enhanced CT Image", 0x00189301, []core.Element{original}, nil, true, false},
		{"ct-derived", "Enhanced CT Image", 0x00189301, nil, nil, false, false},
		{"ct-reconstruction", "Enhanced CT Image", 0x00189314, []core.Element{original}, map[uint32]core.DataSet{0x00189301: fgMatrixItem(0x00189301)}, true, false},
		{"ct-constant-angle", "Enhanced CT Image", 0x00189314, []core.Element{original}, map[uint32]core.DataSet{0x00189301: {Elements: []core.Element{fgTestElement(0x00189302, core.VRCS, core.StringValue{"CONSTANT_ANGLE"})}}}, false, false},
		{"ct-reconstruction-unknown", "Enhanced CT Image", 0x00189314, []core.Element{original}, nil, false, true},
		{"ct-energy-rwvm", "Enhanced CT Image", 0x00409096, []core.Element{fgTestElement(0x00189361, core.VRCS, core.StringValue{"YES"})}, nil, true, false},
		{"ct-noenergy-rwvm", "Enhanced CT Image", 0x00409096, []core.Element{fgTestElement(0x00189361, core.VRCS, core.StringValue{"NO"})}, nil, false, false},
		{"ct-unknownenergy-rwvm", "Enhanced CT Image", 0x00409096, []core.Element{fgTestElement(0x00189361, core.VRCS, core.StringValue{"INVALID"})}, nil, false, true},
		{"mr-original", "Enhanced MR Image", 0x00189112, []core.Element{original}, nil, true, false},
		{"mr-derived", "Enhanced MR Image", 0x00189112, nil, nil, false, false},
		{"mr-mono", "Enhanced MR Image", 0x00289145, nil, nil, true, false},
		{"mr-color", "Enhanced MR Image", 0x00289145, []core.Element{fgTestElement(0x00280004, core.VRCS, core.StringValue{"RGB"})}, nil, false, false},
		{"mr-metabolite", "Enhanced MR Image", 0x00189152, []core.Element{fgTestElement(0x00080008, core.VRCS, core.StringValue{"DERIVED", "PRIMARY", "METABOLITE_MAP", "NONE"})}, nil, true, false},
		{"mr-asl", "Enhanced MR Image", 0x00189251, []core.Element{fgTestElement(0x00080008, core.VRCS, core.StringValue{"DERIVED", "PRIMARY", "ASL", "NONE"})}, nil, true, false},
		{"mr-diffusion", "Enhanced MR Image", 0x00189117, []core.Element{original}, map[uint32]core.DataSet{0x00189226: {Elements: []core.Element{fgTestElement(0x00089209, core.VRCS, core.StringValue{"DIFFUSION"})}}}, true, false},
		{"mr-nondiffusion", "Enhanced MR Image", 0x00189117, []core.Element{original}, map[uint32]core.DataSet{0x00189226: {Elements: []core.Element{fgTestElement(0x00089209, core.VRCS, core.StringValue{"T1"})}}}, false, false},
		{"mr-diffusion-unknown", "Enhanced MR Image", 0x00189117, []core.Element{original}, nil, false, true},
		{"mr-slab", "Enhanced MR Image", 0x00189107, []core.Element{original}, map[uint32]core.DataSet{0x00189006: {Elements: []core.Element{fgTestElement(0x00189027, core.VRCS, core.StringValue{"SLAB"})}}}, true, false},
		{"mr-noslab", "Enhanced MR Image", 0x00189107, []core.Element{original}, map[uint32]core.DataSet{0x00189006: fgMatrixItem(0x00189006)}, false, false},
		{"seg-binary", "Segmentation", 0x0062000A, nil, nil, true, false},
		{"seg-fractional", "Segmentation", 0x0062000A, []core.Element{fgTestElement(0x00620001, core.VRCS, core.StringValue{"FRACTIONAL"})}, nil, true, false},
		{"seg-labelmap", "Segmentation", 0x0062000A, []core.Element{fgTestElement(0x00620001, core.VRCS, core.StringValue{"LABELMAP"})}, nil, false, false},
		{"seg-tiled", "Segmentation", 0x0062000A, []core.Element{fgTestElement(0x00209311, core.VRCS, core.StringValue{"TILED_FULL"})}, nil, false, false},
		{"seg-no-for-derivation", "Segmentation", 0x00089124, nil, nil, true, false},
		{"seg-for-derivation-context", "Segmentation", 0x00089124, []core.Element{fgTestElement(0x00200052, core.VRUI, core.StringValue{"1.2.826.0.1.3680043.10.999.11"})}, nil, false, true},
	}
	for _, profile := range []string{"Enhanced CT Image", "Enhanced MR Image"} {
		for _, c := range []struct {
			name         string
			tag, trigger uint32
			yes, no      string
		}{
			{"cardiac", 0x00189118, 0x00189037, "RETROSPECTIVE", "NONE"},
			{"respiratory", 0x00209253, 0x00189170, "RETROSPECTIVE", "BREATH_HOLD"},
		} {
			for _, value := range []string{c.yes, c.no, ""} {
				root := []core.Element{original}
				if value != "" {
					root = append(root, fgTestElement(c.trigger, core.VRCS, core.StringValue{value}))
				}
				cases = append(cases, tc{profile + "/" + c.name + "/" + value, profile, c.tag, root, nil, value == c.yes, value == ""})
			}
		}
		cases = append(cases, tc{profile + "/contrast", profile, 0x00189341, []core.Element{fgTestSQ(0x00180012, core.DataSet{Elements: []core.Element{fgTestElement(0x00189337, core.VRUS, core.Uint16Value{1})}})}, nil, true, false})
	}
	for _, c := range []struct {
		name         string
		tag, trigger uint32
		yes, no      string
	}{{"kspace", 0x00189125, 0x00189032, "RECTILINEAR", "RADIAL"}, {"velocity", 0x00189197, 0x00189014, "YES", "NO"}} {
		for _, value := range []string{c.yes, c.no, ""} {
			root := []core.Element{original}
			if value != "" {
				root = append(root, fgTestElement(c.trigger, core.VRCS, core.StringValue{value}))
			}
			cases = append(cases, tc{c.name + "/" + value, "Enhanced MR Image", c.tag, root, nil, value == c.yes, value == ""})
		}
	}

	for _, profile := range []string{"Enhanced CT Image", "Enhanced MR Image"} {
		tags := []uint32{0x00189301, 0x00189304, 0x00189308, 0x00189326, 0x00189312, 0x00189321, 0x00189325}
		mandatory := []uint32{0x00189477, 0x00189329, 0x00289145}
		if profile == "Enhanced MR Image" {
			tags = []uint32{0x00189112, 0x00189114, 0x00189115, 0x00189006, 0x00189042, 0x00189049, 0x00189119}
			mandatory = []uint32{0x00189226}
		}
		for _, tag := range tags {
			for _, value := range []string{"ORIGINAL", "MIXED", "DERIVED"} {
				cases = append(cases, tc{fmt.Sprintf("%s/%08X/%s", profile, tag, value), profile, tag, []core.Element{fgTestElement(0x00080008, core.VRCS, core.StringValue{value, "PRIMARY", "VOLUME", "NONE"})}, nil, value != "DERIVED", false})
			}
		}
		for _, tag := range append(mandatory, 0x00289110, 0x00209111, 0x00209113, 0x00209116, 0x00209071) {
			cases = append(cases, tc{fmt.Sprintf("%s/%08X/mandatory", profile, tag), profile, tag, nil, nil, true, false})
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := fgTestObject(c.profile, 2)
			for _, e := range c.root {
				obj.Put(e)
			}
			for tag, item := range c.macros {
				fgMatrixPut(obj, tag, 1)
				shared, _ := obj.GetSequence(fgShared)
				shared[0].Put(fgTestSQ(tag, item))
				obj.Put(fgTestSQ(0x52009229, shared[0].ToDataSet()))
			}
			fgMatrixRemove(obj, c.tag)
			result, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: c.profile})
			if err != nil {
				t.Fatal(err)
			}
			missing := fgMatrixFind(result, c.tag, MissingFunctionalGroup, "required-macro")
			manual := fgMatrixFind(result, c.tag, ManualFunctionalGroup, "manual-macro-condition")
			if (len(missing) > 0) != c.required || (len(manual) > 0) != c.manual {
				t.Fatalf("required=%v manual=%v %+v", c.required, c.manual, result.Findings)
			}
			// Unknown conditions produce only manual findings; decidable required
			// conditions produce only missing findings, exactly once per frame.
			for _, check := range []struct {
				findings []Finding
				want     bool
			}{{missing, c.required}, {manual, c.manual}} {
				wantCount := 0
				if check.want {
					wantCount = 2
				}
				if len(check.findings) != wantCount {
					t.Fatalf("conditional finding count=%d want=%d", len(check.findings), wantCount)
				}
				for frame, finding := range check.findings {
					if finding.Path != fgPath(frame, fgTag(c.tag)) {
						t.Fatalf("conditional path=%q want=%q", finding.Path, fgPath(frame, fgTag(c.tag)))
					}
				}
			}
			fgMatrixPut(obj, c.tag, 1)
			result, err = ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: c.profile})
			if err != nil {
				t.Fatal(err)
			}
			if len(fgMatrixFind(result, c.tag, MissingFunctionalGroup, "")) > 0 || len(fgMatrixFind(result, c.tag, ManualFunctionalGroup, "manual-macro-condition")) > 0 {
				t.Fatal("present macro still absent/manual")
			}
		})
	}
}
func TestFunctionalGroupsForbiddenEmptyAndConflictNoContradiction(t *testing.T) {
	for _, tag := range []uint32{0x00289132, 0x00409096} {
		for _, count := range []int{0, 1} {
			obj := fgTestObject("Enhanced MR Image", 2)
			obj.Put(fgTestElement(0x00280004, core.VRCS, core.StringValue{"RGB"}))
			fgMatrixPut(obj, tag, count)
			result, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced MR Image"})
			if err != nil {
				t.Fatal(err)
			}
			if len(fgMatrixFind(result, tag, InvalidFunctionalGroup, "macro-forbidden")) != 1 {
				t.Fatal("shared forbidden finding missing or duplicated")
			}
		}
	}
	obj := fgTestObject("Enhanced CT Image", 2)
	fgMatrixPut(obj, 0x00289110, 1)
	per, _ := obj.GetSequence(fgPerFrame)
	per[0].Put(fgTestSQ(0x00289110, fgMatrixItem(0x00289110)))
	obj.Put(fgTestSQ(0x52009230, per[0].ToDataSet(), per[1].ToDataSet()))
	result, err := ValidateFunctionalGroups(context.Background(), obj, FunctionalGroupOptions{IOD: "Enhanced CT Image"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fgMatrixFind(result, 0x00289110, FunctionalGroupConflict, "shared-per-frame-conflict")) != 1 {
		t.Fatal("conflict not unique")
	}
	if len(fgMatrixFind(result, 0x00289110, InvalidFunctionalGroup, "macro-cardinality")) > 0 {
		t.Fatal("conflict produced contradictory empty cardinality")
	}
}
