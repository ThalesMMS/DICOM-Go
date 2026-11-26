package attrpath

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestRecursiveEditExistingOnlyAndSQPruning(t *testing.T) {
	o := dcsessObject()
	for _, path := range []string{"ReferencedStudySequence[0].PatientName", "ReferencedStudySequence[1].ReferencedStudySequence[0].PatientName"} {
		if _, e := Insert(o, MustParse(path), "OLD", Options{}); e != nil {
			t.Fatal(e)
		}
	}
	s, e := NewEditSession(context.Background(), o, EditSessionOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if n, e := s.ModifyAll(MustParse("PatientName")[0], "NEW"); e != nil || n != 3 {
		t.Fatalf("%d %v", n, e)
	}
	if n, e := s.RemoveAll(MustParse("ReferencedStudySequence")[0]); e != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, e)
	}
	if o.Has(core.NewTag(0x0008, 0x1110)) {
		t.Fatal("sequence remains")
	}
	if n, e := s.ModifyAll(MustParse("PatientID")[0], "NEW"); n != 0 || !errors.Is(e, ErrNotFound) {
		t.Fatalf("%d %v", n, e)
	}
}
func TestRecursiveSelectionBudgetBeforeMutation(t *testing.T) {
	o := object.New(std.Dictionary)
	for _, path := range []string{"ReferencedStudySequence[0].PatientName", "ReferencedStudySequence[1].PatientName"} {
		if _, e := Insert(o, MustParse(path), "OLD", Options{}); e != nil {
			t.Fatal(e)
		}
	}
	s, e := NewEditSession(context.Background(), o, EditSessionOptions{Limits: EditLimits{MaxMatches: 1}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ModifyAll(MustParse("PatientName")[0], "NEW"); !errors.Is(e, ErrLimit) {
		t.Fatal(e)
	}
	items, _ := o.GetSequence(core.NewTag(0x0008, 0x1110))
	for _, item := range items {
		v, _ := item.GetString(core.NewTag(0x0010, 0x0010))
		if v != "OLD" {
			t.Fatal(v)
		}
	}
	if _, e = s.RemoveAll(MustParse("ReferencedStudySequence[0]")[0]); !errors.Is(e, ErrSyntax) {
		t.Fatal(e)
	}
}

func dcrecPrivateModel() *object.Object {
	leaf := func(tag core.Tag, vr core.VR, value string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
	}
	return object.FromElements([]core.Element{
		leaf(core.NewTag(0x0011, 0x0010), core.VRLO, "CREATOR"),
		leaf(core.NewTag(0x0011, 0x1010), core.VRLO, "PRIVATE"),
		leaf(core.NewTag(0x7776, 0x1010), core.VRLO, "PUBLIC_UNKNOWN"),
		leaf(core.NewTag(0x0002, 0x0100), core.VRUI, "1.2.3"),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			leaf(core.NewTag(0x0011, 0x0010), core.VRLO, "CREATOR"),
			leaf(core.NewTag(0x0011, 0x1010), core.VRLO, "NESTED_PRIVATE"),
			leaf(core.NewTag(0x0010, 0x0010), core.VRPN, "PUBLIC"),
		}}}}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1020), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{leaf(core.NewTag(0x0010, 0x0010), core.VRPN, "REMOVED_WITH_SQ")}}}}},
	}, std.Dictionary)
}

func TestErasePrivateUsesAtomicStripAPIAndSessionBudgets(t *testing.T) {
	o := dcrecPrivateModel()
	s, err := NewEditSession(context.Background(), o, EditSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ErasePrivate(); err != nil || n != 5 {
		t.Fatalf("private removal %d %v", n, err)
	}
	if o.Has(core.NewTag(0x0011, 0x0010)) || o.Has(core.NewTag(0x0011, 0x1020)) || !o.Has(core.NewTag(0x7776, 0x1010)) || !o.Has(core.NewTag(0x0002, 0x0100)) {
		t.Fatal("private/public classification differs")
	}
	items, _ := o.GetSequence(core.NewTag(0x0008, 0x1110))
	if len(items) != 1 || items[0].Len() != 1 || !items[0].Has(core.NewTag(0x0010, 0x0010)) {
		t.Fatal("nested public value not retained")
	}
	if n, err := s.ErasePrivate(); err != nil || n != 0 {
		t.Fatalf("no-private no-op %d %v", n, err)
	}
	for _, limits := range []EditLimits{{MaxTouches: 1}, {MaxVisits: 26}} {
		o = dcrecPrivateModel()
		s, err = NewEditSession(context.Background(), o, EditSessionOptions{Limits: limits})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ErasePrivate(); !errors.Is(err, ErrLimit) || !o.Has(core.NewTag(0x0011, 0x1020)) {
			t.Fatalf("helper ran before aggregate admission: %v", err)
		}
	}
}

func TestRecursivePrivateCreatorResolvesLocalBlocks(t *testing.T) {
	creator := "SIEMENS CSA HEADER"
	leaf := func(tag core.Tag, vr core.VR, value string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
	}
	o := object.FromElements([]core.Element{
		leaf(core.NewTag(0x0029, 0x0010), core.VRLO, creator), leaf(core.NewTag(0x0029, 0x1008), core.VRCS, "OLD"),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			leaf(core.NewTag(0x0029, 0x0010), core.VRLO, "OTHER"), leaf(core.NewTag(0x0029, 0x1008), core.VRCS, "KEEP"),
			leaf(core.NewTag(0x0029, 0x0011), core.VRLO, creator), leaf(core.NewTag(0x0029, 0x1108), core.VRCS, "OLD"),
		}}}}},
	}, std.Dictionary)
	s, err := NewEditSession(context.Background(), o, EditSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tag := MustParse(`(0029,"SIEMENS CSA HEADER",08)`)[0]
	if n, err := s.ModifyAll(tag, "NEW"); err != nil || n != 2 {
		t.Fatalf("creator edit: %d %v", n, err)
	}
	items, _ := o.GetSequence(core.NewTag(0x0008, 0x1110))
	if v, _ := items[0].GetString(core.NewTag(0x0029, 0x1008)); v != "KEEP" {
		t.Fatal("edited another creator")
	}
	if v, _ := items[0].GetString(core.NewTag(0x0029, 0x1108)); v != "NEW" {
		t.Fatal("local block unresolved")
	}
	if n, err := s.RemoveAll(tag); err != nil || n != 2 {
		t.Fatalf("creator removal: %d %v", n, err)
	}
	items, _ = o.GetSequence(core.NewTag(0x0008, 0x1110))
	if !items[0].Has(core.NewTag(0x0029, 0x0011)) || !items[0].Has(core.NewTag(0x0029, 0x1008)) {
		t.Fatal("creator or other data removed")
	}
}

func TestRecursiveThreeLevelsAndTwoItemsPerLevel(t *testing.T) {
	var tree func(int) []core.Element
	tree = func(depth int) []core.Element {
		out := []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"OLD"}}}
		if depth < 3 {
			out = append(out, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: tree(depth + 1)}, {Elements: tree(depth + 1)}}}})
		}
		return out
	}
	o := object.FromElements(tree(0), std.Dictionary)
	s, err := NewEditSession(context.Background(), o, EditSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ModifyAll(MustParse("PatientName")[0], "NEW"); err != nil || n != 15 {
		t.Fatalf("three levels: %d %v", n, err)
	}
	seen := 0
	if err := o.WalkPathContext(context.Background(), object.WalkOptions{}, func(_ object.WalkPath, el core.Element) error {
		if el.Tag() == core.NewTag(0x0010, 0x0010) {
			seen++
			if el.StringValue() != "NEW" {
				t.Fatal("missed deep leaf")
			}
		}
		return nil
	}); err != nil || seen != 15 {
		t.Fatalf("walk %d %v", seen, err)
	}
	if n, err := s.RemoveAll(MustParse("PatientName")[0]); err != nil || n != 15 {
		t.Fatalf("deep remove: %d %v", n, err)
	}
}
