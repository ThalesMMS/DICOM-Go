package parser_test

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// dcmtkChunkSource exposes only io.Reader. It can end on a separate read or
// return the final bytes with EOF, both of which io.Reader permits.
type dcmtkChunkSource struct {
	data       []byte
	chunk      int
	eofLast    bool
	maxRequest int
}

func (s *dcmtkChunkSource) Read(p []byte) (int, error) {
	s.maxRequest = max(s.maxRequest, len(p))
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), s.chunk)], s.data)
	s.data = s.data[n:]
	if s.eofLast && len(s.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

// DCMTK dcmdata/tests/tparser.cc dcmdata_parser_{zeroLengthElementAtEndOfStream,
// valueSplitAcrossChunks,outOfOrderTagWithValueSplitAcrossChunks,
// duplicateTagWithValueSplitAcrossChunks,outOfOrderTagWithCompleteValue}.
// The authored values contain a plausible next header even when the enclosing
// value is cut short. A parser must honor the enclosing length in both cases.
func TestDCMTKFragmentedElementOrderAndEOF(t *testing.T) {
	first := core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte("TEST^STREAM "))
	next := core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("NEXT"))
	empty := core.NewRawElement(core.NewTag(0x0020, 0x000D), core.VRUI, nil)
	for _, syntaxCase := range []struct {
		name   string
		syntax transfer.Syntax
	}{
		{"implicit-le", transfer.ImplicitVRLittleEndian},
		{"explicit-le", transfer.ExplicitVRLittleEndian},
		{"explicit-be", transfer.ExplicitVRBigEndian},
	} {
		t.Run(syntaxCase.name, func(t *testing.T) {
			syntax := syntaxCase.syntax
			prefix := dicomtest.EncodeElement(first, syntax)
			lookalike := dicomtest.EncodeElement(next, syntax)
			for _, order := range []struct {
				name string
				tag  core.Tag
				vr   core.VR
			}{
				{"in-order", core.NewTag(0x0010, 0x1000), core.VRLO},
				{"out-of-order", core.NewTag(0x0008, 0x1030), core.VRLO},
				{"duplicate", first.Tag(), first.VR()},
			} {
				value := bytes.Join([][]byte{[]byte("HEAD"), lookalike, []byte("TAIL")}, nil)
				middle := core.NewRawElement(order.tag, order.vr, value)
				middleWire := dicomtest.EncodeElement(middle, syntax)
				valueOffset := len(prefix) + len(middleWire) - len(value)
				wire := append(bytes.Clone(prefix), middleWire...)
				wire = append(wire, dicomtest.EncodeElements(syntax, next, empty)...)
				truncated := wire[:valueOffset+len(value)-len("TAIL")]
				for _, ending := range []struct {
					name    string
					chunk   int
					eofLast bool
				}{
					{"one-byte-separate-EOF", 1, false},
					{"seven-bytes-EOF-with-data", 7, true},
				} {
					newReader := func(data []byte) *parser.Reader {
						source := &dcmtkChunkSource{data: data, chunk: ending.chunk, eofLast: ending.eofLast}
						return parser.NewReader(source, syntax, parser.ReaderOptions{Dictionary: std.Dictionary})
					}
					t.Run(order.name+"/"+ending.name+"/complete", func(t *testing.T) {
						got, err := newReader(wire).ReadDataSet()
						if err != nil {
							t.Fatal(err)
						}
						want := core.DataSet{Elements: []core.Element{first, middle, next, empty}}
						if diff := dicomtest.DiffDataSet(got, want); diff != "" {
							t.Fatal(diff)
						}
					})
					t.Run(order.name+"/"+ending.name+"/truncated", func(t *testing.T) {
						got, err := newReader(truncated).ReadDataSet()
						checkDCMTKTruncatedValue(t, err, middle.Header, int64(valueOffset))
						if len(got.Elements) != 0 {
							t.Fatal("strict parse published elements after truncation")
						}
						partial, err := newReader(truncated).ReadDataSetPartial()
						checkDCMTKTruncatedValue(t, err, middle.Header, int64(valueOffset))
						if diff := dicomtest.DiffDataSet(partial, core.DataSet{Elements: []core.Element{first}}); diff != "" {
							t.Fatal(diff)
						}
					})
				}
			}
		})
	}
}

func checkDCMTKTruncatedValue(t *testing.T, err error, header core.ElementHeader, offset int64) {
	t.Helper()
	var pe *parser.ParseError
	if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.As(err, &pe) ||
		pe.Op != parser.OpReadValue || pe.Tag != header.Tag || pe.Offset != offset || pe.Length != header.Length {
		t.Fatalf("incomplete value error = %v, want truncation at %s offset %d length %d", err, header.Tag, offset, header.Length)
	}
}

// DCMTK dcmdata/tests/tparser.cc dcmdata_parser_{implicitVR_hugeLengthInBufferStream,
// incrementalBufferStreamRead}. An unsized source cannot justify one read of the
// declared length. This checks read requests and error reporting, not allocations.
func TestDCMTKUnsizedStreamStagesLargeDefinedValues(t *testing.T) {
	syntax := transfer.ImplicitVRLittleEndian
	tag := core.NewTag(0x0042, 0x0011)
	value := bytes.Repeat([]byte{0x41, 0x7e, 0xc2, 0x00}, 17<<10)
	complete := core.NewRawElement(tag, core.VROB, value)
	t.Run("complete-with-many-short-reads", func(t *testing.T) {
		source := &dcmtkChunkSource{data: dicomtest.EncodeElement(complete, syntax), chunk: 541, eofLast: true}
		got, err := parser.NewReader(source, syntax, parser.ReaderOptions{Dictionary: std.Dictionary}).ReadDataSet()
		if err != nil {
			t.Fatal(err)
		}
		if diff := dicomtest.DiffDataSet(got, core.DataSet{Elements: []core.Element{complete}}); diff != "" {
			t.Fatal(diff)
		}
		if source.maxRequest >= len(value) {
			t.Fatalf("unsized source received whole-value read request of %d bytes", source.maxRequest)
		}
	})
	t.Run("forged-length-without-explicit-budget", func(t *testing.T) {
		// Bound the fixture even if a regression allocates this value up front.
		const declared = 64 << 20
		header := dicomtest.SequenceControlBytes(syntax.ByteOrder, tag, declared)
		source := &dcmtkChunkSource{data: append(header, value[:14]...), chunk: 5}
		got, err := parser.NewReader(source, syntax, parser.ReaderOptions{Dictionary: std.Dictionary}).ReadDataSet()
		checkDCMTKTruncatedValue(t, err, core.ElementHeader{Tag: tag, Length: declared}, int64(len(header)))
		if len(got.Elements) != 0 || source.maxRequest > 64<<10 {
			t.Fatalf("truncated stream returned %d elements and received %d-byte read request", len(got.Elements), source.maxRequest)
		}
	})
}

// DCMTK dcmdata/tests/tparser.cc dcmdata_parser_manyFragmentsBufferStreamRead.
// Each fragment has a distinct payload and size pattern so dropping, repeating
// or reordering a fragment is observable even when total bytes match.
func TestDCMTKManyEncapsulatedFragmentsOnShortReader(t *testing.T) {
	const fragmentCount = 129
	fragments := make([][]byte, fragmentCount)
	for i := range fragments {
		fragments[i] = bytes.Repeat([]byte{byte(i), byte(255 - i)}, i%7+1)
	}
	syntax := transfer.JPEGBaseline
	pixelData := dicomtest.NewFragmentSequenceElement(core.TagPixelData, []byte{0, 0, 0, 0}, fragments...)
	sentinel := core.NewRawElement(core.NewTag(0xFFFC, 0xFFFC), core.VROB, []byte{0, 0})
	wire := dicomtest.EncodeElements(syntax, pixelData, sentinel)
	for _, chunk := range []int{1, 7, 47} {
		t.Run(fmt.Sprintf("chunk=%d", chunk), func(t *testing.T) {
			read := func(limit int) (core.DataSet, error) {
				return parser.NewReader(&dcmtkChunkSource{data: wire, chunk: chunk, eofLast: true}, syntax, parser.ReaderOptions{MaxFragments: limit}).ReadDataSet()
			}
			got, err := read(fragmentCount)
			if err != nil {
				t.Fatal(err)
			}
			if diff := dicomtest.DiffDataSet(got, core.DataSet{Elements: []core.Element{pixelData, sentinel}}); diff != "" {
				t.Fatal(diff)
			}
			got, err = read(fragmentCount - 1)
			if !errors.Is(err, parser.ErrMaxFragmentsExceeded) || len(got.Elements) != 0 {
				t.Fatalf("over limit = %d elements, %v", len(got.Elements), err)
			}
		})
	}
}

// DCMTK dcmdata/tests/tparser.cc dcmdata_parser_deflatedBufferStreamRead.
// The compressed transport is much smaller than the decoded value. Deflate is
// produced independently of the DICOM writer and fed through a short reader.
func TestDCMTKDeflatedValueSpansStreamBuffers(t *testing.T) {
	documentTag := core.NewTag(0x0042, 0x0011)
	value := bytes.Repeat([]byte("DEFLATE-STREAM!\x00"), 4096)
	document := core.NewRawElement(documentTag, core.VROB, value)
	end := core.NewRawElement(core.NewTag(0x0042, 0x0012), core.VRLO, nil)
	wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, document, end)
	var compressed bytes.Buffer
	w, err := flate.NewWriter(&compressed, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(wire); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if compressed.Len()%2 != 0 {
		compressed.WriteByte(0)
	}
	for _, chunk := range []int{1, 11, 137} {
		t.Run(fmt.Sprintf("chunk=%d", chunk), func(t *testing.T) {
			read := func(limit int64) (*object.Object, error) {
				return object.ReadDataSetWithOptions(&dcmtkChunkSource{data: compressed.Bytes(), chunk: chunk, eofLast: true},
					transfer.DeflatedExplicitVRLittleEndian, object.ReadFileOptions{MaxTotalBytes: limit})
			}
			// Leave room to probe for EOF after reading the whole data set.
			got, err := read(int64(len(wire) + 1))
			if err != nil {
				t.Fatal(err)
			}
			if diff := dicomtest.DiffDataSet(got.ToDataSet(), core.DataSet{Elements: []core.Element{document, end}}); diff != "" {
				t.Fatal(diff)
			}
			got, err = read(int64(len(wire) - 1))
			if got != nil || !errors.Is(err, parser.ErrMaxTotalBytesExceeded) {
				t.Fatalf("decoded-byte limit error = %v, object returned = %t", err, got != nil)
			}
		})
	}
}
