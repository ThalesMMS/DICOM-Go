package tractography

import (
	"bytes"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestFacadeExcludesFiniteRootBulkBeforeConstructor(t *testing.T) {
	// This is deliberately finite and benign. Only metadata is admitted to
	// the facade, even when ignored root bulk has a non-pixel payload kind.
	payload := core.RawValue{1, 2, 3, 4, 5, 6, 7, 8}
	nested := core.Element{
		Header: core.ElementHeader{Tag: core.NewTag(9, 0x1001), VR: core.VROB, Length: 8},
		Value:  payload,
	}
	for depth := 0; depth < 64; depth++ {
		nested = core.Element{
			Header: core.ElementHeader{Tag: core.NewTag(9, 0x1002), VR: core.VRSQ, Length: core.UndefinedLength},
			Value:  core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{nested}}}},
		}
	}
	bulk := []core.Tag{core.TagPixelData, core.NewTag(0x7fe0, 8), core.NewTag(0x7fe0, 9), core.NewTag(0x5600, 0x20)}
	id := core.NewTag(0x10, 0x20)
	es := []core.Element{{Header: core.ElementHeader{Tag: id, VR: core.VRLO, Length: 2}, Value: core.StringValue{"ID"}}}
	for _, tag := range bulk {
		es = append(es, core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VROB}, Value: nested.Value})
	}
	// All backing is finite before construction; no cycle or stack probe.
	source := object.FromElements(es, nil)
	b, err := operation(nil, Limits{MaxSequenceDepth: 1, MaxElements: 32, MaxItems: 1, MaxValueBytes: 2, MaxTextBytes: 2, MaxBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	facade, err := b.facade(source)
	if err != nil {
		t.Fatal(err)
	}
	if facade.Len() != 1 {
		t.Fatalf("facade contains %d elements; only metadata may reach Object constructor", facade.Len())
	}
	if b.elements != 4*len(es) {
		t.Fatal("ignored root headers were not charged", b.elements)
	}
	if value, ok := facade.GetString(id); !ok || value != "ID" {
		t.Fatal("metadata lost", value, ok)
	}
	for _, tag := range bulk {
		if facade.Has(tag) {
			t.Fatalf("ignored bulk %s entered facade", tag)
		}
		el, ok := source.Get(tag)
		if !ok {
			t.Fatalf("source bulk %s was removed", tag)
		}
		seq := el.Value.(core.SequenceValue)
		original := nested.Value.(core.SequenceValue)
		if &seq.Items[0].Elements[0] != &original.Items[0].Elements[0] {
			t.Fatal("source backing changed")
		}
	}
	if source.Len() != len(es) || !bytes.Equal(payload, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatal("source structure or payload changed")
	}
	if value, ok := source.GetString(id); !ok || value != "ID" {
		t.Fatal("source metadata changed", value, ok)
	}
}
