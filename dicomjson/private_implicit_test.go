package dicomjson

import (
	"encoding/json"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestMarshalResolvesUnknownPrivateCreatorsAndSequences(t *testing.T) {
	un := func(group, element uint16, value []byte) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: core.VRUN}, Value: core.RawValue(value)}
	}
	item := core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x0013), VR: core.VRIS}, Value: core.StringValue{"1"}}}}
	obj := object.FromElements([]core.Element{
		un(0x0009, 0x0010, []byte("SPI RELEASE 1 ")),
		un(0x0009, 0x0011, []byte("GEIIS\x00")),
		un(0x0009, 0x0012, []byte("CAF\xc9")),
		un(0x0003, 0x0010, []byte("NOT A CREATOR ")),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0009, 0x1020), VR: core.VRUN, Length: core.UndefinedLength}, Value: core.SequenceValue{Items: []core.DataSet{item}}},
	}, nil)
	data, err := Marshal(obj, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]Element
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if e := got["00090010"]; e.VR != "LO" || len(e.Value) != 1 || e.Value[0] != "SPI RELEASE 1" {
		t.Fatalf("UN creator: %+v", e)
	}
	// Invalid creators (NUL, non-ASCII) and odd groups below 0009 stay UN.
	for _, tag := range []string{"00090011", "00090012", "00030010"} {
		if e := got[tag]; e.VR != "UN" || e.InlineBinary == "" {
			t.Fatalf("%s: %+v", tag, e)
		}
	}
	if e := got["00091020"]; e.VR != "SQ" || len(e.Value) != 1 {
		t.Fatalf("UN sequence: %+v", e)
	}
	back, err := Unmarshal(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if items, ok := back.GetSequence(core.NewTag(0x0009, 0x1020)); !ok || len(items) != 1 {
		t.Fatalf("round trip: %d items", len(items))
	}
}
