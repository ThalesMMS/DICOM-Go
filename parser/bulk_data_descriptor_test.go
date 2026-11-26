package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestBulkDataDescriptorVRBoundaryAndSnapshot(t *testing.T) {
	tag := core.NewTag(0x0011, 0x1001)
	for _, vr := range []core.VR{core.VROB, core.VROD, core.VROF, core.VROL, core.VROV, core.VROW, core.VRUN, core.VRDS, core.VRIS, core.VRFL, core.VRFD, core.VRSL, core.VRSS, core.VRSV, core.VRUL, core.VRUS, core.VRUV, core.VRLT, core.VRST, core.VRUC, core.VRUT} {
		t.Run(string(vr), func(t *testing.T) {
			wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, core.NewRawElement(tag, vr, make([]byte, 8)))
			for _, threshold := range []int64{7, 8} {
				d := &BulkDataDescriptor{Rules: []BulkDataRule{{VR: vr, Threshold: threshold, Path: []BulkDataPathStep{{Tag: tag, ItemIndex: -1}}}}}
				r := NewReader(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{BulkDataDescriptor: d})
				d.Rules[0].Threshold = 100
				d.Rules[0].Path[0].Tag = core.TagPixelData
				ds, err := r.ReadDataSet()
				if err != nil {
					t.Fatal(err)
				}
				if got := ds.Elements[0].Value == nil; got != (threshold == 7) {
					t.Fatalf("deferred=%v threshold=%d", got, threshold)
				}
				if threshold == 7 {
					loc, ok := r.ValueLocation(tag)
					if !ok {
						t.Fatal("missing location")
					}
					ref, err := r.ValueReferenceAt(loc)
					if err != nil {
						t.Fatal(err)
					}
					if ref.Length != 8 || ref.VR != vr || ref.Path[0].Tag != tag {
						t.Fatalf("bad reference %+v", ref)
					}
					ref.Path[0].Tag = core.TagPixelData
					again, err := r.ValueReferenceAt(loc)
					if err != nil || again.Path[0].Tag != tag {
						t.Fatal("reference path alias")
					}
				}
			}
		})
	}
}

func TestBulkDataDescriptorNestedFullPathAndWildcard(t *testing.T) {
	seq := core.NewTag(0x0008, 0x1115)
	leaf := core.NewTag(0x0011, 0x1001)
	value := core.NewRawElement(leaf, core.VROL, []byte{1, 0, 0, 0, 2, 0, 0, 0})
	wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, value, core.Element{Header: core.ElementHeader{Tag: seq, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{value}}, {Elements: []core.Element{value}}}}})
	for _, index := range []int{0, 1, -1} {
		t.Run(string(rune('a'+index+1)), func(t *testing.T) {
			path := []BulkDataPathStep{{Tag: seq, ItemIndex: index}, {Tag: leaf, ItemIndex: -1}}
			r := NewReader(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{BulkDataDescriptor: &BulkDataDescriptor{Rules: []BulkDataRule{{Path: path}}}})
			ds, err := r.ReadDataSet()
			if err != nil {
				t.Fatal(err)
			}
			if ds.Elements[0].Value == nil {
				t.Fatal("path matched root leaf")
			}
			items := ds.Elements[1].Value.(core.SequenceValue).Items
			for i, item := range items {
				if (item.Elements[0].Value == nil) != (index == -1 || index == i) {
					t.Fatalf("wrong item %d", i)
				}
			}
			for _, loc := range r.ValueLocations(leaf) {
				ref, err := r.ValueReferenceAt(loc)
				if err != nil {
					t.Fatal(err)
				}
				want := []BulkDataPathStep{{Tag: seq, ItemIndex: ref.Path[0].ItemIndex}, {Tag: leaf, ItemIndex: -1}}
				if !loc.ItemOffsetSet || !reflect.DeepEqual(ref.Path, want) {
					t.Fatalf("bad nested reference %+v", ref)
				}
			}
		})
	}
}

func TestBulkDataDescriptorLimitsAndInvalidSources(t *testing.T) {
	tag := core.NewTag(0x0011, 0x1001)
	wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, core.NewRawElement(tag, core.VROB, make([]byte, 8)))
	valid := &BulkDataDescriptor{Rules: []BulkDataRule{{VR: core.VROB}}}
	for _, tc := range []struct {
		name string
		opts ReaderOptions
		data []byte
		want error
	}{
		{"element", ReaderOptions{BulkDataDescriptor: valid, MaxElementBytes: 7}, wire, ErrMaxElementBytesExceeded},
		{"total", ReaderOptions{BulkDataDescriptor: valid, MaxTotalBytes: 19}, wire, ErrMaxTotalBytesExceeded},
		{"truncated", ReaderOptions{BulkDataDescriptor: valid}, wire[:len(wire)-1], io.ErrUnexpectedEOF},
		{"negative", ReaderOptions{BulkDataDescriptor: &BulkDataDescriptor{Rules: []BulkDataRule{{Threshold: -1}}}}, wire, ErrInvalidBulkDataDescriptor},
		{"vr", ReaderOptions{BulkDataDescriptor: &BulkDataDescriptor{Rules: []BulkDataRule{{VR: core.VRSQ}}}}, wire, ErrInvalidBulkDataDescriptor},
		{"rules", ReaderOptions{BulkDataDescriptor: &BulkDataDescriptor{Rules: make([]BulkDataRule, MaxBulkDataRules+1)}}, wire, ErrInvalidBulkDataDescriptor},
		{"depth", ReaderOptions{BulkDataDescriptor: &BulkDataDescriptor{Rules: []BulkDataRule{{Path: make([]BulkDataPathStep, MaxBulkDataPathSteps+1)}}}}, wire, ErrInvalidBulkDataDescriptor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewReader(bytes.NewReader(tc.data), transfer.ExplicitVRLittleEndian, tc.opts).ReadDataSet()
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want %v", err, tc.want)
			}
		})
	}
	_, err := NewReader(bytes.NewBuffer(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{BulkDataDescriptor: valid}).ReadDataSet()
	if !errors.Is(err, ErrInvalidBulkDataDescriptor) {
		t.Fatal(err)
	}
	_, err = NewReader(bytes.NewReader(wire), transfer.DeflatedExplicitVRLittleEndian, ReaderOptions{BulkDataDescriptor: valid}).ReadDataSet()
	if !errors.Is(err, ErrInvalidBulkDataDescriptor) {
		t.Fatal(err)
	}
}

func TestBulkDataDescriptorDoesNotSelectFragmentItemsOrPrivateCreators(t *testing.T) {
	creator := core.NewRawElement(core.NewTag(0x0011, 0x0010), core.VRUN, []byte("TEST"))
	pixel := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: [][]byte{{1, 2}}}}
	wire := dicomtest.EncodeElements(transfer.JPEGBaseline, creator, pixel)
	ds, err := NewReader(bytes.NewReader(wire), transfer.JPEGBaseline, ReaderOptions{BulkDataDescriptor: &BulkDataDescriptor{Rules: []BulkDataRule{{}}}}).ReadDataSet()
	if err != nil {
		t.Fatal(err)
	}
	if ds.Elements[0].Value == nil {
		t.Fatal("private creator deferred")
	}
	if _, ok := ds.Elements[1].Value.(core.FragmentSequence); !ok {
		t.Fatal("fragments lost")
	}
}

func TestBulkDataReferencePhysicalOffsetAndBigEndian(t *testing.T) {
	tag := core.NewTag(0x0011, 0x1001)
	value := []byte{0, 0, 0, 42}
	wire := dicomtest.EncodeElements(transfer.ExplicitVRBigEndian, core.NewRawElement(tag, core.VROL, value))
	source := bytes.NewReader(append(make([]byte, 37), wire...))
	source.Seek(37, io.SeekStart)
	r := NewReader(source, transfer.ExplicitVRBigEndian, ReaderOptions{BaseOffset: 123, BulkDataDescriptor: &BulkDataDescriptor{Rules: []BulkDataRule{{}}}})
	if _, err := r.ReadDataSet(); err != nil {
		t.Fatal(err)
	}
	loc, _ := r.ValueLocation(tag)
	ref, err := r.ValueReferenceAt(loc)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Offset != 49 || ref.ByteOrder != binary.BigEndian {
		t.Fatalf("bad physical reference %+v", ref)
	}
	var got bytes.Buffer
	if _, err := r.CopyElementValueAt(loc, &got); err != nil || !bytes.Equal(got.Bytes(), value) {
		t.Fatalf("copy %x error %v", got.Bytes(), err)
	}
	if _, err := r.ValueReferenceAt(ValueLocation{Tag: tag, ValueOffset: -1}); err == nil {
		t.Fatal("forged location accepted")
	}
}

func TestBulkDataReferenceRejectsArithmeticOverflow(t *testing.T) {
	for _, tc := range []struct{ base, root, offset, length int64 }{
		{math.MinInt64, 0, 0, 1},
		{0, math.MaxInt64, 1, 1},
		{0, math.MaxInt64, 0, 1},
	} {
		location := ValueLocation{Tag: core.TagPixelData, ValueOffset: tc.offset, Length: tc.length}
		reader := &Reader{baseOffset: tc.base, rootOffset: tc.root,
			counter: &countingReader{r: bytes.NewReader(nil)}, valueReferences: map[ValueLocation]ValueReference{
				location: {Tag: core.TagPixelData, Length: tc.length, Path: []BulkDataPathStep{{Tag: core.TagPixelData, ItemIndex: -1}}},
			}}
		if _, err := reader.ValueReferenceAt(location); err == nil {
			t.Fatalf("accepted overflow %+v", tc)
		}
	}
}
