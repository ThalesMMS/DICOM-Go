package object

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestInheritCommonByteOrderAndSourceIsolation(t *testing.T) {
	for _, orders := range []struct {
		name           string
		source, target binary.ByteOrder
	}{{"BE-LE", binary.BigEndian, binary.LittleEndian}, {"LE-BE", binary.LittleEndian, binary.BigEndian}, {"BE-BE", binary.BigEndian, binary.BigEndian}, {"LE-LE", binary.LittleEndian, binary.LittleEndian}} {
		t.Run(orders.name, func(t *testing.T) {
			var nested []core.Element
			for index, vr := range []core.VR{core.VRAT, core.VROW, core.VRSS, core.VRUS, core.VRFL, core.VROF, core.VROL, core.VRSL, core.VRUL, core.VRFD, core.VROD, core.VROV, core.VRSV, core.VRUV, core.VROB, core.VRUN} {
				width := valueencode.ByteOrderWidth(vr)
				le := make([]byte, 2*width)
				for i := range le {
					le[i] = byte(i + 1)
				}
				raw := le
				if orders.source == binary.BigEndian {
					raw = valueencode.ReverseFixedWidth(le, width)
				}
				nested = append(nested, core.NewRawElement(core.NewTag(0x0011, uint16(0x1000+index)), vr, raw))
			}
			nested = append(nested, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1100), VR: core.VRUS}, Value: core.Uint16Value{4}})
			before := make([][]byte, len(nested)-1)
			for index, element := range nested[:len(nested)-1] {
				before[index] = core.CloneBytes(element.Value.(core.RawValue))
			}
			root := core.NewTag(0x0008, 0x1120)
			source := FromElements([]core.Element{{Header: core.ElementHeader{Tag: root, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: nested}}}}}, nil)
			source.SetValueByteOrder(orders.source)
			destination := New(nil)
			destination.SetValueByteOrder(orders.target)
			if err := (DerivedCommon{Source: source}).Apply(destination, InheritPatient); err != nil {
				t.Fatal(err)
			}
			items, ok := destination.GetSequence(root)
			if !ok || len(items) != 1 {
				t.Fatal("missing inherited sequence")
			}
			for index, original := range nested[:len(nested)-1] {
				copied, _ := items[0].Get(original.Tag())
				raw, _ := copied.RawBytes()
				want := before[index]
				if orders.source != orders.target {
					want = valueencode.ReverseFixedWidth(want, valueencode.ByteOrderWidth(original.VR()))
				}
				if !bytes.Equal(raw, want) {
					t.Fatalf("VR %s changed numeric value", original.VR())
				}
				raw[0] ^= 0xff
				if !bytes.Equal(original.Value.(core.RawValue), before[index]) {
					t.Fatal("source value aliases destination")
				}
			}
			typed, _ := items[0].Get(core.NewTag(0x0011, 0x1100))
			if value, ok := typed.Value.(core.Uint16Value); !ok || value[0] != 4 {
				t.Fatal("typed value representation changed")
			}
			typed.Value.(core.Uint16Value)[0] = 99
			if nested[len(nested)-1].Value.(core.Uint16Value)[0] != 4 {
				t.Fatal("typed value alias escaped")
			}
		})
	}
}

func TestInheritCommonTypedValuesAndFragmentsDetached(t *testing.T) {
	root := core.NewTag(0x0008, 0x1120)
	tag := core.NewTag(0x0011, 0x1010)
	for _, test := range []struct {
		name      string
		vr        core.VR
		value     core.Value
		mutate    func(core.Value)
		unchanged func(core.Value) bool
	}{
		{"UV", core.VRUV, core.Uint64Value{1}, func(v core.Value) { v.(core.Uint64Value)[0] = 99 }, func(v core.Value) bool { return v.(core.Uint64Value)[0] == 1 }},
		{"SV", core.VRSV, core.Int64Value{-1}, func(v core.Value) { v.(core.Int64Value)[0] = 99 }, func(v core.Value) bool { return v.(core.Int64Value)[0] == -1 }},
		{"AT", core.VRAT, core.TagValue{core.NewTag(0x0010, 0x0010)}, func(v core.Value) { v.(core.TagValue)[0] = core.TagPixelData }, func(v core.Value) bool { return v.(core.TagValue)[0] == core.NewTag(0x0010, 0x0010) }},
		{"fragments", core.VRUN, core.FragmentSequence{OffsetTable: []byte{0, 0, 0, 0}, Fragments: [][]byte{{1, 2}, {3, 4}}}, func(v core.Value) {
			f := v.(core.FragmentSequence)
			f.OffsetTable[0] = 99
			f.Fragments[0][0] = 99
			f.Fragments[1] = []byte{99}
		}, func(v core.Value) bool {
			f := v.(core.FragmentSequence)
			return f.OffsetTable[0] == 0 && f.Fragments[0][0] == 1 && f.Fragments[1][0] == 3
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			src := FromElements([]core.Element{{Header: core.ElementHeader{Tag: root, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{{Header: core.ElementHeader{Tag: tag, VR: test.vr}, Value: test.value}}}}}}}, nil)
			dst := New(nil)
			if err := (DerivedCommon{Source: src}).Apply(dst, InheritPatient); err != nil {
				t.Fatal(err)
			}
			items, _ := dst.GetSequence(root)
			copied, _ := items[0].Get(tag)
			test.mutate(copied.Value)
			if !test.unchanged(test.value) {
				t.Fatal("inherited value aliases source")
			}
		})
	}
	for _, value := range []core.FragmentSequence{{}, {OffsetTable: []byte{}, Fragments: [][]byte{nil, {}}}} {
		copied := cloneInheritedValue(value).(core.FragmentSequence)
		if (value.OffsetTable == nil) != (copied.OffsetTable == nil) || (value.Fragments == nil) != (copied.Fragments == nil) {
			t.Fatal("fragment copy changed nil versus empty")
		}
		for index, fragment := range value.Fragments {
			if (fragment == nil) != (copied.Fragments[index] == nil) {
				t.Fatal("fragment payload copy changed nil versus empty")
			}
		}
	}
}

func TestInheritCommonDeferredTextUsesSourceCharset(t *testing.T) {
	name := core.NewTag(0x0010, 0x0010)
	source := FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0005), core.VRCS, []byte("ISO_IR 100")),
		core.NewRawElement(name, core.VRPN, []byte("M\xfcller^SYNTHETIC")),
	}, nil)
	var encoded bytes.Buffer
	if err := WriteDataSet(&encoded, source, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "text.dcm")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hooks, err := validation.NewHookChain(validation.HookRegistration{Name: "defer-root-text", Points: []validation.HookPoint{validation.HookElementHeaderRead}, Hook: validation.HookFunc(func(_ context.Context, event validation.HookEvent) (validation.HookDecision, error) {
		return validation.HookDecision{DeferValue: event.Header != nil && event.Header.Tag == name}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	obj, _, err := ReadDataSetWithValidation(context.Background(), file, transfer.ExplicitVRLittleEndian, ReadFileOptions{}, validation.Options{Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	deferred, _ := obj.Get(name)
	if deferred.Value != nil {
		t.Fatal("text fixture is not deferred")
	}
	dst := New(nil)
	dst.Put(core.NewRawElement(core.NewTag(0x0008, 0x0005), core.VRCS, []byte("ISO_IR 192")))
	if err := (DerivedCommon{Source: obj}).Apply(dst, InheritPatient); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	text, ok := dst.GetString(name)
	if !ok || text != "Müller^SYNTHETIC" {
		t.Fatalf("deferred text lost source charset: %q %v", text, ok)
	}
	var roundtrip bytes.Buffer
	if err := WriteDataSet(&roundtrip, dst, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	read, err := ReadDataSet(bytes.NewReader(roundtrip.Bytes()), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	text, ok = read.GetString(name)
	if !ok || text != "Müller^SYNTHETIC" {
		t.Fatal("deferred text could not round-trip in destination charset")
	}
}

func TestInheritCommonByteOrderDeferredAndInvalidUnits(t *testing.T) {
	root := core.NewTag(0x0008, 0x1120)
	blob := core.NewTag(0x0011, 0x1010)
	raw := bytes.Repeat([]byte{0, 4}, 128)
	source := FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.2")),
		core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.826.0.1.3680043.10.543.1525.7")),
		{Header: core.ElementHeader{Tag: root, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{core.NewRawElement(blob, core.VROW, raw)}}}}},
	}, nil)
	source.SetValueByteOrder(binary.BigEndian)
	var encoded bytes.Buffer
	if err := WriteFile(&encoded, &File{Dataset: source, TransferSyntax: transfer.ExplicitVRBigEndian}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deferred.dcm")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := OpenFileWithOptions(path, ReadFileOptions{InlineValueBytesThreshold: 64, DeferNestedValues: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	items, _ := file.Dataset.GetSequence(root)
	deferred, _ := items[0].Get(blob)
	if deferred.Value != nil {
		t.Fatal("fixture is not deferred")
	}
	dst := New(nil)
	if err := (DerivedCommon{Source: file.Dataset}).Apply(dst, InheritPatient); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	copied, _ := dst.GetSequence(root)
	value, _ := copied[0].Get(blob)
	got, _ := value.RawBytes()
	if !bytes.Equal(got, bytes.Repeat([]byte{4, 0}, 128)) {
		t.Fatal("deferred numeric value was not detached in target byte order")
	}
	if !bytes.Equal(raw, bytes.Repeat([]byte{0, 4}, 128)) {
		t.Fatal("deferred copy changed source bytes")
	}
	for _, vr := range []core.VR{core.VRUS, core.VRAT, core.VRUL, core.VRFD} {
		source := FromElements([]core.Element{{Header: core.ElementHeader{Tag: root, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{core.NewRawElement(blob, vr, []byte{1})}}}}}}, nil)
		source.SetValueByteOrder(binary.BigEndian)
		dst := New(nil)
		if err := (DerivedCommon{Source: source}).Apply(dst, InheritPatient); err == nil || dst.Has(root) {
			t.Fatalf("converted partial %s numeric unit accepted", vr)
		}
	}
}
