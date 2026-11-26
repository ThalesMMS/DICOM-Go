package parser_test

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Adapt the unsized Deflate stream from DCMTK dcmdata/tests/tparser.cc.
func TestReferenceAcceptanceDeflatedDatasetAtLogicalByteLimit(t *testing.T) {
	document := core.NewRawElement(core.NewTag(0x0042, 0x0011), core.VROB, bytes.Repeat([]byte{0x51, 0x83}, 33))
	empty := core.NewRawElement(core.NewTag(0x0042, 0x0012), core.VRLO, nil)
	for _, tc := range []struct {
		name     string
		elements []core.Element
	}{
		{"ends-in-value", []core.Element{document}},
		{"ends-in-empty-element", []core.Element{document, empty}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logical := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, tc.elements...)
			limit := int64(len(logical))
			rawDeflate := dcmtkCompressLogicalDataSet(t, logical)
			wire := dcmtkPadDeflate(rawDeflate)
			read := func(wire []byte, limit int64) (*object.Object, error) {
				source := &dcmtkChunkSource{data: wire, chunk: 5, eofLast: true}
				return object.ReadDataSetWithOptions(source, transfer.DeflatedExplicitVRLittleEndian, object.ReadFileOptions{MaxTotalBytes: limit})
			}
			for _, success := range []struct {
				name   string
				budget int64
			}{
				{"one-byte-below-limit", limit + 1},
				{"at-limit", limit},
			} {
				t.Run(success.name, func(t *testing.T) {
					obj, err := read(wire, success.budget)
					if err != nil || obj == nil {
						t.Fatalf("valid %d-byte logical data set with budget %d: object present=%t, error=%v", limit, success.budget, obj != nil, err)
					}
					if diff := dicomtest.DiffDataSet(obj.ToDataSet(), core.DataSet{Elements: tc.elements}); diff != "" {
						t.Fatal(diff)
					}
				})
			}
			t.Run("one-byte-over-limit", func(t *testing.T) {
				obj, err := read(wire, limit-1)
				if obj != nil || !errors.Is(err, parser.ErrMaxTotalBytesExceeded) {
					t.Fatalf("over-limit data set: object present=%t, error=%v, want %v", obj != nil, err, parser.ErrMaxTotalBytesExceeded)
				}
			})
			t.Run("extra-byte-after-exact-prefix", func(t *testing.T) {
				// A parser must distinguish real EOF from a byte beyond a valid prefix.
				overLimit := dcmtkCompressLogicalDataSet(t, append(bytes.Clone(logical), 0xAB))
				obj, err := read(dcmtkPadDeflate(overLimit), limit)
				if obj != nil || !errors.Is(err, parser.ErrMaxTotalBytesExceeded) {
					t.Fatalf("extra byte after complete data set: object present=%t, error=%v, want %v", obj != nil, err, parser.ErrMaxTotalBytesExceeded)
				}
			})
			t.Run("missing-deflate-ending", func(t *testing.T) {
				obj, err := read(rawDeflate[:len(rawDeflate)-1], limit)
				if obj != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("truncated Deflate stream: object present=%t, error=%v, want %v", obj != nil, err, io.ErrUnexpectedEOF)
				}
			})
		})
	}
}

func dcmtkCompressLogicalDataSet(t *testing.T, logical []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(logical); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func dcmtkPadDeflate(raw []byte) []byte {
	if len(raw)%2 != 0 {
		return append(raw, 0)
	}
	return raw
}
