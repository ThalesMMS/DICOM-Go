package encapdoc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestDocumentLengthDistinguishesPaddingFromContradictions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        string
		declared   []uint32 // nil omits the attribute
		want       string
		invalid    bool
		diagnostic int64
	}{
		{name: "authored odd", raw: "abc", declared: []uint32{3}, want: "abc"},
		{name: "exact even ends in a genuine NUL", raw: "abc\x00", declared: []uint32{4}, want: "abc\x00"},
		{name: "a single padding byte", raw: "abc\x00", declared: []uint32{3}, want: "abc"},
		{name: "legacy without declaration", raw: "abc\x00", want: "abc"},
		{name: "short declaration", raw: "PA-ZZZ", declared: []uint32{2}, invalid: true, diagnostic: 2},
		{name: "multiple trailing NULs", raw: "abc\x00\x00\x00", declared: []uint32{3}, invalid: true, diagnostic: 3},
		{name: "nonzero pad candidate", raw: "abcd", declared: []uint32{3}, invalid: true, diagnostic: 3},
		{name: "pad after an even declaration", raw: "ab\x00", declared: []uint32{2}, invalid: true, diagnostic: 2},
		{name: "length beyond value", raw: "ab", declared: []uint32{3}, invalid: true, diagnostic: 3},
		{name: "zero declaration for nonempty value", raw: "ab", declared: []uint32{0}, invalid: true, diagnostic: 0},
		{name: "multiple declared lengths", raw: "abcd", declared: []uint32{2, 4}, invalid: true, diagnostic: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := object.New(nil)
			obj.Put(stringElement(tagSOPClassUID, core.VRUI, EncapsulatedOBJStorage))
			obj.Put(core.NewRawElement(tagEncapsulatedDocument, core.VROB, []byte(tc.raw)))
			if tc.declared != nil {
				obj.SetUL(tagEncapsulatedDocumentLength, tc.declared...)
			}
			doc, err := Extract(obj)
			if !tc.invalid {
				if err != nil || string(doc.Data) != tc.want || doc.DeclaredLength != (tc.declared != nil) {
					t.Fatalf("Extract returned size %d, declared %t, error %v", len(doc.Data), doc.DeclaredLength, err)
				}
				return
			}
			var mismatch *DocumentLengthError
			if len(doc.Data) != 0 || !errors.Is(err, ErrInvalidDocument) || !errors.As(err, &mismatch) ||
				mismatch.StoredBytes != int64(len(tc.raw)) || mismatch.DeclaredBytes != tc.diagnostic {
				t.Fatalf("Extract returned size %d, error %v, typed diagnostic %+v", len(doc.Data), err, mismatch)
			}
			if strings.Contains(err.Error(), "PA-ZZZ") {
				t.Fatal("diagnostic exposed the document bytes")
			}
			var stored bytes.Buffer
			if _, err := obj.CopyValueTo(tagEncapsulatedDocument, &stored); err != nil || stored.String() != tc.raw {
				t.Fatalf("raw copy changed the encoded value after a contradiction: %v", err)
			}
		})
	}
}

func TestDeferredDocumentLengthChecksOnlyThePossiblePaddingByte(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared uint32
		tail     byte
		invalid  bool
		wantRead int
	}{
		{"single NUL pad", 4095, 0, false, 1},
		{"nonzero pad", 4095, 'X', true, 1},
		{"short declaration", 2, 'X', true, 0},
		{"exact declaration", 4096, 'X', false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := bytes.Repeat([]byte("p"), 4096)
			value[len(value)-1] = tc.tail
			f, err := NewPDF(documentOptions("2.25.1051810", []byte("%PDF-1.7\n%%EOF\n")))
			if err != nil {
				t.Fatal(err)
			}
			f.Dataset.Put(core.NewRawElement(tagEncapsulatedDocument, core.VROB, value))
			f.Dataset.SetUL(tagEncapsulatedDocumentLength, tc.declared)
			var wire bytes.Buffer
			if err := object.WriteFile(&wire, f); err != nil {
				t.Fatal(err)
			}
			source := &documentLengthReadSpy{Reader: bytes.NewReader(wire.Bytes())}
			parsed, err := object.ReadFileWithOptions(source, object.ReadFileOptions{InlineValueBytesThreshold: 64})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = parsed.Close() })
			elem, ok := parsed.Dataset.Get(tagEncapsulatedDocument)
			if !ok || elem.Value != nil {
				t.Fatal("fixture's encoded Value Field was not deferred")
			}
			source.read = 0
			pos, _ := source.Seek(0, io.SeekCurrent)
			info, err := InspectDocumentLength(context.Background(), parsed.Dataset)
			var mismatch *DocumentLengthError
			if errors.As(err, &mismatch) != tc.invalid || !tc.invalid && err != nil ||
				!info.HasDeclaration || info.StoredBytes != int64(len(value)) || info.DeclaredBytes != int64(tc.declared) ||
				source.read != tc.wantRead {
				t.Fatalf("InspectDocumentLength = %+v, %v, read %d bytes (want %d)", info, err, source.read, tc.wantRead)
			}
			if got, _ := source.Seek(0, io.SeekCurrent); got != pos {
				t.Fatalf("source position = %d, want restored %d", got, pos)
			}
			var stored bytes.Buffer
			if _, err := parsed.Dataset.CopyValueTo(tagEncapsulatedDocument, &stored); err != nil || !bytes.Equal(stored.Bytes(), value) {
				t.Fatalf("raw deferred copy changed stored bytes: %v", err)
			}
		})
	}
}

type documentLengthReadSpy struct {
	*bytes.Reader
	read int
}

func (r *documentLengthReadSpy) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}
