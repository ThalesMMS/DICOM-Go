package object_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/iconimage"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func prefixWire(order binary.ByteOrder, tag core.Tag, vr string, length uint32, value []byte) []byte {
	b := make([]byte, 4)
	order.PutUint16(b, tag.Group)
	order.PutUint16(b[2:], tag.Element)
	if vr != "" {
		b = append(b, vr...)
		if vr == "SQ" || vr == "OB" || vr == "UN" {
			b = append(b, 0, 0)
		} else {
			at := len(b)
			b = append(b, 0, 0)
			order.PutUint16(b[at:], uint16(length))
			return append(b, value...)
		}
	}
	at := len(b)
	b = append(b, 0, 0, 0, 0)
	order.PutUint32(b[at:], length)
	return append(b, value...)
}

func prefixPart10(t *testing.T, syntax transfer.Syntax, dataset []byte) ([]byte, int) {
	t.Helper()
	meta := []byte{}
	for _, value := range []struct {
		tag  core.Tag
		text string
	}{
		{core.NewTag(2, 2), "1.2.840.10008.5.1.4.1.1.7"}, {core.NewTag(2, 3), "1.2.3.4"}, {core.NewTag(2, 0x10), syntax.UID},
	} {
		raw := []byte(value.text)
		if len(raw)%2 != 0 {
			raw = append(raw, 0)
		}
		meta = append(meta, prefixWire(binary.LittleEndian, value.tag, "UI", uint32(len(raw)), raw)...)
	}
	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, uint32(len(meta)))
	meta = append(prefixWire(binary.LittleEndian, core.NewTag(2, 0), "UL", 4, length), meta...)
	wire := append(make([]byte, 128), []byte("DICM")...)
	wire = append(wire, meta...)
	offset := len(wire)
	if syntax.Deflated {
		var compressed bytes.Buffer
		w, err := flate.NewWriter(&compressed, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(dataset); err != nil {
			t.Fatal(err)
		}
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
		dataset = compressed.Bytes()
	}
	return append(wire, dataset...), offset
}

func prefixIdentity(order binary.ByteOrder) []byte {
	dataset := prefixWire(order, core.NewTag(8, 0x16), "UI", 26, []byte("1.2.840.10008.5.1.4.1.1.7\x00"))
	// The class UID has 25 characters; its even encoded length is 26.
	return append(dataset, prefixWire(order, core.NewTag(8, 0x18), "UI", 8, []byte("1.2.3.4\x00"))...)
}

type prefixAdversarialSource struct {
	r             *bytes.Reader
	forbid        int64
	reads, closes int
	mutate        *core.Tag
}

func (r *prefixAdversarialSource) Read(p []byte) (int, error) {
	if r.mutate != nil {
		*r.mutate = core.NewTag(0, 0)
		r.mutate = nil
	}
	pos, _ := r.r.Seek(0, io.SeekCurrent)
	if r.forbid > 0 && (pos >= r.forbid || int64(len(p)) > r.forbid-pos) {
		return 0, errors.New("prefix consumed stopped value")
	}
	r.reads++
	return r.r.Read(p)
}
func (r *prefixAdversarialSource) Seek(n int64, w int) (int64, error) { return r.r.Seek(n, w) }
func (r *prefixAdversarialSource) Close() error                       { r.closes++; return nil }

func TestFilePrefixStoppedHeaderIncompleteAndNoOutput(t *testing.T) {
	threshold := core.NewTag(0x10, 0x10)
	leaf := prefixWire(binary.LittleEndian, core.NewTag(0x28, 0x10), "US", 2, []byte{7, 0}) // > threshold, but nested
	item := prefixWire(binary.LittleEndian, core.NewTag(0xfffe, 0xe000), "", uint32(len(leaf)), leaf)
	dataset := prefixIdentity(binary.LittleEndian)
	dataset = append(dataset, prefixWire(binary.LittleEndian, core.NewTag(8, 0x1111), "SQ", uint32(len(item)), item)...)
	stopOffset := len(dataset)
	dataset = append(dataset, prefixWire(binary.LittleEndian, threshold, "PN", 100, nil)...)
	wire, base := prefixPart10(t, transfer.ExplicitVRLittleEndian, dataset)
	before := append([]byte(nil), wire...)
	source := &prefixAdversarialSource{r: bytes.NewReader(wire), forbid: int64(len(wire)), mutate: &threshold}
	f, err := object.ReadFileWithOptions(source, object.ReadFileOptions{StopBeforeTag: &threshold})
	var stopErr *object.ReadStopError
	if f == nil || !errors.Is(err, object.ErrReadStopped) || !errors.Is(err, object.ErrPartialRead) || !errors.As(err, &stopErr) {
		t.Fatalf("file %v error %v", f != nil, err)
	}
	stop, ok := f.ReadStop()
	if !ok || !stop.Stopped || stop.Header.Tag != core.NewTag(0x10, 0x10) || stop.Offset != int64(base+stopOffset) {
		t.Fatalf("stop %+v, expected %d", stop, base+stopOffset)
	}
	partial, ok := f.PartialRead()
	if !ok || partial.Elements != 3 || partial.Offset != stop.Offset {
		t.Fatalf("partial %+v", partial)
	}
	seq, _ := f.Dataset.Get(core.NewTag(8, 0x1111))
	if len(seq.Value.(core.SequenceValue).Items) != 1 {
		t.Fatal("pre-cut sequence lost")
	}
	if got := seq.Value.(core.SequenceValue).Items[0].Elements[0]; got.Header.Tag != core.NewTag(0x28, 0x10) || !bytes.Equal([]byte(got.Value.(core.RawValue)), []byte{7, 0}) {
		t.Fatalf("nested element %+v", got)
	}
	stopErr.Stop.Header.Tag = core.NewTag(0, 0)
	stopErr.Elements = 0
	stop.Header.Tag = core.NewTag(0, 0)
	fresh, _ := f.ReadStop()
	if fresh.Header.Tag != core.NewTag(0x10, 0x10) {
		t.Fatal("descriptor alias")
	}
	if _, err := f.ValidateFile(context.Background(), validation.Options{}); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("validation: %v", err)
	}
	if _, err := f.ValidatePart10Identity(); err != nil {
		t.Fatalf("focused identity: %v", err)
	}
	var output bytes.Buffer
	if err := object.WriteFile(&output, f); !errors.Is(err, object.ErrPartialFile) || output.Len() != 0 {
		t.Fatalf("write emitted %d: %v", output.Len(), err)
	}
	if err := object.WriteFileFrames(&output, f, nil, object.FrameWriteOptions{}); !errors.Is(err, object.ErrPartialFile) || output.Len() != 0 {
		t.Fatalf("frames emitted %d: %v", output.Len(), err)
	}
	if _, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{}); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("decompress: %v", err)
	}
	if _, err := iconimage.Generate(context.Background(), f, iconimage.GenerateOptions{}); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("icon: %v", err)
	}
	if err := object.WriteFileWithOptions(&output, f, object.WriteFileOptions{AllowPartial: true}); err != nil {
		t.Fatalf("explicit truncated-output override: %v", err)
	}
	if err := f.Close(); err != nil || source.closes != 0 {
		t.Fatalf("borrowed source closed %d: %v", source.closes, err)
	}
	if !bytes.Equal(wire, before) {
		t.Fatal("source changed")
	}
}

func TestFilePrefixEOFMalformedAndFiniteAllocation(t *testing.T) {
	threshold := core.NewTag(0xffff, 0xffff)
	wire, _ := prefixPart10(t, transfer.ExplicitVRLittleEndian, prefixIdentity(binary.LittleEndian))
	f, err := object.ReadFileWithOptions(bytes.NewReader(wire), object.ReadFileOptions{StopBeforeTag: &threshold})
	if err != nil || f == nil {
		t.Fatalf("complete EOF %v", err)
	}
	if _, ok := f.PartialRead(); ok {
		t.Fatal("EOF marked partial")
	}
	if _, ok := f.ReadStop(); ok {
		t.Fatal("EOF marked stopped")
	}
	legacy, err := object.ReadFile(bytes.NewReader(wire))
	if err != nil || len(legacy.Dataset.Elements()) != len(f.Dataset.Elements()) {
		t.Fatalf("default read %v", err)
	}
	for _, tc := range []struct {
		name string
		tail []byte
		opts object.ReadFileOptions
		want error
	}{
		{"truncated value", prefixWire(binary.LittleEndian, core.NewTag(0x10, 0x10), "PN", 4, []byte("AB")), object.ReadFileOptions{StopBeforeTag: &threshold}, io.ErrUnexpectedEOF},
		{"finite default value", prefixWire(binary.LittleEndian, core.NewTag(0x11, 0x1010), "OB", (1<<20)+2, nil), object.ReadFileOptions{StopBeforeTag: &threshold}, parser.ErrMaxElementBytesExceeded},
		{"header budget", prefixWire(binary.LittleEndian, core.NewTag(0x10, 0x10), "PN", 2, []byte("AB")), object.ReadFileOptions{StopBeforeTag: &threshold, PrefixMaxTokens: 2}, parser.ErrMaxSelectiveTokensExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(prefixIdentity(binary.LittleEndian), tc.tail...)
			wire, _ := prefixPart10(t, transfer.ExplicitVRLittleEndian, data)
			if f, err := object.ReadFileWithOptions(bytes.NewReader(wire), tc.opts); f != nil || !errors.Is(err, tc.want) || errors.Is(err, object.ErrReadStopped) {
				t.Fatalf("file %v error %v", f != nil, err)
			}
		})
	}
}

type prefixSink struct{ count int }

func (s *prefixSink) HandleFrame(parser.Frame) error { s.count++; return nil }
func (s *prefixSink) Close() error                   { s.count++; return nil }

func TestFilePrefixInvalidOptionsBeforeIOAndSink(t *testing.T) {
	threshold := core.NewTag(0x10, 0x10)
	sink := &prefixSink{}
	for _, opts := range []object.ReadFileOptions{
		{PrefixMaxTokens: 1}, {StopBeforeTag: &threshold, PrefixMaxTokens: -1}, {StopBeforeTag: &threshold, MaxSequenceDepth: -1},
		{StopBeforeTag: &threshold, MaxTotalBytes: -1}, {StopBeforeTag: &threshold, AllowPartialRead: true}, {StopBeforeTag: &threshold, FrameSink: sink},
	} {
		s := &prefixAdversarialSource{r: bytes.NewReader(nil)}
		if _, err := object.ReadFileWithOptions(s, opts); !errors.Is(err, object.ErrReadPrefixOptions) || s.reads != 0 || sink.count != 0 {
			t.Fatalf("options %+v reads %d sink %d error %v", opts, s.reads, sink.count, err)
		}
	}
	if _, err := object.ReadFileWithOptions(io.LimitReader(bytes.NewReader(nil), 0), object.ReadFileOptions{StopBeforeTag: &threshold}); !errors.Is(err, object.ErrReadPrefixRequiresSeekable) {
		t.Fatalf("nonseekable: %v", err)
	}
	opts := object.ReadFileOptions{StopBeforeTag: &threshold, FrameSink: sink}
	s := &prefixAdversarialSource{r: bytes.NewReader(nil)}
	if _, err := object.ReadDataSetWithOptions(s, transfer.ExplicitVRLittleEndian, opts); !errors.Is(err, object.ErrReadPrefixOptions) {
		t.Fatal(err)
	}
	if _, _, err := object.ReadDataSetWithValidation(context.Background(), s, transfer.ExplicitVRLittleEndian, opts, validation.Options{}); !errors.Is(err, object.ErrReadPrefixOptions) {
		t.Fatal(err)
	}
	if _, _, err := object.ReadFileWithTransferSyntaxRecovery(s, opts, object.TransferSyntaxRecoveryOptions{}); !errors.Is(err, object.ErrReadPrefixOptions) {
		t.Fatal(err)
	}
	if _, _, err := object.ReadDataSetWithTransferSyntaxRecovery(s, opts, object.TransferSyntaxRecoveryOptions{}); !errors.Is(err, object.ErrReadPrefixOptions) {
		t.Fatal(err)
	}
	if _, _, err := object.OpenFileWithRecoveryLadder("must-not-open", opts, nil); !errors.Is(err, object.ErrReadPrefixOptions) {
		t.Fatal(err)
	}
	if _, err := object.OpenDataSetWithOptions("must-not-open", transfer.ExplicitVRLittleEndian, opts); !errors.Is(err, object.ErrReadPrefixOptions) {
		t.Fatal(err)
	}
	if _, _, err := object.OpenFileWithTransferSyntaxRecovery("must-not-open", opts, object.TransferSyntaxRecoveryOptions{}); !errors.Is(err, object.ErrReadPrefixOptions) {
		t.Fatal(err)
	}
	if s.reads != 0 || sink.count != 0 {
		t.Fatalf("reads/sink %d/%d", s.reads, sink.count)
	}
}

func TestFilePrefixEndianAndDeflateLogicalOffsets(t *testing.T) {
	threshold := core.NewTag(0x10, 0x10)
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			data := prefixIdentity(syntax.ByteOrder)
			offset := len(data)
			data = append(data, prefixWire(syntax.ByteOrder, threshold, "PN", 2, []byte("AB"))...)
			wire, base := prefixPart10(t, syntax, data)
			f, err := object.ReadFileWithOptions(bytes.NewReader(wire), object.ReadFileOptions{StopBeforeTag: &threshold})
			if !errors.Is(err, object.ErrReadStopped) {
				t.Fatal(err)
			}
			stop, _ := f.ReadStop()
			if stop.Offset != int64(base+offset) || f.TransferSyntax.UID != syntax.UID || f.Dataset.ValueByteOrder() != syntax.ByteOrder {
				t.Fatalf("stop %+v syntax %+v", stop, f.TransferSyntax)
			}
		})
	}
}

func TestFilePrefixDeferredPathLifetime(t *testing.T) {
	threshold := core.NewTag(0xfffc, 0xfffc)
	dataset := prefixIdentity(binary.LittleEndian)
	pixelOffset := len(dataset) + 12
	dataset = append(dataset, prefixWire(binary.LittleEndian, core.TagPixelData, "OB", 8, []byte{1, 2, 3, 4, 5, 6, 7, 8})...)
	dataset = append(dataset, prefixWire(binary.LittleEndian, threshold, "OB", 2, []byte{0, 0})...)
	wire, base := prefixPart10(t, transfer.ExplicitVRLittleEndian, dataset)
	path := filepath.Join(t.TempDir(), "prefix.dcm")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := object.OpenFileWithOptions(path, object.ReadFileOptions{StopBeforeTag: &threshold, DeferPixelData: true})
	if !errors.Is(err, object.ErrReadStopped) || f == nil {
		t.Fatal(err)
	}
	locations := f.ValueLocations(core.TagPixelData)
	if len(locations) != 1 || locations[0].ValueOffset != int64(base+pixelOffset) {
		t.Fatalf("locations %+v", locations)
	}
	var value bytes.Buffer
	n, err := f.Dataset.CopyValueTo(core.TagPixelData, &value)
	if err != nil || n != 8 || !bytes.Equal(value.Bytes(), []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("replay %d/%x: %v", n, value.Bytes(), err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Dataset.CopyValueTo(core.TagPixelData, io.Discard); err == nil {
		t.Fatal("owned source survived Close")
	}
}

func TestFilePrefixRetainedCharsetAndSectionCoordinates(t *testing.T) {
	threshold := core.NewTag(0x10, 0x20)
	dataset := prefixWire(binary.LittleEndian, core.NewTag(8, 5), "CS", 10, []byte("ISO_IR 100"))
	dataset = append(dataset, prefixIdentity(binary.LittleEndian)...)
	dataset = append(dataset, prefixWire(binary.LittleEndian, core.NewTag(0x10, 0x10), "PN", 6, []byte{'M', 0xfc, 'l', 'l', 'e', 'r'})...)
	stopOffset := len(dataset)
	dataset = append(dataset, prefixWire(binary.LittleEndian, threshold, "LO", 2, []byte("ID"))...)
	wire, base := prefixPart10(t, transfer.ExplicitVRLittleEndian, dataset)
	// Section offsets are intentionally value-relative; no outer file prefix
	// enters the logical parser coordinates or retained deferred locations.
	container := append(make([]byte, 37), wire...)
	section := io.NewSectionReader(bytes.NewReader(container), 37, int64(len(wire)))
	f, err := object.ReadFileWithOptions(section, object.ReadFileOptions{StopBeforeTag: &threshold})
	if !errors.Is(err, object.ErrReadStopped) {
		t.Fatal(err)
	}
	names, present := f.Dataset.GetStrings(core.NewTag(0x10, 0x10))
	if !present || len(names) != 1 || names[0] != "Müller" {
		t.Fatalf("name %v, present %v", names, present)
	}
	stop, _ := f.ReadStop()
	if stop.Offset != int64(base+stopOffset) {
		t.Fatalf("section stop %d want %d", stop.Offset, base+stopOffset)
	}
}
