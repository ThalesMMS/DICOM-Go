package object

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

func TestElementCapacityHintBoundsDistinctTags(t *testing.T) {
	// Capacity must never be based on the raw occurrence count: parsers may
	// supply repeated tags, while Object retains only their last occurrence.
	for _, tags := range [][]core.Tag{
		nil,
		{},
		{core.NewTag(8, 1)},
		{core.NewTag(8, 1), core.NewTag(8, 2), core.NewTag(0x10, 1)},
		{core.NewTag(0x10, 1), core.NewTag(8, 2), core.NewTag(8, 1)},
		{core.NewTag(8, 1), core.NewTag(8, 2), core.NewTag(8, 1), core.NewTag(8, 3)},
		{core.NewTag(8, 1), core.NewTag(8, 1), core.NewTag(8, 1)},
	} {
		elements := make([]core.Element, len(tags))
		unique := make(map[core.Tag]bool)
		ordered := true
		for i, tag := range tags {
			elements[i].Header.Tag = tag
			unique[tag] = true
			if i > 0 && !tags[i-1].Less(tag) {
				ordered = false
			}
		}
		hint := elementCapacityHint(elements)
		if hint < 0 || hint > len(unique) {
			t.Fatalf("hint %d exceeds %d distinct tags in %v", hint, len(unique), tags)
		}
		if ordered && hint != len(tags) {
			t.Fatalf("ordered input hint = %d, want %d", hint, len(tags))
		}
	}
}

func TestObjectCapacityDenseDuplicatesRetainBoundedOrder(t *testing.T) {
	elements := make([]core.Element, 16384)
	for i := range elements {
		elements[i] = core.NewRawElement(core.NewTag(8, 0x1000+uint16(i%4)), core.VRLO, []byte{byte(i), byte(i >> 8)})
	}
	obj := FromElements(elements, std.Dictionary)
	if obj.Len() != 4 || cap(obj.order) > 4 {
		t.Fatalf("dense duplicates retained len %d, order capacity %d; want 4/at most 4", obj.Len(), cap(obj.order))
	}
	for i := 0; i < 4; i++ {
		raw, ok := obj.GetRaw(core.NewTag(8, 0x1000+uint16(i)))
		last := len(elements) - 4 + i
		if !ok || !bytes.Equal(raw, []byte{byte(last), byte(last >> 8)}) {
			t.Fatalf("tag %d did not retain final occurrence: %v", i, raw)
		}
	}
}

func TestCapacityConstructorsPreserveObjectContracts(t *testing.T) {
	charsetTag := core.NewTag(8, 5)
	nameTag := core.NewTag(0x10, 0x10)
	creatorTag := core.NewTag(0x11, 0x10)
	deferredTag := core.TagPixelData
	deferred := core.Element{Header: core.ElementHeader{Tag: deferredTag, VR: core.VROB, Length: 4, LengthSet: true}}
	elements := []core.Element{
		core.NewRawElement(charsetTag, core.VRCS, []byte("UNSUPPORTED")),
		core.NewRawElement(nameTag, core.VRPN, []byte("FIRST")),
		core.NewRawElement(creatorTag, core.VRLO, []byte("FIRST_CREATOR")),
		deferred,
		core.NewRawElement(nameTag, core.VRPN, []byte("Jos\xe9")),
		core.NewRawElement(creatorTag, core.VRLO, []byte("LAST_CREATOR")),
		deferred,
	}
	ds := core.DataSet{Elements: elements, ItemOffset: 123, ItemOffsetSet: true}
	opts := TextOptions{AllowUnsupportedCharsetFallback: true, FallbackCharacterSet: dicomenc.ISOIR100}
	for _, tc := range []struct {
		name        string
		build       func() *Object
		textOptions bool
		dataset     bool
		parsed      bool
	}{
		{"elements", func() *Object { return FromElements(elements, std.Dictionary) }, false, false, false},
		{"dataset", func() *Object { return FromDataSet(ds, std.Dictionary) }, false, true, false},
		{"elements_text", func() *Object { return FromElementsWithTextOptions(elements, std.Dictionary, opts) }, true, false, false},
		{"dataset_text", func() *Object { return FromDataSetWithTextOptions(ds, std.Dictionary, opts) }, true, true, false},
		{"parsed_dataset", func() *Object { return fromParsedDataSetWithTextOptions(ds, std.Dictionary, opts) }, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := tc.build()
			wantOrder := []core.Tag{charsetTag, nameTag, creatorTag, deferredTag}
			if !reflect.DeepEqual(obj.order, wantOrder) || obj.Len() != len(wantOrder) {
				t.Fatalf("last-position order = %v, want %v", obj.order, wantOrder)
			}
			if offset, set := obj.ItemOffset(); set != tc.dataset || (set && offset != 123) {
				t.Fatalf("ItemOffset() = %d/%v, dataset = %v", offset, set, tc.dataset)
			}
			gotEntry, gotEntryOK := obj.dict.ByTag(nameTag)
			wantEntry, wantEntryOK := std.Dictionary.ByTag(nameTag)
			if gotEntryOK != wantEntryOK || !reflect.DeepEqual(gotEntry, wantEntry) {
				t.Fatal("constructor changed the supplied dictionary")
			}
			if obj.sequenceCache != nil || obj.charsetCached || obj.sharedBacking {
				t.Fatal("constructor changed initial cache/backing state")
			}
			if !obj.privateDuplicateSlots[creatorTag] || !errors.Is(obj.ValidatePrivateReservations(), dictionary.ErrPrivateCreatorConflict) {
				t.Fatal("constructor lost duplicate private-creator ambiguity")
			}
			if obj.ambiguousDeferred[deferredTag] != tc.parsed {
				t.Fatalf("deferred ambiguity = %v, parsed = %v", obj.ambiguousDeferred[deferredTag], tc.parsed)
			}
			wantDeferred := 1
			if tc.parsed {
				wantDeferred = 0
			}
			if obj.deferredCount != wantDeferred {
				t.Fatalf("deferred count = %d, want %d", obj.deferredCount, wantDeferred)
			}
			value, err := obj.LookupString(nameTag)
			if tc.textOptions {
				if err != nil || value != "José" {
					t.Fatalf("text options decode = %q/%v", value, err)
				}
			} else if err == nil {
				t.Fatal("default constructor accepted an unsupported charset")
			}
			// A charset update must invalidate a cached error or fallback decode.
			obj.Put(core.NewRawElement(charsetTag, core.VRCS, []byte("ISO_IR 100")))
			if value, err := obj.LookupString(nameTag); err != nil || value != "José" {
				t.Fatalf("charset update decode = %q/%v", value, err)
			}
			obj.Put(core.NewRawElement(deferredTag, core.VROB, []byte{1, 2, 3, 4}))
			if obj.deferredCount != 0 || obj.ambiguousDeferred[deferredTag] {
				t.Fatal("explicit update did not clear deferred ambiguity/count")
			}
			obj.Put(core.NewRawElement(creatorTag, core.VRLO, []byte("LAST_CREATOR")))
			if err := obj.ValidatePrivateReservations(); err != nil {
				t.Fatalf("explicit private-creator repair = %v", err)
			}
		})
	}
}

func TestCapacityConstructorOrderingAcrossIndexThreshold(t *testing.T) {
	// Duplicates near the lazy order-index threshold exercise both slice
	// removal and indexed tombstones with the new initial capacity.
	for _, count := range []int{orderIndexThreshold - 1, orderIndexThreshold, orderIndexThreshold + 1} {
		for _, reverse := range []bool{false, true} {
			elements := make([]core.Element, count, count+1)
			want := make([]core.Tag, count)
			for i := range elements {
				index := i
				if reverse {
					index = count - i - 1
				}
				want[i] = core.NewTag(8, uint16(0x1000+index))
				elements[i] = core.NewRawElement(want[i], core.VRLO, []byte("FIRST"))
			}
			first := want[0]
			elements = append(elements, core.NewRawElement(first, core.VRLO, []byte("LAST")))
			want = append(want[1:], first)
			obj := FromDataSet(core.DataSet{Elements: elements}, std.Dictionary)
			got := obj.Elements()
			if len(got) != len(want) {
				t.Fatalf("count %d/reverse %v: retained %d, want %d", count, reverse, len(got), len(want))
			}
			for i := range want {
				if got[i].Tag() != want[i] {
					t.Fatalf("count %d/reverse %v: order at %d = %s, want %s", count, reverse, i, got[i].Tag(), want[i])
				}
			}
			if value, ok := obj.GetString(first); !ok || value != "LAST" {
				t.Fatalf("last occurrence = %q/%v", value, ok)
			}
			if !obj.Remove(first) || obj.Has(first) || obj.Len() != count-1 {
				t.Fatal("Remove did not reconcile the indexed duplicate")
			}
			obj.Put(core.NewRawElement(first, core.VRLO, []byte("REINSERTED")))
			got = obj.Elements()
			if len(got) != count || got[count-1].Tag() != first || got[count-1].StringValue() != "REINSERTED" {
				t.Fatal("Put after Remove did not append one final occurrence")
			}
		}
	}
}
