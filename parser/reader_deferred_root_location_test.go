package parser

import (
	"bytes"
	"io"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type rootLocationReadSpy struct {
	*bytes.Reader
	reads int
}

func (s *rootLocationReadSpy) Read(p []byte) (int, error) { s.reads++; return s.Reader.Read(p) }

func TestRootValueLocationUniqueRootReadOnly(t *testing.T) {
	tag := core.NewTag(0x0042, 0x0011)
	leaf := core.NewRawElement(tag, core.VROB, []byte{1, 2, 3, 4})
	seq := dicomtest.NewSequenceElement(core.NewTag(0x0040, 0xa730), core.DataSet{Elements: []core.Element{leaf, leaf}})
	for _, tc := range []struct {
		name     string
		elements []core.Element
		root     bool
		global   bool
		count    int
	}{
		{"root", []core.Element{leaf}, true, true, 1},
		{"nested", []core.Element{seq}, false, false, 2},
		{"root-and-nested", []core.Element{leaf, seq}, true, false, 3},
		{"duplicate-root-and-nested", []core.Element{leaf, seq, leaf}, false, false, 4},
		{"absent", nil, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, tc.elements...)
			spy := &rootLocationReadSpy{Reader: bytes.NewReader(wire)}
			r := NewReader(spy, transfer.ExplicitVRLittleEndian, ReaderOptions{BulkDataDescriptor: &BulkDataDescriptor{Rules: []BulkDataRule{{VR: core.VROB}}}})
			if _, err := r.ReadDataSet(); err != nil {
				t.Fatal(err)
			}
			before := r.ValueLocations(tag)
			if len(before) != tc.count {
				t.Fatalf("record count=%d", len(before))
			}
			pos, _ := spy.Seek(0, io.SeekCurrent)
			reads := spy.reads
			loc, ok := r.RootValueLocation(tag)
			if ok != tc.root {
				t.Fatalf("root=%v location=%+v", ok, loc)
			}
			if !ok && loc != (ValueLocation{}) {
				t.Fatal("ambiguous/absent must return zero")
			}
			if ok && (loc.ItemOffsetSet || loc.Tag != tag || loc.Length != 4) {
				t.Fatalf("wrong root=%+v", loc)
			}
			if _, global := r.ValueLocation(tag); global != tc.global {
				t.Fatal("legacy tag-only ambiguity changed")
			}
			loc.ValueOffset = -1
			again, againOK := r.RootValueLocation(tag)
			if againOK != ok || ok && again.ValueOffset < 0 {
				t.Fatal("returned value aliases stored metadata")
			}
			var probe ValueLocation
			var probeOK bool
			if got := testing.AllocsPerRun(100, func() { probe, probeOK = r.RootValueLocation(tag) }); got != 0 {
				t.Fatalf("metadata query allocations=%v", got)
			}
			if probeOK != tc.root || probeOK && probe.ItemOffsetSet {
				t.Fatal("allocation probe did not consume a unique root result")
			}
			after, _ := spy.Seek(0, io.SeekCurrent)
			if reads != spy.reads || pos != after || !reflect.DeepEqual(before, r.ValueLocations(tag)) {
				t.Fatal("query modified source or recorded locations")
			}
			if len(before) > 0 {
				before[0].ValueOffset = -1
				if r.ValueLocations(tag)[0].ValueOffset < 0 {
					t.Fatal("legacy ValueLocations copy changed")
				}
			}
		})
	}
	var nilReader *Reader
	if loc, ok := nilReader.RootValueLocation(tag); ok || loc != (ValueLocation{}) {
		t.Fatal("nil convention changed")
	}
}

func TestValueReferencePreservesOriginalLengthForm(t *testing.T) {
	for _, undefined := range []bool{false, true} {
		name := "defined"
		if undefined {
			name = "undefined"
		}
		t.Run(name, func(t *testing.T) {
			elem := core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2, 3, 4})
			if undefined {
				elem = dicomtest.NewFragmentSequenceElement(core.TagPixelData, []byte{0, 0, 0, 0}, []byte{1, 2, 3, 4})
			}
			wire := dicomtest.EncodeElement(elem, transfer.JPEGBaseline)
			spy := &rootLocationReadSpy{Reader: bytes.NewReader(wire)}
			r := NewReader(spy, transfer.JPEGBaseline, ReaderOptions{DeferPixelData: true})
			ds, err := r.ReadDataSet()
			if err != nil {
				t.Fatal(err)
			}
			if ds.Elements[0].Value != nil {
				t.Fatal("fixture must defer actual root Pixel Data")
			}
			loc, ok := r.RootValueLocation(core.TagPixelData)
			if !ok {
				t.Fatal("origin missing")
			}
			position, _ := spy.Seek(0, io.SeekCurrent)
			reads := spy.reads
			ref, err := r.ValueReferenceAt(loc)
			if err != nil || ref.UndefinedLength != undefined || ref.Length != int64(len(wire)-12) {
				t.Fatalf("original length form lost: %+v %v", ref, err)
			}
			ref.UndefinedLength = !undefined
			again, err := r.ValueReferenceAt(loc)
			if err != nil || again.UndefinedLength != undefined {
				t.Fatal("caller mutated original metadata")
			}
			after, _ := spy.Seek(0, io.SeekCurrent)
			if reads != spy.reads || position != after {
				t.Fatal("metadata read payload or moved cursor")
			}
		})
	}
}
