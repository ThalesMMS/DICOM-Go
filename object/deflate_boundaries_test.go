package object_test

import (
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func deflateFixture(pixels []byte) *object.Object {
	u16 := func(element, v uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, element), VR: core.VRUS}, Value: core.Uint16Value{v}}
	}
	return object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.940.1"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"SYNTHETIC^DEFLATE"}},
		u16(0x0002, 1), {Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}},
		u16(0x0010, 1), u16(0x0011, uint16(len(pixels))), u16(0x0100, 8), u16(0x0101, 8), u16(0x0102, 7), u16(0x0103, 0),
		core.NewRawElement(core.TagPixelData, core.VROB, pixels),
		{Header: core.ElementHeader{Tag: core.NewTag(0x7FE1, 0x0010), VR: core.VRLO}, Value: core.StringValue{"DICOMGO SENTINEL"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x7FE1, 0x1001), VR: core.VRLO}, Value: core.StringValue{"AFTER PIXEL"}},
	}, std.Dictionary)
}

// Part 10 keeps File Meta Information uncompressed; raw Deflate applies to the
// Explicit VR Little Endian data set that follows it (PS3.5 A.5).
func TestDeflatedPart10CompressesOnlyTheDataSetRegion(t *testing.T) {
	pixels := bytes.Repeat([]byte{0, 1, 2, 3, 250, 251}, 50)
	dataset := deflateFixture(pixels)
	var deflated, explicit, raw bytes.Buffer
	if err := object.WriteFile(&deflated, &object.File{Dataset: dataset, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := object.WriteFile(&explicit, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := object.WriteDataSet(&raw, dataset, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(deflated.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var meta bytes.Buffer
	if err := object.WriteDataSet(&meta, file.Meta, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	prefix := 128 + 4
	if !bytes.Contains(deflated.Bytes()[:prefix+meta.Len()], []byte(transfer.DeflatedExplicitVRLittleEndian.UID)) {
		t.Fatal("File Meta is not stored uncompressed")
	}
	inflated, err := io.ReadAll(flate.NewReader(bytes.NewReader(deflated.Bytes()[prefix+meta.Len():])))
	if err != nil {
		t.Fatalf("data set region is not a raw Deflate stream: %v", err)
	}
	if !bytes.Equal(inflated, raw.Bytes()) {
		t.Fatal("inflated data set differs from the Explicit VR Little Endian encoding")
	}
	element, _ := file.Dataset.Get(core.TagPixelData)
	if got, ok := element.Value.(core.RawValue); !ok || !bytes.Equal(got, pixels) {
		t.Fatalf("Deflated Pixel Data read as %T", element.Value)
	}
	if value, _ := file.Dataset.GetString(core.NewTag(0x7FE1, 0x1001)); value != "AFTER PIXEL" {
		t.Fatal("attribute after Pixel Data lost")
	}

	// Fragments are never written under Deflated: it is a native syntax.
	fragmented := deflateFixture(pixels)
	fragmented.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: [][]byte{pixels}}})
	if err := object.WriteFile(io.Discard, &object.File{Dataset: fragmented, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}); !errors.Is(err, object.ErrPixelDataEncodingMismatch) {
		t.Fatalf("fragments under Deflated error = %v", err)
	}
}

func TestDeflatedReadRefusesBombsTruncationAndCorruptStreams(t *testing.T) {
	// 64 MiB of zeros deflates to about 64 KiB.
	bomb := deflateFixture(make([]byte, 64<<20))
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: bomb, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() > 1<<20 {
		t.Fatalf("fixture is not a compression bomb: %d bytes", encoded.Len())
	}
	budget := object.ReadFileOptions{MaxTotalBytes: 1 << 20, MaxElementBytes: 1 << 20}
	if _, err := object.ReadFileWithOptions(bytes.NewReader(encoded.Bytes()), budget); err == nil {
		t.Fatal("decompression bomb accepted under a 1 MiB inflated budget")
	}

	good := deflateFixture(bytes.Repeat([]byte{1, 2, 3, 4}, 4096))
	encoded.Reset()
	if err := object.WriteFile(&encoded, &object.File{Dataset: good, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if _, err := object.ReadFile(bytes.NewReader(encoded.Bytes())); err != nil {
		t.Fatal(err)
	}
	// Cut the deflate bitstream itself, not only its PS3.5 A.5 pad byte.
	streamEnd := encoded.Len() - deflatePadLength(t, good)
	for cut := streamEnd - 1; cut > streamEnd-64; cut -= 7 {
		if _, err := object.ReadFile(bytes.NewReader(encoded.Bytes()[:cut])); err == nil {
			t.Fatalf("Deflate stream truncated at %d/%d accepted", cut, encoded.Len())
		}
	}
	corrupt := append([]byte(nil), encoded.Bytes()...)
	for i := len(corrupt) - 200; i < len(corrupt)-100; i++ {
		corrupt[i] = 0xFF
	}
	if _, err := object.ReadFile(bytes.NewReader(corrupt)); err == nil {
		t.Fatal("corrupt Deflate stream accepted")
	}
}

// deflatePadLength returns the number of bytes that follow the end of the
// deflate bitstream of obj written as a Deflated data set.
func deflatePadLength(t *testing.T, obj *object.Object) int {
	t.Helper()
	var stream bytes.Buffer
	if err := object.WriteDataSet(&stream, obj, transfer.DeflatedExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	// A bytes.Reader is an io.ByteReader, so the inflater consumes exactly the
	// bitstream and leaves the padding unread.
	source := bytes.NewReader(stream.Bytes())
	if _, err := io.Copy(io.Discard, flate.NewReader(source)); err != nil {
		t.Fatal(err)
	}
	return source.Len()
}

// PS3.5 A.5: an odd-length deflate bitstream gets one trailing NULL byte, so
// the data set is always even. DIMSE relies on it: every P-DATA fragment must
// be even (PS3.8 E.1) and DCMTK aborts an odd one (#1271).
func TestDeflatedDataSetIsPaddedToEvenLength(t *testing.T) {
	sawPad, sawUnpadded := false, false
	for size := 1; size <= 64; size++ {
		pixels := make([]byte, size*2)
		for i := range pixels {
			pixels[i] = byte(i*37 + size)
		}
		dataset := deflateFixture(pixels)
		var direct, validated bytes.Buffer
		if err := object.WriteDataSet(&direct, dataset, transfer.DeflatedExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		if _, err := object.WriteDataSetWithValidation(context.Background(), &validated, dataset, transfer.DeflatedExplicitVRLittleEndian, validation.Options{}); err != nil {
			t.Fatal(err)
		}
		for name, encoded := range map[string][]byte{"WriteDataSet": direct.Bytes(), "WriteDataSetWithValidation": validated.Bytes()} {
			if len(encoded)%2 != 0 {
				t.Fatalf("%s: %d-byte pixel fixture deflated to odd length %d", name, len(pixels), len(encoded))
			}
			source := bytes.NewReader(encoded)
			if _, err := io.Copy(io.Discard, flate.NewReader(source)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			switch pad := encoded[len(encoded)-source.Len():]; {
			case len(pad) == 0:
				sawUnpadded = true
			case len(pad) == 1 && pad[0] == 0:
				sawPad = true
			default:
				t.Fatalf("%s: %d bytes after the bitstream: % x", name, len(pad), pad)
			}
			got, err := object.ReadDataSet(bytes.NewReader(encoded), transfer.DeflatedExplicitVRLittleEndian)
			if err != nil {
				t.Fatalf("%s: read padded data set: %v", name, err)
			}
			if raw, _ := got.GetRaw(core.TagPixelData); !bytes.Equal(raw, pixels) {
				t.Fatalf("%s: samples differ after the pad", name)
			}
		}
	}
	if !sawPad || !sawUnpadded {
		t.Fatalf("fixtures did not cover both parities: padded=%t unpadded=%t", sawPad, sawUnpadded)
	}
}
