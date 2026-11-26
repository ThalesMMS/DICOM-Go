package object

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

func TestIndexedFunctionalGroupFacadesPreserveContextAndMutability(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%t", warm), func(t *testing.T) {
			macro := core.NewTag(0x0028, 0x9145)
			text := core.NewTag(0x0008, 0x1030)
			numeric := core.NewTag(0x0028, 0x0010)
			frames := []core.DataSet{
				{},
				{Elements: []core.Element{groupFacadeFixture(macro, core.DataSet{Elements: []core.Element{
					core.NewRawElement(text, core.VRLO, []byte{'J', 'o', 's', 0xe9}),
					core.NewRawElement(numeric, core.VRUS, []byte{1, 2}),
				}})}},
			}
			obj := FromElements([]core.Element{
				{Header: core.ElementHeader{Tag: tagSpecificCharacterSet, VR: core.VRCS}, Value: core.StringValue{"ISO_IR 100"}},
				groupFacadeFixture(tagPerFrameFunctionalGroups, frames...),
			}, nil)
			obj.SetValueByteOrder(binary.BigEndian)
			if warm {
				if _, ok := obj.GetSequence(tagPerFrameFunctionalGroups); !ok {
					t.Fatal("failed to prime whole-sequence cache")
				}
			}
			one, source, err := obj.FunctionalGroupItem(1, macro)
			if err != nil || source != FunctionalGroupPerFrame {
				t.Fatalf("indexed group = %v, %s, %v", one, source, err)
			}
			if got, ok := one.GetString(text); !ok || got != "Jos\u00e9" {
				t.Fatalf("inherited text = %q, %t", got, ok)
			}
			if got, err := one.GetInt(numeric); err != nil || got != 258 {
				t.Fatalf("inherited byte order = %d, %v", got, err)
			}
			one.Remove(text)
			two, _, err := obj.FunctionalGroupItem(1, macro)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := two.GetString(text); !ok || got != "Jos\u00e9" {
				t.Fatalf("editing first facade changed a later lookup: %q, %t", got, ok)
			}
			if missing, source, err := obj.FunctionalGroupItem(len(frames), macro); missing != nil || source != FunctionalGroupNone || err != nil {
				t.Fatalf("out of range = %v, %s, %v", missing, source, err)
			}
		})
	}
}

func BenchmarkFunctionalGroupIndexed(b *testing.B) {
	macro := core.NewTag(0x0028, 0x9145)
	slope := core.NewTag(0x0028, 0x1053)
	for _, count := range []int{128, 1024} {
		for _, warm := range []bool{false, true} {
			b.Run(fmt.Sprintf("%d/warm=%t", count, warm), func(b *testing.B) {
				frames := make([]core.DataSet, count)
				for i := range frames {
					frames[i] = core.DataSet{Elements: []core.Element{
						groupFacadeFixture(macro, core.DataSet{Elements: []core.Element{
							{Header: core.ElementHeader{Tag: slope, VR: core.VRDS}, Value: core.StringValue{fmt.Sprintf("%d", i+1)}},
						}}),
					}}
				}
				obj := FromElements([]core.Element{groupFacadeFixture(tagPerFrameFunctionalGroups, frames...)}, nil)
				if warm {
					obj.GetSequence(tagPerFrameFunctionalGroups)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if !warm {
						obj.sequenceCache = nil
					}
					for frame := range count {
						item, source, err := obj.FunctionalGroupItem(frame, macro)
						if item == nil || source != FunctionalGroupPerFrame || err != nil {
							b.Fatal("unexpected index result", frame, source, err)
						}
					}
				}
			})
		}
	}
}

func groupFacadeFixture(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: core.VRSQ},
		Value:  core.SequenceValue{Items: items},
	}
}
