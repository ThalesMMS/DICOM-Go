package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

var partialFileSQ = core.NewTag(0x0008, 0x1110)
var partialFileLeaf = core.NewTag(0x0008, 0x1155)

func partialSequenceFile(t *testing.T, syntax transfer.Syntax, first, second []byte) []byte {
	t.Helper()
	base, err := dicomtest.Part10File(syntax, dicomtest.MinimalDataset()...)
	if err != nil {
		t.Fatal(err)
	}
	control := func(tag core.Tag, length uint32) []byte {
		return dicomtest.SequenceControlBytes(syntax.ByteOrder, tag, length)
	}
	tail := bytes.Join([][]byte{dicomtest.SequenceHeaderBytes(syntax, partialFileSQ, uint32(core.UndefinedLength)), control(core.TagItem, uint32(core.UndefinedLength)), first, control(core.TagItemDelimitationItem, 0), control(core.TagItem, uint32(core.UndefinedLength)), second}, nil)
	return append(base, tail...)
}

func TestPartialFileSequenceSummaryAndConfirmedRewrite(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			leaf := dicomtest.EncodeElement(dicomtest.NewUIElement(partialFileLeaf, "1.2.3"), syntax)
			bad := dicomtest.EncodeElement(dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "INCOMPLETE"), syntax)
			wire := partialSequenceFile(t, syntax, leaf, append(append([]byte(nil), leaf...), bad[:len(bad)-3]...))
			for _, seekable := range []bool{false, true} {
				var source io.Reader = bytes.NewReader(wire)
				if !seekable {
					source = io.MultiReader(source)
				}
				file, err := ReadFileWithOptions(source, ReadFileOptions{AllowPartialRead: true})
				var partialErr *PartialReadError
				var parse *parser.ParseError
				if file == nil || !errors.Is(err, ErrPartialRead) || !errors.Is(err, ErrDataSet) || !errors.As(err, &partialErr) || !errors.As(err, &parse) {
					t.Fatalf("partial File = %v", err)
				}
				read, partial := file.PartialRead()
				if !partial || read.Elements != len(file.Dataset.Elements())-1 || read.RetainedElements != len(file.Dataset.Elements()) || len(read.Containers) != 2 || read.Containers[1].ItemIndex != 1 {
					t.Fatalf("summary = %+v", read)
				}
				if got := read.Path.String(); got != partialFileSQ.String()+"[1]/(0010,0010)" {
					t.Fatalf("path = %s", got)
				}
				items, ok := file.GetSequence(partialFileSQ)
				if !ok || len(items) != 2 || !items[0].Has(partialFileLeaf) || !items[1].Has(partialFileLeaf) || items[1].Has(core.NewTag(0x0010, 0x0010)) {
					t.Fatal("wrong recovered items")
				}
				// Both summaries are owned; mutation cannot change the File marker.
				read.Path[0].ItemIndex = 88
				read.Containers[0].Retained = false
				partialErr.Read.Path[0].ItemIndex = 99
				partialErr.Read.Containers[0].Retained = false
				again, _ := file.PartialRead()
				if again.Path[0].ItemIndex != 1 || !again.Containers[0].Retained {
					t.Fatal("partial metadata aliases caller slices")
				}
				var output bytes.Buffer
				if err := WriteFile(&output, file); !errors.Is(err, ErrPartialFile) || output.Len() != 0 {
					t.Fatalf("unchecked write = %v", err)
				}
				if _, err := file.ValidateFile(context.Background(), validation.Options{}); !errors.Is(err, ErrPartialFile) {
					t.Fatalf("validation = %v", err)
				}
				if err := WriteFileWithOptions(&output, file, WriteFileOptions{AllowPartial: true}); err != nil {
					t.Fatal(err)
				}
				rewritten, err := ReadFile(bytes.NewReader(output.Bytes()))
				if err != nil {
					t.Fatalf("confirmed rewrite is not structurally complete: %v", err)
				}
				if _, partial := rewritten.PartialRead(); partial {
					t.Fatal("rewritten prefix retains an old partial marker")
				}
				rewrittenItems, _ := rewritten.GetSequence(partialFileSQ)
				if len(rewrittenItems) != 2 || len(rewrittenItems[1].Elements()) != 1 {
					t.Fatal("rewrite changed retained prefix")
				}
			}
		})
	}
}

func TestPartialFileDefinedSequenceConfirmedRewrite(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			leaf := dicomtest.EncodeElement(dicomtest.NewUIElement(partialFileLeaf, "1.2.3"), syntax)
			wire := partialSequenceFile(t, syntax, leaf, leaf)
			header := dicomtest.SequenceHeaderBytes(syntax, partialFileSQ, uint32(core.UndefinedLength))
			offset := bytes.Index(wire, header)
			if offset < 0 {
				t.Fatal("SQ header not found")
			}
			// A defined outer sequence would end after the missing item delimiter.
			declared := uint32(len(wire) - offset - len(header) + 8)
			copy(wire[offset:], dicomtest.SequenceHeaderBytes(syntax, partialFileSQ, declared))
			file, err := ReadFileWithOptions(bytes.NewReader(wire), ReadFileOptions{AllowPartialRead: true})
			if file == nil || !errors.Is(err, ErrPartialRead) {
				t.Fatalf("defined prefix = %v", err)
			}
			read, _ := file.PartialRead()
			if len(read.Containers) != 2 || !read.Containers[0].Retained || !read.Containers[1].Retained {
				t.Fatalf("unmarked defined prefix: %+v", read)
			}
			var output bytes.Buffer
			if err := WriteFileWithOptions(&output, file, WriteFileOptions{AllowPartial: true}); err != nil {
				t.Fatal(err)
			}
			complete, err := ReadFile(bytes.NewReader(output.Bytes()))
			if err != nil {
				t.Fatalf("defined rewrite = %v", err)
			}
			items, _ := complete.GetSequence(partialFileSQ)
			if len(items) != 2 || !items[1].Has(partialFileLeaf) {
				t.Fatal("defined prefix lost on rewrite")
			}
		})
	}
}

func TestPartialFileSequenceDeferredReferencesStayAuthenticated(t *testing.T) {
	blob := core.NewTag(0x0042, 0x0011)
	syntax := transfer.ExplicitVRLittleEndian
	makeWire := func(seed byte) ([]byte, [][]byte) {
		values := [][]byte{bytes.Repeat([]byte{seed}, 128), bytes.Repeat([]byte{seed + 1}, 128)}
		first := dicomtest.EncodeElement(core.NewRawElement(blob, core.VROB, values[0]), syntax)
		second := append(dicomtest.EncodeElement(core.NewRawElement(blob, core.VROB, values[1]), syntax), []byte{0x10, 0x00, 0x20}...)
		return partialSequenceFile(t, syntax, first, second), values
	}
	wire, want := makeWire(0x42)
	path := filepath.Join(t.TempDir(), "partial-sequence.dcm")
	if err := os.WriteFile(path, wire, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := ReadFileOptions{AllowPartialRead: true, InlineValueBytesThreshold: 64, DeferNestedValues: true}
	file, err := OpenFileWithOptions(path, opts)
	if file == nil || !errors.Is(err, ErrPartialRead) {
		t.Fatalf("open = %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	items, _ := file.GetSequence(partialFileSQ)
	for i, item := range items {
		elem, ok := item.Get(blob)
		if !ok || elem.Value != nil {
			t.Fatalf("blob %d not deferred", i)
		}
		var copied bytes.Buffer
		if _, err := file.Dataset.CopyNestedValueTo(item, blob, &copied); err != nil || !bytes.Equal(copied.Bytes(), want[i]) {
			t.Fatalf("nested replay %d = %v", i, err)
		}
		refPath := []parser.BulkDataPathStep{{Tag: partialFileSQ, ItemIndex: i}, {Tag: blob, ItemIndex: -1}}
		ref, ok, err := file.Dataset.DeferredBulkDataReference(refPath)
		if err != nil || !ok || ref.Length != 128 || ref.Offset < 0 || !bytes.Equal(wire[ref.Offset:ref.Offset+ref.Length], want[i]) || !reflect.DeepEqual(ref.Path, refPath) {
			t.Fatalf("reference %d = %+v / %v", i, ref, err)
		}
		copied.Reset()
		if _, err := file.Dataset.CopyBulkDataValueTo(context.Background(), refPath, &copied); err != nil || !bytes.Equal(copied.Bytes(), want[i]) {
			t.Fatalf("reference replay %d = %v", i, err)
		}
	}
	var rewritten bytes.Buffer
	if err := WriteFileWithOptions(&rewritten, file, WriteFileOptions{AllowPartial: true}); err != nil {
		t.Fatal(err)
	}
	complete, err := ReadFile(bytes.NewReader(rewritten.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	completeItems, _ := complete.GetSequence(partialFileSQ)
	for i, item := range completeItems {
		raw, ok := item.GetRaw(blob)
		if !ok || !bytes.Equal(raw, want[i]) {
			t.Fatal("deferred rewrite lost bytes")
		}
	}
	// Swapping in a different source's prefix must not authorize old bytes.
	foreignWire, _ := makeWire(0x73)
	foreign, err := ReadFileWithOptions(bytes.NewReader(foreignWire), opts)
	if foreign == nil || !errors.Is(err, ErrPartialRead) {
		t.Fatal(err)
	}
	foreignSQ, _ := foreign.Dataset.Get(partialFileSQ)
	file.Dataset.Put(foreignSQ)
	refPath := []parser.BulkDataPathStep{{Tag: partialFileSQ, ItemIndex: 1}, {Tag: blob, ItemIndex: -1}}
	if _, _, err := file.Dataset.DeferredBulkDataReference(refPath); err == nil {
		t.Fatal("foreign prefix forged a source reference")
	}
	if _, err := file.Dataset.CopyBulkDataValueTo(context.Background(), refPath, io.Discard); err == nil {
		t.Fatal("foreign prefix replayed old bytes")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Dataset.CopyNestedValueTo(items[1], blob, io.Discard); err == nil {
		t.Fatal("closed partial source remained replayable")
	}
}

func TestPartialFileSequenceCharsets(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	latinPN := dicomtest.EncodeElement(core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte{'A', 0xe9}), syntax)
	utf8Charset := dicomtest.EncodeElement(dicomtest.NewStringElement(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 192"), syntax)
	utf8PN := dicomtest.EncodeElement(dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "Bé"), syntax)
	// Parent charset is outside the interrupted SQ; the second item's complete
	// local declaration overrides it even though the item delimiter is absent.
	base, err := dicomtest.Part10File(syntax, append(dicomtest.MinimalDataset(), dicomtest.NewStringElement(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 100"))...)
	if err != nil {
		t.Fatal(err)
	}
	tail := bytes.Join([][]byte{dicomtest.SequenceHeaderBytes(syntax, partialFileSQ, uint32(core.UndefinedLength)), dicomtest.SequenceControlBytes(syntax.ByteOrder, core.TagItem, uint32(core.UndefinedLength)), latinPN, dicomtest.SequenceControlBytes(syntax.ByteOrder, core.TagItemDelimitationItem, 0), dicomtest.SequenceControlBytes(syntax.ByteOrder, core.TagItem, uint32(core.UndefinedLength)), utf8Charset, utf8PN}, nil)
	file, err := ReadFileWithOptions(bytes.NewReader(append(base, tail...)), ReadFileOptions{AllowPartialRead: true})
	if file == nil || !errors.Is(err, ErrPartialRead) {
		t.Fatal(err)
	}
	items, _ := file.GetSequence(partialFileSQ)
	for i, want := range []string{"Aé", "Bé"} {
		got, ok := items[i].GetString(core.NewTag(0x0010, 0x0010))
		if !ok || got != want {
			t.Fatalf("charset %d = %q", i, got)
		}
	}
}

type partialContextErrorReader struct{ err error }

func (r partialContextErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestPartialSequenceCancellationReturnsNoFile(t *testing.T) {
	leaf := dicomtest.EncodeElement(dicomtest.NewUIElement(partialFileLeaf, "1.2.3"), transfer.ExplicitVRLittleEndian)
	wire := partialSequenceFile(t, transfer.ExplicitVRLittleEndian, leaf, leaf)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		file, err := ReadFileWithOptions(io.MultiReader(bytes.NewReader(wire), partialContextErrorReader{cause}), ReadFileOptions{AllowPartialRead: true})
		if file != nil || !errors.Is(err, cause) || errors.Is(err, ErrPartialRead) {
			t.Fatalf("cancellation published partial file: %v", err)
		}
	}
}

func TestPartialFileValidationRefusesEOFAndLimits(t *testing.T) {
	base, _ := partialFixture(t)
	leaf := dicomtest.EncodeElement(dicomtest.NewUIElement(partialFileLeaf, "1.2.3"), transfer.ExplicitVRLittleEndian)
	for _, tc := range []struct {
		name string
		wire []byte
		opts ReadFileOptions
	}{
		{"truncated root", append(append([]byte(nil), base...), explicitElementBytes(0x0010, 0x0010, "PN", []byte("ABCD"))[:10]...), ReadFileOptions{AllowPartialRead: true}},
		{"open item EOF", partialSequenceFile(t, transfer.ExplicitVRLittleEndian, leaf, leaf), ReadFileOptions{AllowPartialRead: true}},
		{"depth limit", partialSequenceFile(t, transfer.ExplicitVRLittleEndian, leaf, leaf), ReadFileOptions{AllowPartialRead: true, MaxSequenceDepth: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := ReadFileWithOptions(bytes.NewReader(tc.wire), tc.opts)
			if file == nil || !errors.Is(err, ErrPartialRead) {
				t.Fatal(err)
			}
			if _, err := file.ValidateFile(context.Background(), validation.Options{}); !errors.Is(err, ErrPartialFile) {
				t.Fatalf("incomplete File validated: %v", err)
			}
		})
	}
	complete, err := ReadFileWithOptions(bytes.NewReader(base), ReadFileOptions{AllowPartialRead: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := complete.ValidateFile(context.Background(), validation.Options{}); err != nil {
		t.Fatalf("complete File refused: %v", err)
	}
}
