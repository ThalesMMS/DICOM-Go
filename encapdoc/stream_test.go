package encapdoc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func streamFixture(t *testing.T, k kind, storage transfer.Syntax, contents []byte, declared int, deferValue bool) (*object.File, *bytes.Reader, []byte) {
	t.Helper()
	dataset := object.New(nil)
	dataset.Put(stringElement(tagSOPClassUID, core.VRUI, k.sopClassUID))
	dataset.Put(stringElement(tagSOPInstanceUID, core.VRUI, "2.25.1665"))
	dataset.Put(stringElement(tagMIMETypeOfEncapsulatedDoc, core.VRLO, k.mimeType))
	dataset.Put(core.NewRawElement(tagEncapsulatedDocument, core.VROB, contents))
	if declared >= 0 {
		dataset.SetUL(tagEncapsulatedDocumentLength, uint32(declared))
	}
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, &object.File{Dataset: dataset, TransferSyntax: storage}); err != nil {
		t.Fatal(err)
	}
	source := bytes.NewReader(wire.Bytes())
	var opts object.ReadFileOptions
	if deferValue {
		opts.InlineValueBytesThreshold = 1
		opts.DeferValueTags = []core.Tag{tagEncapsulatedDocument}
	}
	file, err := object.ReadFileWithOptions(source, opts)
	if err != nil {
		t.Fatal(err)
	}
	element, _ := file.Dataset.Get(tagEncapsulatedDocument)
	_, resident := element.RawBytes()
	if resident != (!deferValue || storage.Deflated) {
		t.Fatalf("resident=%t, deferValue=%t, deflated=%t", resident, deferValue, storage.Deflated)
	}
	return file, source, wire.Bytes()
}

func TestCopyDocumentToMatchesExtract(t *testing.T) {
	// Put the final PDF marker across a copy-buffer boundary. It supersedes
	// the old revision's marker. Bytes after its line ending are not payload.
	pdf := bytes.Repeat([]byte{'x'}, documentChunkSize-2)
	copy(pdf, "%PDF-1.5\n%%EOF\n")
	pdf = append(pdf, "%%EOF\r\n"...)
	for _, tc := range []struct {
		name     string
		kind     kind
		value    []byte
		declared int
		want     []byte
	}{
		{"pdf", kindPDF, append(bytes.Clone(pdf), 'x', 0), -1, pdf},
		{"cda", kindCDA, []byte(syntheticCDA + " \x00"), -1, []byte(syntheticCDA + " ")},
		{"stl", kindSTL, binarySTL(1), -1, binarySTL(1)},
		{"obj", kindOBJ, []byte("v 0 0 0"), 7, []byte("v 0 0 0")},
		{"mtl", kindMTL, []byte("newmtl test\n\x00\x00"), -1, []byte("newmtl test\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, source := range []struct {
				name    string
				syntax  transfer.Syntax
				deferIt bool
			}{
				{"resident", transfer.ExplicitVRLittleEndian, false},
				{"deferred", transfer.ExplicitVRLittleEndian, true},
				{"deflated", transfer.DeflatedExplicitVRLittleEndian, true},
			} {
				t.Run(source.name, func(t *testing.T) {
					file, _, _ := streamFixture(t, tc.kind, source.syntax, tc.value, tc.declared, source.deferIt)
					var payload, stored bytes.Buffer
					n, err := CopyDocumentTo(context.Background(), file.Dataset, &payload, int64(len(tc.want)))
					if err != nil || n != int64(len(tc.want)) || !bytes.Equal(payload.Bytes(), tc.want) {
						t.Fatalf("copy=%d, err=%v, matching=%t", n, err, bytes.Equal(payload.Bytes(), tc.want))
					}
					_, err = file.Dataset.CopyValueTo(tagEncapsulatedDocument, &stored)
					if err != nil || !bytes.HasPrefix(stored.Bytes(), tc.value) || stored.Len()%2 != 0 {
						t.Fatalf("stored value: %d bytes, error=%v", stored.Len(), err)
					}
				})
			}
		})
	}
}

type documentWriterFunc func([]byte) (int, error)

func (f documentWriterFunc) Write(p []byte) (int, error) { return f(p) }

func TestCopyDocumentToBoundsAndErrors(t *testing.T) {
	const contentSize = 2 * documentChunkSize
	contents := bytes.Repeat([]byte{'x'}, contentSize)
	destinationErr := errors.New("destination failed")
	for _, deferValue := range []bool{false, true} {
		file, _, _ := streamFixture(t, kindPDF, transfer.ExplicitVRLittleEndian, contents, len(contents), deferValue)
		var out bytes.Buffer
		if n, err := CopyDocumentTo(context.Background(), file.Dataset, &out, contentSize-1); !errors.Is(err, ErrEncapsulatedDocumentTooLarge) || n != 0 || out.Len() != 0 {
			t.Fatalf("output limit: wrote %d bytes, error=%v", n, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if n, err := CopyDocumentTo(ctx, file.Dataset, &out, 0); !errors.Is(err, context.Canceled) || n != 0 || out.Len() != 0 {
			t.Fatalf("pre-canceled: wrote %d bytes, error=%v", n, err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		cancelingWriter := documentWriterFunc(func(p []byte) (int, error) {
			cancel()
			return len(p), nil
		})
		if n, err := CopyDocumentTo(ctx, file.Dataset, cancelingWriter, 0); !errors.Is(err, context.Canceled) || n <= 0 || n >= contentSize {
			t.Fatalf("delay cancellation: wrote %d bytes, error=%v", n, err)
		}
		for _, failure := range []error{nil, destinationErr} {
			writer := documentWriterFunc(func([]byte) (int, error) { return 9, failure })
			want := failure
			if want == nil {
				want = io.ErrShortWrite
			}
			if n, err := CopyDocumentTo(context.Background(), file.Dataset, writer, 0); !errors.Is(err, want) || n != 9 {
				t.Fatalf("writer failure: wrote %d bytes, error=%v, want %v", n, err, want)
			}
		}
		file.Dataset.SetUL(tagEncapsulatedDocumentLength, contentSize-1) // Final byte is not NUL padding.
		if n, err := CopyDocumentTo(context.Background(), file.Dataset, &out, 0); !errors.As(err, new(*DocumentLengthError)) || n != 0 || out.Len() != 0 {
			t.Fatalf("contradictory declaration: wrote %d bytes, error=%v", n, err)
		}
	}
}

func TestCopyDocumentToReportsTruncatedDeferredSource(t *testing.T) {
	contents := bytes.Repeat([]byte{'x'}, documentChunkSize+2)
	file, source, wire := streamFixture(t, kindPDF, transfer.ExplicitVRLittleEndian, contents, len(contents), true)
	ref, ok, err := file.Dataset.DeferredBulkDataReference([]parser.BulkDataPathStep{{Tag: tagEncapsulatedDocument, ItemIndex: -1}})
	if err != nil || !ok {
		t.Fatalf("deferred reference: %v, found=%t", err, ok)
	}
	source.Reset(wire[:ref.Offset+16])
	var out bytes.Buffer
	n, err := CopyDocumentTo(context.Background(), file.Dataset, &out, 0)
	if !errors.Is(err, io.EOF) || n != 16 || !bytes.Equal(out.Bytes(), contents[:16]) {
		t.Fatalf("truncated replay: wrote %d bytes, error=%v", n, err)
	}
}
