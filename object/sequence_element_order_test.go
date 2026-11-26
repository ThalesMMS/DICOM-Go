package object

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// An item built in any order is a data set, and a data set is encoded in
// ascending tag order.
func TestSequenceElementOrdersItemElementsByTag(t *testing.T) {
	text := func(tag core.Tag, value string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSH}, Value: core.StringValue{value}}
	}
	later, earlier := core.NewTag(0x0040, 0x1001), core.NewTag(0x0040, 0x0009)
	item := FromElements([]core.Element{text(later, "RP1"), text(earlier, "STEP1")}, std.Dictionary)
	sequence := SequenceElement(core.NewTag(0x0040, 0x0275), item)
	elements := sequence.Value.(core.SequenceValue).Items[0].Elements
	if len(elements) != 2 || elements[0].Tag() != earlier || elements[1].Tag() != later {
		t.Fatalf("item elements = %v, %v; want ascending tag order", elements[0].Tag(), elements[1].Tag())
	}
}
