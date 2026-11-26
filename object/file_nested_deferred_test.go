package object

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

var (
	nestedTestSequenceTag = core.NewTag(0x0040, 0xA730) // Content Sequence
	nestedTestCreatorTag  = core.NewTag(0x0029, 0x0010)
	nestedTestBlobTag     = core.NewTag(0x0029, 0x1010)
	nestedTestTextTag     = core.NewTag(0x0008, 0x0104) // Code Meaning, LO
)

func patternBytes(n int, seed byte) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = seed + byte(i*7)
	}
	return data
}

// largeNestedValuePart10 builds a file with a large top-level Encapsulated
// Document and private blobs nested two items deep.
func largeNestedValuePart10(tb testing.TB, documentBytes, blobBytes int) []byte {
	tb.Helper()
	inner := core.DataSet{Elements: []core.Element{
		dicomtest.NewStringElement(nestedTestTextTag, core.VRLO, "inner"),
		dicomtest.NewStringElement(nestedTestCreatorTag, core.VRLO, "DICOM GO TEST"),
		dicomtest.NewOBElement(nestedTestBlobTag, patternBytes(blobBytes, 3)),
	}}
	elements := append([]core.Element{}, dicomtest.MinimalDataset()...)
	elements = append(elements,
		dicomtest.NewOBElement(core.NewTag(0x0042, 0x0011), patternBytes(documentBytes, 1)),
		dicomtest.NewSequenceElement(nestedTestSequenceTag,
			core.DataSet{Elements: []core.Element{
				dicomtest.NewStringElement(nestedTestTextTag, core.VRLO, "outer"),
				dicomtest.NewStringElement(nestedTestCreatorTag, core.VRLO, "DICOM GO TEST"),
				dicomtest.NewOBElement(nestedTestBlobTag, patternBytes(blobBytes, 5)),
				dicomtest.NewSequenceElement(nestedTestSequenceTag, inner),
			}},
		),
	)
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		tb.Fatal(err)
	}
	// Normalize once through WriteFile so the source is what dicom-go writes.
	file, err := ReadFile(bytes.NewReader(data))
	if err != nil {
		tb.Fatal(err)
	}
	var canonical bytes.Buffer
	if err := WriteFile(&canonical, file); err != nil {
		tb.Fatal(err)
	}
	return canonical.Bytes()
}

func openNestedTestFile(t *testing.T, data []byte, opts ReadFileOptions) *File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := OpenFileWithOptions(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestDeferNestedValuesStreamsLargeValuesInSequenceItems(t *testing.T) {
	data := largeNestedValuePart10(t, 4096, 2048)
	file := openNestedTestFile(t, data, ReadFileOptions{InlineValueBytesThreshold: 1024, DeferNestedValues: true})

	document, _ := file.Dataset.Get(core.NewTag(0x0042, 0x0011))
	if document.Value != nil {
		t.Fatalf("Encapsulated Document value = %T, want deferred", document.Value)
	}
	outer, ok := file.Dataset.GetSequence(nestedTestSequenceTag)
	if !ok || len(outer) != 1 {
		t.Fatalf("outer items = %d/%v", len(outer), ok)
	}
	inner, ok := outer[0].GetSequence(nestedTestSequenceTag)
	if !ok || len(inner) != 1 {
		t.Fatalf("inner items = %d/%v", len(inner), ok)
	}
	for _, check := range []struct {
		item *Object
		seed byte
	}{{outer[0], 5}, {inner[0], 3}} {
		blob, _ := check.item.Get(nestedTestBlobTag)
		if blob.Value != nil {
			t.Fatalf("nested blob value = %T, want deferred", blob.Value)
		}
		if text, _ := check.item.GetString(nestedTestTextTag); text == "" {
			t.Fatal("small nested LO was not materialized")
		}
		var value bytes.Buffer
		if _, err := file.Dataset.CopyNestedValueTo(check.item, nestedTestBlobTag, &value); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(value.Bytes(), patternBytes(2048, check.seed)) {
			t.Fatal("CopyNestedValueTo returned the wrong bytes")
		}
	}

	materialized, err := ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []WriteFileOptions{
		{},
		{SequenceLength: SequenceLengthDefined},
		{SequenceLength: SequenceLengthPreserve},
		{GroupLength: GroupLengthCreate},
		{SequenceLength: SequenceLengthDefined, GroupLength: GroupLengthCreate},
	} {
		var got, want bytes.Buffer
		if err := WriteFileWithOptions(&got, file, opts); err != nil {
			t.Fatalf("WriteFileWithOptions(%+v) deferred error = %v", opts, err)
		}
		if err := WriteFileWithOptions(&want, materialized, opts); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatalf("WriteFileWithOptions(%+v) of deferred nested values differs from the materialized rewrite", opts)
		}
	}
	var direct bytes.Buffer
	if err := WriteFile(&direct, file); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(direct.Bytes(), data) {
		t.Fatal("read, defer and rewrite did not reproduce the source bytes")
	}

	if _, err := WriteDataSetWithValidation(context.Background(), &bytes.Buffer{}, file.Dataset, transfer.ExplicitVRLittleEndian, validation.Options{}); !errors.Is(err, ErrDeferredSequenceValueWrite) {
		t.Fatalf("WriteDataSetWithValidation error = %v, want ErrDeferredSequenceValueWrite", err)
	}
}

func TestDeferNestedValuesOptions(t *testing.T) {
	data := largeNestedValuePart10(t, 4096, 2048)
	tests := []struct {
		name                    string
		opts                    ReadFileOptions
		documentDeferred, blobs bool
	}{
		{"top level only by default", ReadFileOptions{InlineValueBytesThreshold: 1024}, true, false},
		{"allow list keeps other tags", ReadFileOptions{InlineValueBytesThreshold: 1024, DeferNestedValues: true, DeferValueTags: []core.Tag{nestedTestBlobTag}}, false, true},
		{"threshold above the values", ReadFileOptions{InlineValueBytesThreshold: 8192, DeferNestedValues: true}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := openNestedTestFile(t, data, tt.opts)
			document, _ := file.Dataset.Get(core.NewTag(0x0042, 0x0011))
			if got := document.Value == nil; got != tt.documentDeferred {
				t.Fatalf("document deferred = %v, want %v", got, tt.documentDeferred)
			}
			outer, _ := file.Dataset.GetSequence(nestedTestSequenceTag)
			blob, _ := outer[0].Get(nestedTestBlobTag)
			if got := blob.Value == nil; got != tt.blobs {
				t.Fatalf("nested blob deferred = %v, want %v", got, tt.blobs)
			}
			var out bytes.Buffer
			if err := WriteFile(&out, file); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), data) {
				t.Fatal("rewrite changed the source bytes")
			}
		})
	}
}

func TestCopyNestedValueToRejectsItemsWithoutSourceOffset(t *testing.T) {
	data := largeNestedValuePart10(t, 16, 2048)
	file := openNestedTestFile(t, data, ReadFileOptions{InlineValueBytesThreshold: 1024, DeferNestedValues: true})
	outer, _ := file.Dataset.GetSequence(nestedTestSequenceTag)
	blob, _ := outer[0].Get(nestedTestBlobTag)
	detached := New(std.Dictionary)
	detached.Put(blob)
	if _, err := file.Dataset.CopyNestedValueTo(detached, nestedTestBlobTag, &bytes.Buffer{}); !errors.Is(err, ErrDeferredSequenceValueWrite) {
		t.Fatalf("CopyNestedValueTo(detached item) error = %v, want ErrDeferredSequenceValueWrite", err)
	}
}
