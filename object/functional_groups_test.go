package object

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

func TestFunctionalGroupItem(t *testing.T) {
	macro := core.NewTag(0x0028, 0x9145)
	item := func(v string) core.DataSet {
		return core.DataSet{Elements: []core.Element{{
			Header: core.ElementHeader{Tag: macro, VR: core.VRSQ},
			Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{{
				Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x1053), VR: core.VRDS}, Value: core.StringValue{v}}}}}},
		}}}
	}
	group := func(tag core.Tag, items ...core.DataSet) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
	}
	slope := func(o *Object) string { s, _ := o.GetString(core.NewTag(0x0028, 0x1053)); return s }

	obj := FromElements([]core.Element{
		group(tagSharedFunctionalGroups, core.DataSet{}),
		group(tagPerFrameFunctionalGroups, item("2"), core.DataSet{}),
	}, nil)
	if !obj.HasFunctionalGroups() {
		t.Fatal("functional groups not detected")
	}
	if got, source, err := obj.FunctionalGroupItem(0, macro); err != nil || source != FunctionalGroupPerFrame || slope(got) != "2" {
		t.Fatalf("frame 0: %v %v", source, err)
	}
	if got, source, err := obj.FunctionalGroupItem(1, macro); err != nil || source != FunctionalGroupNone || got != nil {
		t.Fatalf("frame 1: %v %v", source, err)
	}

	shared := FromElements([]core.Element{
		group(tagSharedFunctionalGroups, item("1")),
		group(tagPerFrameFunctionalGroups, core.DataSet{}, item("3")),
	}, nil)
	if got, source, _ := shared.FunctionalGroupItem(0, macro); source != FunctionalGroupShared || slope(got) != "1" {
		t.Fatalf("shared: %v", source)
	}
	if got, source, err := shared.FunctionalGroupItem(1, macro); !errors.Is(err, ErrFunctionalGroupConflict) || source != FunctionalGroupPerFrame || slope(got) != "3" {
		t.Fatalf("conflict: %v %v", source, err)
	}
	if New(nil).HasFunctionalGroups() {
		t.Fatal("plain object has functional groups")
	}
}

func TestFunctionalGroupItemsAndConflicts(t *testing.T) {
	macro := core.NewTag(0x0040, 0x9096) // Real World Value Mapping: several items
	other := core.NewTag(0x0020, 0x9113)
	sequence := func(tag core.Tag, items int) core.Element {
		value := core.SequenceValue{}
		for i := 0; i < items; i++ {
			value.Items = append(value.Items, core.DataSet{Elements: []core.Element{{
				Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0x9224), VR: core.VRFD},
				Value:  core.RawValue{0, 0, 0, 0, 0, 0, 0, 0},
			}}})
		}
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: value}
	}
	group := func(tag core.Tag, items ...core.DataSet) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
	}
	obj := FromElements([]core.Element{
		group(tagSharedFunctionalGroups, core.DataSet{Elements: []core.Element{sequence(macro, 2), sequence(other, 1)}}),
		group(tagPerFrameFunctionalGroups,
			core.DataSet{},
			core.DataSet{Elements: []core.Element{sequence(macro, 3)}},
			core.DataSet{Elements: []core.Element{sequence(macro, 1), sequence(other, 1)}},
		),
	}, nil)
	if items, source, err := obj.FunctionalGroupItems(0, macro); err != nil || source != FunctionalGroupShared || len(items) != 2 {
		t.Fatalf("frame 0: %d items from %s, %v", len(items), source, err)
	}
	if items, source, err := obj.FunctionalGroupItems(1, macro); !errors.Is(err, ErrFunctionalGroupConflict) || source != FunctionalGroupPerFrame || len(items) != 3 {
		t.Fatalf("frame 1: %d items from %s, %v", len(items), source, err)
	}
	if items, source, _ := obj.FunctionalGroupItems(-1, macro); source != FunctionalGroupShared || len(items) != 2 {
		t.Fatalf("shared only: %d items from %s", len(items), source)
	}
	conflicts := obj.FunctionalGroupConflicts()
	want := []FunctionalGroupConflict{{Macro: other, Frames: []int{2}}, {Macro: macro, Frames: []int{1, 2}}}
	if len(conflicts) != 2 {
		t.Fatalf("conflicts %+v", conflicts)
	}
	for i := range want {
		if conflicts[i].Macro != want[i].Macro || fmt.Sprint(conflicts[i].Frames) != fmt.Sprint(want[i].Frames) {
			t.Fatalf("conflicts %+v, want %+v", conflicts, want)
		}
	}
	if New(nil).FunctionalGroupConflicts() != nil {
		t.Fatal("conflicts without functional groups")
	}
}
