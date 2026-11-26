package object_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func incompleteObjectWire() []byte {
	b := []byte{0xe0, 0x7f, 0x10, 0, 'O', 'W', 0, 0, 12, 0, 0, 0}
	return append(b, 1, 2, 3, 4, 5, 6, 7, 8, 9)
}
func TestIncompletePixelDataObjectRawAndWriteGuards(t *testing.T) {
	wire := incompleteObjectWire()
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{AllowIncompletePixelData: true})
	if obj == nil || !errors.Is(err, object.ErrPartialRead) || !errors.Is(err, object.ErrDataSet) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("obj%v err%v", obj, err)
	}
	info, ok := obj.IncompletePixelData()
	if !ok || info.AvailableBytes != 9 || info.ValueOffset != 12 {
		t.Fatalf("%+v", info)
	}
	info.AvailableBytes = 0
	again, _ := obj.IncompletePixelData()
	if again.AvailableBytes != 9 {
		t.Fatal("report mutated")
	}
	for _, fn := range []func(io.Writer) error{
		func(w io.Writer) error { return object.WriteDataSet(w, obj, transfer.ExplicitVRLittleEndian) },
		func(w io.Writer) error {
			return object.WriteDataSetWithOptions(w, obj, transfer.ExplicitVRLittleEndian, object.WriteDataSetOptions{})
		},
		func(w io.Writer) error {
			return object.WriteFile(w, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian})
		},
	} {
		var b bytes.Buffer
		if err := fn(&b); !errors.Is(err, object.ErrPartialFile) || b.Len() != 0 {
			t.Fatalf("write%d %v", b.Len(), err)
		}
	}
	var validatedWrite bytes.Buffer
	if _, err := object.WriteDataSetWithValidation(context.Background(), &validatedWrite, obj, transfer.ExplicitVRLittleEndian, validation.Options{}); !errors.Is(err, object.ErrPartialFile) || validatedWrite.Len() != 0 {
		t.Fatalf("validated write %v", err)
	}
	if _, err := obj.ValidateDataSet(context.Background(), validation.Options{}); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("validate %v", err)
	}
	f := &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}
	if _, err := f.ValidateFile(context.Background(), validation.Options{}); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("file validate %v", err)
	}
	if clone, err := object.CloneFile(f); clone != nil || !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("clone %v", err)
	}
	// Inspection may stream actual present bytes; it never supplies the declared tail.
	var raw bytes.Buffer
	n, err := obj.CopyValueTo(core.TagPixelData, &raw)
	if err != nil || n != 9 || !bytes.Equal(raw.Bytes(), wire[12:]) {
		t.Fatalf("copy%d %v", n, err)
	}
	path := filepath.Join(t.TempDir(), "truncated.dcm")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := object.OpenDataSetWithOptions(path, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{AllowIncompletePixelData: true})
	if opened == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatalf("open %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestIncompletePixelDataObjectModesAndValidation(t *testing.T) {
	wire := incompleteObjectWire()
	tag := core.TagPixelData
	options := []object.ReadFileOptions{
		{AllowIncompletePixelData: true, SkipPixelData: true, DataSetTags: []core.Tag{}},
		{AllowIncompletePixelData: true, AllowPartialRead: true}, {AllowIncompletePixelData: true, StopBeforeTag: &tag}, {AllowIncompletePixelData: true, DeferPixelData: true}, {AllowIncompletePixelData: true, SkipPixelData: true}, {AllowIncompletePixelData: true, InlineValueBytesThreshold: 1}, {AllowIncompletePixelData: true, BulkDataDescriptor: &parser.BulkDataDescriptor{}},
	}
	for i, opts := range options {
		obj, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, opts)
		if obj != nil || !errors.Is(err, parser.ErrIncompletePixelDataOptions) {
			t.Fatalf("mode%d %v", i, err)
		}
	}
	if obj, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.DeflatedExplicitVRLittleEndian, object.ReadFileOptions{AllowIncompletePixelData: true}); obj != nil || !errors.Is(err, parser.ErrIncompletePixelDataOptions) {
		t.Fatalf("deflated %v", err)
	}
	// Uniform validation must not call completion hooks or publish an incomplete object.
	r, err := parser.NewReaderWithValidation(context.Background(), bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, parser.ReaderOptions{AllowIncompletePixelData: true}, validation.Options{})
	if err == nil {
		_, err = r.ReadDataSet()
	}
	if !errors.Is(err, parser.ErrIncompletePixelDataOptions) {
		t.Fatalf("validated %v", err)
	}
	r, err = parser.NewSelectiveReader(context.Background(), bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, parser.ReaderOptions{AllowIncompletePixelData: true}, parser.SelectiveReaderOptions{Select: func(context.Context, validation.Path, core.ElementHeader, int64) (parser.SelectiveDisposition, error) {
		t.Fatal("selector ran")
		return parser.SelectiveMaterialize, nil
	}})
	if err == nil {
		_, err = r.ReadDataSet()
	}
	if !errors.Is(err, parser.ErrIncompletePixelDataOptions) {
		t.Fatalf("selective %v", err)
	}
	// The same option does not relabel a complete ordinary Pixel Data value.
	clean := bytes.Clone(wire)
	binary.LittleEndian.PutUint32(clean[8:], 8)
	clean = clean[:20]
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(clean), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{AllowIncompletePixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := obj.IncompletePixelData(); ok {
		t.Fatal("complete flagged")
	}
}

func TestIncompletePixelDataDeferredMetadataLifetimeAndCharset(t *testing.T) {
	// A normal deferred Waveform Data value remains readable while the raw
	// dataset source is retained, but recovering Pixel Data has no deferred span.
	waveform := []byte{0, 0x54, 0x10, 0x10, 'O', 'B', 0, 0, 4, 0, 0, 0, 11, 22, 33, 44}
	charset := []byte{8, 0, 5, 0, 'C', 'S', 10, 0, 'I', 'S', 'O', '_', 'I', 'R', ' ', '1', '9', '2'}
	name := []byte{0x10, 0, 0x10, 0, 'P', 'N', 6, 0, 'J', 0xc3, 0xb6, 'r', 'g', ' '}
	wire := append(append(append(charset, name...), waveform...), incompleteObjectWire()...)
	path := filepath.Join(t.TempDir(), "partial-raw.dcm")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	obj, err := object.OpenDataSetWithOptions(path, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{AllowIncompletePixelData: true, DeferWaveformData: true})
	if obj == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatal(err)
	}
	text, ok := obj.GetString(core.NewTag(0x0010, 0x0010))
	if !ok || text != "Jörg" {
		t.Fatalf("charset %q", text)
	}
	var b bytes.Buffer
	n, err := obj.CopyValueTo(core.NewTag(0x5400, 0x1010), &b)
	if err != nil || n != 4 || !bytes.Equal(b.Bytes(), []byte{11, 22, 33, 44}) {
		t.Fatalf("deferred %v", err)
	}
	if err := obj.Close(); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	if _, err := obj.CopyValueTo(core.NewTag(0x5400, 0x1010), &b); err == nil {
		t.Fatal("closed source still readable")
	}
	if _, _, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{AllowIncompletePixelData: true}, validation.Options{}); err == nil {
		t.Fatal("validated publication")
	}
}

type incompleteForgedErrorReader struct {
	source *bytes.Reader
	err    error
}

func (r incompleteForgedErrorReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if err == io.EOF {
		return n, r.err
	}
	return n, err
}
func TestIncompletePixelDataSourceCannotForgeCertificate(t *testing.T) {
	boom := errors.New("unrelated source failure")
	injected := errors.Join(object.ErrIncompletePixelData, boom)
	obj, err := object.ReadDataSetWithOptions(incompleteForgedErrorReader{source: bytes.NewReader(incompleteObjectWire()), err: injected}, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{AllowIncompletePixelData: true})
	if obj != nil || !errors.Is(err, boom) || errors.Is(err, object.ErrPartialRead) {
		t.Fatalf("forged dataset result%v err%v", obj, err)
	}

}
func TestIncompletePixelDataFileSourceCannotForgeCertificate(t *testing.T) {
	boom := errors.New("unrelated source failure")
	injected := errors.Join(object.ErrIncompletePixelData, boom)
	source := object.New(nil)
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1312.2"}})
	source.Put(core.NewRawElement(core.TagPixelData, core.VROW, make([]byte, 12)))
	var b bytes.Buffer
	if err := object.WriteFile(&b, &object.File{Dataset: source, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFileWithOptions(incompleteForgedErrorReader{source: bytes.NewReader(b.Bytes()[:b.Len()-3]), err: injected}, object.ReadFileOptions{AllowIncompletePixelData: true})
	if file != nil || !errors.Is(err, boom) || errors.Is(err, object.ErrPartialRead) {
		t.Fatalf("forged File result%v err%v", file, err)
	}
}

func TestIncompletePixelDataReplacedDatasetCannotBypassWriteRefusal(t *testing.T) {
	source := object.New(nil)
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1312.3"}})
	source.Put(core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}))
	var original bytes.Buffer
	if err := object.WriteFile(&original, &object.File{Dataset: source, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFileWithOptions(bytes.NewReader(original.Bytes()[:original.Len()-1]), object.ReadFileOptions{AllowIncompletePixelData: true})
	if file == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatal(err)
	}
	file.Dataset = object.FromElements(file.Dataset.Elements(), nil)
	if _, certified := file.Dataset.IncompletePixelData(); certified {
		t.Fatal("FromElements imported private certificate")
	}
	if _, certified := file.IncompletePixelData(); certified {
		t.Fatal("replaced Dataset retained decode authority")
	}
	var out bytes.Buffer
	err = object.WriteFileWithOptions(&out, file, object.WriteFileOptions{AllowPartial: true})
	if !errors.Is(err, object.ErrPartialFile) || out.Len() != 0 {
		t.Fatalf("replaced incomplete File write bytes%d err%v", out.Len(), err)
	}
}

type incompletePreludeFailureReader struct {
	*bytes.Reader
	failure  error
	injected bool
}

func (r *incompletePreludeFailureReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if !r.injected && n > 0 {
		r.injected = true
		return n, r.failure
	}
	return n, err
}
func TestIncompletePixelDataPreludeFullReadFailureCannotDisappear(t *testing.T) {
	source := object.New(nil)
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1312.4"}})
	source.Put(core.NewRawElement(core.TagPixelData, core.VROW, make([]byte, 12)))
	var b bytes.Buffer
	if err := object.WriteFile(&b, &object.File{Dataset: source, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("full-count Part10 preamble source failure")
	stream := &incompletePreludeFailureReader{Reader: bytes.NewReader(b.Bytes()[:b.Len()-3]), failure: boom}
	out, err := object.ReadFileWithOptions(stream, object.ReadFileOptions{AllowIncompletePixelData: true})
	if out != nil || !errors.Is(err, boom) || errors.Is(err, object.ErrPartialRead) {
		t.Fatalf("prelude failure hidden output%v err%v", out, err)
	}
}

type incompleteObjectGarbageFailureReader struct {
	*bytes.Reader
	at          int64
	failure     error
	readAtCount int
	fired       bool
}

func (r *incompleteObjectGarbageFailureReader) Read(p []byte) (int, error) {
	at, _ := r.Seek(0, io.SeekCurrent)
	n, err := r.Reader.Read(p)
	if r.readAtCount == 0 && !r.fired && at == r.at {
		r.fired = true
		return n, r.failure
	}
	return n, err
}
func (r *incompleteObjectGarbageFailureReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.Reader.ReadAt(p, off)
	if r.readAtCount > 0 && len(p) == 8 && off == r.Size()-8 {
		return min(n, r.readAtCount), r.failure
	}
	return n, err
}
func TestIncompletePixelDataGarbageTailIOCannotPublishObject(t *testing.T) {
	boom := errors.New("garbage tail source failure")
	tail := []byte{0x0c, 0xb0, 0xb6, 0x0e, 0x50, 0xd1, 0x02, 0x0e}
	source := object.New(nil)
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1312.5"}})
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: [][]byte{{0xff, 0xd8, 0xff, 0xd9}}}})
	var raw, part10 bytes.Buffer
	if err := object.WriteDataSet(&raw, source, transfer.JPEGBaseline); err != nil {
		t.Fatal(err)
	}
	if err := object.WriteFile(&part10, &object.File{Dataset: source, TransferSyntax: transfer.JPEGBaseline}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		failure     error
		readAtCount int
	}{{"plain", boom, 0}, {"forged", errors.Join(object.ErrIncompletePixelData, boom), 0}, {"readat-full", boom, 8}, {"readat-partial", boom, 2}} {
		for _, fileAPI := range []bool{false, true} {
			mode := "dataset"
			wire := raw.Bytes()
			if fileAPI {
				mode = "file"
				wire = part10.Bytes()
			}
			t.Run(tc.name+"_"+mode, func(t *testing.T) {
				wire := append(bytes.Clone(wire[:len(wire)-8]), tail...)
				reader := &incompleteObjectGarbageFailureReader{Reader: bytes.NewReader(wire), at: int64(len(wire) - 2), failure: tc.failure, readAtCount: tc.readAtCount}
				if fileAPI {
					out, err := object.ReadFileWithOptions(reader, object.ReadFileOptions{AllowIncompletePixelData: true})
					if out != nil || !errors.Is(err, boom) {
						t.Fatalf("garbage wrapper I/O published File%v err%v", out, err)
					}
				} else {
					out, err := object.ReadDataSetWithOptions(reader, transfer.JPEGBaseline, object.ReadFileOptions{AllowIncompletePixelData: true})
					if out != nil || !errors.Is(err, boom) {
						t.Fatalf("garbage wrapper I/O published Object%v err%v", out, err)
					}
				}
			})
		}
	}
}

// A named source field intentionally exposes only Read. The meta case feeds
// exactly the prefix first, so the I/O failure is first observed inside the
// File Meta read rather than during the preamble read.
type incompleteNonSeekPreludeReader struct {
	source        *bytes.Reader
	beforeFailure int
	failure       error
	injected      bool
}

func (r *incompleteNonSeekPreludeReader) Read(p []byte) (int, error) {
	if r.beforeFailure > 0 {
		p = p[:min(len(p), r.beforeFailure)]
		n, err := r.source.Read(p)
		r.beforeFailure -= n
		return n, err
	}
	n, err := r.source.Read(p)
	if !r.injected && n > 0 {
		r.injected = true
		return n, r.failure
	}
	return n, err
}

func TestIncompletePixelDataNonSeekPreludeFailureRetainsCause(t *testing.T) {
	source := object.New(nil)
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1312.17"}})
	source.Put(core.NewRawElement(core.TagPixelData, core.VROW, make([]byte, 12)))
	var b bytes.Buffer
	if err := object.WriteFile(&b, &object.File{Dataset: source, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	for _, where := range []string{"preamble", "meta"} {
		t.Run(where, func(t *testing.T) {
			wire := bytes.Clone(b.Bytes()[:b.Len()-3])
			beforeFailure := 0
			if where == "preamble" {
				wire[128] = 'X'
			} else {
				wire[136], wire[137] = 'Z', 'Z'
				beforeFailure = 132
			}
			boom := errors.New("buffered nonseekable Part10 " + where + " source failure")
			stream := &incompleteNonSeekPreludeReader{source: bytes.NewReader(wire), beforeFailure: beforeFailure, failure: boom}
			if _, seekable := any(stream).(io.ReadSeeker); seekable {
				t.Fatal("fixture unexpectedly exposes Seek")
			}
			out, err := object.ReadFileWithOptions(stream, object.ReadFileOptions{AllowIncompletePixelData: true})
			if !stream.injected {
				t.Fatal("fixture did not inject the source failure")
			}
			if out != nil || !errors.Is(err, boom) || errors.Is(err, object.ErrPartialRead) {
				t.Fatalf("nonseek prelude cause lost output%v err%v", out, err)
			}
		})
	}
}
