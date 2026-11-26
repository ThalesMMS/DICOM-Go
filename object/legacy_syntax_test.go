package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func legacyElement(order binary.AppendByteOrder, g, e uint16, value []byte) []byte {
	b := order.AppendUint16(nil, g)
	b = order.AppendUint16(b, e)
	b = order.AppendUint32(b, uint32(len(value)))
	return append(b, value...)
}

func us(order binary.AppendByteOrder, v uint16) []byte { return order.AppendUint16(nil, v) }

// legacyImage is an implicit VR image data set: two 16-bit pixels whose
// bytes are 01 02 03 04 in the stream, with integers in intOrder.
func legacyImage(intOrder binary.AppendByteOrder, bits uint16) []byte {
	var b []byte
	b = append(b, legacyElement(intOrder, 0x0008, 0x0016, []byte("1.2.840.10008.5.1.4.1.1.7\x00"))...)
	b = append(b, legacyElement(intOrder, 0x0008, 0x0018, []byte("1.2.3.4\x00"))...)
	b = append(b, legacyElement(intOrder, 0x0028, 0x0002, us(intOrder, 1))...)
	b = append(b, legacyElement(intOrder, 0x0028, 0x0004, []byte("MONOCHROME2 "))...)
	b = append(b, legacyElement(intOrder, 0x0028, 0x0010, us(intOrder, 1))...)
	b = append(b, legacyElement(intOrder, 0x0028, 0x0011, us(intOrder, 2))...)
	b = append(b, legacyElement(intOrder, 0x0028, 0x0100, us(intOrder, bits))...)
	b = append(b, legacyElement(intOrder, 0x0028, 0x0101, us(intOrder, bits))...)
	b = append(b, legacyElement(intOrder, 0x0028, 0x0102, us(intOrder, bits-1))...)
	b = append(b, legacyElement(intOrder, 0x0028, 0x0103, us(intOrder, 0))...)
	return append(b, legacyElement(intOrder, 0x7fe0, 0x0010, []byte{1, 2, 3, 4})...)
}

// geFile is a Part 10 file declaring the GE private syntax.
func geFile(t *testing.T, bits uint16) []byte {
	t.Helper()
	obj := New(nil)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.3.4"}})
	var buf bytes.Buffer
	if err := WriteFile(&buf, &File{Dataset: obj, TransferSyntax: transfer.ImplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	offset := 132 + 12 + int(binary.LittleEndian.Uint32(data[140:144]))
	meta := bytes.Replace(data[:offset], []byte("1.2.840.10008.1.2\x00"), []byte(transfer.PrivateGEImplicitVRBigEndianPixelData.UID), 1)
	return append(meta, legacyImage(binary.LittleEndian, bits)...)
}

var legacyRecovery = TransferSyntaxRecoveryOptions{AllowMissingPreamble: true, AllowMissingFileMeta: true, AllowLegacySyntaxes: true}

func TestLegacyACRNEMAImplicitVRBigEndian(t *testing.T) {
	data := legacyImage(binary.BigEndian, 16)
	if _, _, err := ReadFileWithTransferSyntaxRecovery(bytes.NewReader(data), ReadFileOptions{}, TransferSyntaxRecoveryOptions{AllowMissingPreamble: true}); err == nil {
		t.Fatal("recovery without legacy syntaxes accepted Implicit VR Big Endian")
	}
	f, res, err := ReadFileWithTransferSyntaxRecovery(bytes.NewReader(data), ReadFileOptions{}, legacyRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if res.Legacy.UID != transfer.ImplicitVRBigEndianACRNEMA.UID || res.Syntax.UID != transfer.ImplicitVRLittleEndian.UID ||
		f.TransferSyntax.UID != transfer.ImplicitVRLittleEndian.UID || res.Confidence < 0.75 {
		t.Fatalf("resolution %s", res)
	}
	if raw, _ := f.Dataset.GetRaw(core.NewTag(0x0028, 0x0011)); !bytes.Equal(raw, []byte{2, 0}) {
		t.Fatalf("columns % X", raw)
	}
	if raw, _ := f.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(raw, []byte{2, 1, 4, 3}) {
		t.Fatalf("pixel data % X", raw)
	}
	// The normalized File writes and reads back as standard Implicit VR Little Endian.
	var out bytes.Buffer
	if err := WriteFile(&out, f); err != nil {
		t.Fatal(err)
	}
	back, err := ReadFile(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := back.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(raw, []byte{2, 1, 4, 3}) {
		t.Fatalf("round trip pixel data % X", raw)
	}
	// Deferred values are swapped as they are copied.
	path := filepath.Join(t.TempDir(), "acr.dcm")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	lazy, _, err := OpenFileWithTransferSyntaxRecovery(path, ReadFileOptions{DeferPixelData: true}, legacyRecovery)
	if err != nil {
		t.Fatal(err)
	}
	defer lazy.Close()
	var got bytes.Buffer
	if _, err := lazy.Dataset.CopyValueTo(core.TagPixelData, &got); err != nil || !bytes.Equal(got.Bytes(), []byte{2, 1, 4, 3}) {
		t.Fatalf("deferred pixel data % X %v", got.Bytes(), err)
	}
	// Streaming sinks see values before normalization and are refused.
	_, _, err = ReadFileWithTransferSyntaxRecovery(bytes.NewReader(data), ReadFileOptions{FrameSink: FrameSinkFunc(func(parser.Frame) error { return nil })}, legacyRecovery)
	if !errors.Is(err, ErrLegacySyntaxFrameSink) {
		t.Fatalf("frame sink error = %v", err)
	}
}

func TestLegacyGEPrivateSyntax(t *testing.T) {
	data := geFile(t, 16)
	if _, err := ReadFile(bytes.NewReader(data)); err == nil {
		t.Fatal("strict read accepted the GE private syntax")
	}
	// Inference would read Big Endian pixels as Little Endian, so an unknown
	// UID policy does not apply to a legacy UID.
	if _, _, err := ReadFileWithTransferSyntaxRecovery(bytes.NewReader(data), ReadFileOptions{},
		TransferSyntaxRecoveryOptions{AllowUnknownTransferSyntaxUID: true}); !errors.Is(err, transfer.ErrUnknownTransferSyntax) {
		t.Fatalf("unknown-UID recovery of a legacy UID: %v", err)
	}
	f, res, err := ReadFileWithTransferSyntaxRecovery(bytes.NewReader(data), ReadFileOptions{}, legacyRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != TransferSyntaxSourceDeclared || res.DeclaredUID != transfer.PrivateGEImplicitVRBigEndianPixelData.UID ||
		res.Legacy.UID != transfer.PrivateGEImplicitVRBigEndianPixelData.UID || f.TransferSyntax.UID != transfer.ImplicitVRLittleEndian.UID {
		t.Fatalf("resolution %s", res)
	}
	if raw, _ := f.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(raw, []byte{2, 1, 4, 3}) {
		t.Fatalf("pixel data % X", raw)
	}
	if raw, _ := f.Dataset.GetRaw(core.NewTag(0x0028, 0x0011)); !bytes.Equal(raw, []byte{2, 0}) {
		t.Fatalf("columns % X", raw)
	}
	// File Meta truthfully declares the legacy syntax that was read.
	if report, err := f.ValidatePart10Identity(); err != nil || len(report.Findings) != 0 {
		t.Fatalf("identity findings %+v %v", report.Findings, err)
	}
	changed := *f
	changed.TransferSyntax = transfer.ExplicitVRLittleEndian
	if report, _ := changed.ValidatePart10Identity(); len(report.Findings) == 0 {
		t.Fatal("a changed syntax passed the identity check")
	}
	// 8-bit pixel data is not swapped.
	f, _, err = ReadFileWithTransferSyntaxRecovery(bytes.NewReader(geFile(t, 8)), ReadFileOptions{}, legacyRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := f.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(raw, []byte{1, 2, 3, 4}) {
		t.Fatalf("8-bit pixel data % X", raw)
	}
}

func TestReversingWriterAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := &reversingWriter{w: &out, width: 4}
	for _, chunk := range [][]byte{{1}, {2, 3}, {4, 5, 6, 7, 8, 9}, {10}} {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	if want := []byte{4, 3, 2, 1, 8, 7, 6, 5, 9, 10}; !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("got % X want % X", out.Bytes(), want)
	}
}
