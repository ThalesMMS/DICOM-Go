package dicomweb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// stowEncodingFixture encodes the same data set, with a sequence and 16-bit
// native Pixel Data, in syntax.
func stowEncodingFixture(t *testing.T, syntax transfer.Syntax) []byte {
	t.Helper()
	order := binary.ByteOrder(binary.LittleEndian)
	if syntax.UID == transfer.ExplicitVRBigEndian.UID {
		order = binary.BigEndian
	}
	us := func(group, element uint16, value uint16) core.Element {
		return dicomtest.Uint16Element(core.NewTag(group, element), core.VRUS, order, value)
	}
	pixels := make([]uint16, 16)
	for i := range pixels {
		pixels[i] = uint16(i*4099 + 7)
	}
	elements := []core.Element{
		dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.543.77.1"),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1115), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			dicomtest.StringElement(core.NewTag(0x0020, 0x000E), core.VRUI, "1.2.826.0.1.3680043.10.543.77.2"),
		}}}}},
		dicomtest.StringElement(core.NewTag(0x0010, 0x0010), core.VRPN, "STOW^ENCODING"),
		dicomtest.StringElement(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2.826.0.1.3680043.10.543.77"),
		us(0x0028, 0x0002, 1),
		dicomtest.StringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		us(0x0028, 0x0010, 4),
		us(0x0028, 0x0011, 4),
		us(0x0028, 0x0100, 16),
		us(0x0028, 0x0101, 16),
		us(0x0028, 0x0102, 15),
		us(0x0028, 0x0103, 0),
		dicomtest.Uint16Element(core.TagPixelData, core.VROW, order, pixels...),
	}
	data, err := dicomtest.Part10File(syntax, elements...)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// stowEncodingDataSet returns the transfer syntax of a Part 10 object and its
// data set in a canonical encoding: parsed and written again in Explicit VR
// Little Endian, so equal values give equal bytes whatever the item lengths.
func stowEncodingDataSet(t *testing.T, data []byte) (string, []byte) {
	t.Helper()
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var canonical bytes.Buffer
	if err := object.WriteDataSet(&canonical, file.Dataset, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	return file.TransferSyntax.UID, canonical.Bytes()
}

func TestExplicitLittleEndianStorePayloadKeepsEveryValue(t *testing.T) {
	_, want := stowEncodingDataSet(t, stowEncodingFixture(t, transfer.ExplicitVRLittleEndian))
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.dcm")
			source := stowEncodingFixture(t, syntax)
			if err := os.WriteFile(path, source, 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := ExplicitLittleEndianStorePayload(file, int64(len(source)))
			if err != nil {
				t.Fatal(err)
			}
			encoded, readErr := io.ReadAll(payload)
			if err := payload.Close(); err != nil || readErr != nil {
				t.Fatalf("read %v close %v", readErr, err)
			}
			// The source is closed with the payload.
			if _, err := file.Read(make([]byte, 1)); err == nil {
				t.Fatal("the payload left its source open")
			}
			uid, got := stowEncodingDataSet(t, encoded)
			if uid != transfer.ExplicitVRLittleEndian.UID {
				t.Fatalf("re-encoded transfer syntax = %s", uid)
			}
			if !bytes.Equal(got, want) {
				first := 0
				for first < min(len(got), len(want)) && got[first] == want[first] {
					first++
				}
				t.Fatalf("the re-encoded data set differs from the Explicit VR Little Endian encoding of the same values (%d and %d bytes, first difference at %d)", len(got), len(want), first)
			}
		})
	}
}

// stowNumericFixture encodes, in syntax, a data set with a value of every
// fixed-width VR, a sequence item holding an OW value over the inline
// threshold (read with its item, as nested values are not deferred) and
// 16-bit Pixel Data over the threshold (deferred, streamed from the source).
// The Little Endian values are reversed by VR width for Explicit VR Big
// Endian.
func stowNumericFixture(t *testing.T, syntax transfer.Syntax) []byte {
	t.Helper()
	bigEndian := syntax.UID == transfer.ExplicitVRBigEndian.UID
	raw := func(tag core.Tag, vr core.VR, littleEndian []byte) core.Element {
		if bigEndian {
			littleEndian = valueencode.ReverseFixedWidth(littleEndian, valueencode.ByteOrderWidth(vr))
		}
		return core.NewRawElement(tag, vr, littleEndian)
	}
	pattern := func(length int, seed byte) []byte {
		data := make([]byte, length)
		for i := range data {
			data[i] = byte(i*31) + seed
		}
		return data
	}
	private := func(element uint16) core.Tag { return core.NewTag(0x0009, 0x1000|element) }
	us := func(tag core.Tag, value uint16) core.Element {
		return raw(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, value))
	}
	const rows, columns = 512, 1025 // 1,049,600 bytes of Pixel Data, over 1 MiB
	elements := []core.Element{
		dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.543.78.1"),
		dicomtest.StringElement(core.NewTag(0x0009, 0x0010), core.VRLO, "TWIN STOW BYTE ORDER"),
		raw(private(0x01), core.VRSS, pattern(4, 1)),
		raw(private(0x02), core.VRSL, pattern(8, 2)),
		raw(private(0x03), core.VRUL, pattern(8, 3)),
		raw(private(0x04), core.VRFL, pattern(8, 4)),
		raw(private(0x05), core.VRFD, pattern(16, 5)),
		raw(private(0x06), core.VRAT, []byte{0x28, 0x00, 0x10, 0x00, 0xE0, 0x7F, 0x10, 0x00}),
		raw(private(0x07), core.VROF, pattern(12, 7)),
		raw(private(0x08), core.VROD, pattern(16, 8)),
		raw(private(0x09), core.VROL, pattern(12, 9)),
		raw(private(0x0A), core.VRSV, pattern(16, 10)),
		raw(private(0x0B), core.VRUV, pattern(16, 11)),
		{Header: core.ElementHeader{Tag: private(0x10), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			us(core.NewTag(0x0028, 0x0010), 7),
			raw(core.NewTag(0x5400, 0x1010), core.VROW, pattern(1<<20+64, 12)),
		}}}}},
		dicomtest.StringElement(core.NewTag(0x0010, 0x0010), core.VRPN, "STOW^BYTE^ORDER"),
		dicomtest.StringElement(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2.826.0.1.3680043.10.543.78"),
		us(core.NewTag(0x0028, 0x0002), 1),
		dicomtest.StringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		us(core.NewTag(0x0028, 0x0010), rows),
		us(core.NewTag(0x0028, 0x0011), columns),
		us(core.NewTag(0x0028, 0x0100), 16),
		us(core.NewTag(0x0028, 0x0101), 16),
		us(core.NewTag(0x0028, 0x0102), 15),
		us(core.NewTag(0x0028, 0x0103), 0),
		raw(core.TagPixelData, core.VROW, pattern(rows*columns*2, 13)),
	}
	data, err := dicomtest.Part10File(syntax, elements...)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A Big Endian source keeps every numeric value through the re-encoding,
// inside sequence items too, and its deferred Pixel Data, which the writer
// reverses while it streams the value from the source.
func TestExplicitLittleEndianStorePayloadReversesBigEndianValues(t *testing.T) {
	_, want := stowEncodingDataSet(t, stowNumericFixture(t, transfer.ExplicitVRLittleEndian))
	source := stowNumericFixture(t, transfer.ExplicitVRBigEndian)
	path := filepath.Join(t.TempDir(), "big-endian.dcm")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := ExplicitLittleEndianStorePayload(file, int64(len(source)))
	if err != nil {
		t.Fatal(err)
	}
	encoded, readErr := io.ReadAll(payload)
	if err := errors.Join(readErr, payload.Close()); err != nil {
		t.Fatal(err)
	}
	uid, got := stowEncodingDataSet(t, encoded)
	if uid != transfer.ExplicitVRLittleEndian.UID {
		t.Fatalf("re-encoded transfer syntax = %s", uid)
	}
	if !bytes.Equal(got, want) {
		first := 0
		for first < min(len(got), len(want)) && got[first] == want[first] {
			first++
		}
		t.Fatalf("the re-encoded Big Endian data set differs from the Little Endian encoding of the same values (%d and %d bytes, first difference at %d)", len(got), len(want), first)
	}
}

// A source in Implicit or Explicit VR Little Endian or in Explicit VR Big
// Endian keeps a top-level value over the 1 MiB inline threshold of a VR the
// parser does not defer (OL, OV, SV, UV, UC): the value is read into memory
// (and reversed by the writer for Big Endian) instead of failing the parse
// (#1980). Standard tags let an Implicit VR source resolve each VR.
func TestExplicitLittleEndianStorePayloadKeepsLargeNonDeferrableValues(t *testing.T) {
	const length = 1<<20 + 64
	for _, tc := range []struct {
		vr  core.VR
		tag core.Tag
	}{
		{core.VROL, core.NewTag(0x0066, 0x0040)}, // Long Primitive Point Index List
		{core.VROV, core.NewTag(0x0072, 0x0081)}, // Selector OV Value
		{core.VRSV, core.NewTag(0x0072, 0x0082)}, // Selector SV Value
		{core.VRUV, core.NewTag(0x0072, 0x0083)}, // Selector UV Value
		{core.VRUC, core.NewTag(0x0072, 0x006F)}, // Selector UC Value
	} {
		vr := tc.vr
		littleEndian := make([]byte, length)
		for i := range littleEndian {
			littleEndian[i] = byte(i*37) + 5
		}
		if vr == core.VRUC {
			littleEndian = bytes.Repeat([]byte("UC"), length/2)
		}
		fixture := func(t *testing.T, syntax transfer.Syntax) []byte {
			value := littleEndian
			if width := valueencode.ByteOrderWidth(vr); width > 1 && syntax.UID == transfer.ExplicitVRBigEndian.UID {
				value = valueencode.ReverseFixedWidth(value, width)
			}
			data, err := dicomtest.Part10File(syntax, append(dicomtest.MinimalDataset(), core.NewRawElement(tc.tag, vr, value))...)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
			t.Run(string(vr)+"/"+syntax.UID, func(t *testing.T) {
				_, want := stowEncodingDataSet(t, fixture(t, transfer.ExplicitVRLittleEndian))
				source := fixture(t, syntax)
				path := filepath.Join(t.TempDir(), "source.dcm")
				if err := os.WriteFile(path, source, 0o600); err != nil {
					t.Fatal(err)
				}
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				payload, err := ExplicitLittleEndianStorePayload(file, int64(len(source)))
				if err != nil {
					t.Fatal(err)
				}
				encoded, readErr := io.ReadAll(payload)
				if err := errors.Join(readErr, payload.Close()); err != nil {
					t.Fatal(err)
				}
				uid, got := stowEncodingDataSet(t, encoded)
				if uid != transfer.ExplicitVRLittleEndian.UID {
					t.Fatalf("re-encoded transfer syntax = %s", uid)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("the re-encoded %s value differs from its Little Endian encoding (%d and %d bytes)", vr, len(got), len(want))
				}
			})
		}
	}
}

// A Big Endian source whose fixed-width value is not a whole number of units
// (a malformed private value that object.ReadFile accepts) is still
// re-encoded: the whole units are reversed and the trailing partial unit is
// kept as it is, as in a Little Endian source, instead of refusing the upload.
func TestExplicitLittleEndianStorePayloadKeepsPartialBigEndianUnits(t *testing.T) {
	tag := core.NewTag(0x0009, 0x1001)
	for _, c := range []struct {
		name      string
		vr        core.VR
		bigEndian []byte
		reencoded []byte
	}{
		{"FD4", core.VRFD, []byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}},
		{"UL2", core.VRUL, []byte{1, 2}, []byte{1, 2}},
		{"FL6", core.VRFL, []byte{1, 2, 3, 4, 5, 6}, []byte{4, 3, 2, 1, 5, 6}},
		{"OL6", core.VROL, []byte{1, 2, 3, 4, 5, 6}, []byte{4, 3, 2, 1, 5, 6}},
		{"FD12InItem", core.VRFD, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, []byte{8, 7, 6, 5, 4, 3, 2, 1, 9, 10, 11, 12}},
	} {
		t.Run(c.name, func(t *testing.T) {
			value := core.NewRawElement(tag, c.vr, c.bigEndian)
			elements := append(dicomtest.MinimalDataset(), dicomtest.StringElement(core.NewTag(0x0009, 0x0010), core.VRLO, "TWIN STOW PARTIAL UNIT"))
			if c.name == "FD12InItem" {
				elements = append(elements, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0009, 0x1002), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
					dicomtest.StringElement(core.NewTag(0x0009, 0x0010), core.VRLO, "TWIN STOW PARTIAL UNIT"),
					value,
				}}}}})
			} else {
				elements = append(elements, value)
			}
			source, err := dicomtest.Part10File(transfer.ExplicitVRBigEndian, elements...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := object.ReadFile(bytes.NewReader(source)); err != nil {
				t.Fatalf("the source is not readable: %v", err)
			}
			path := filepath.Join(t.TempDir(), "big-endian.dcm")
			if err := os.WriteFile(path, source, 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := ExplicitLittleEndianStorePayload(file, int64(len(source)))
			if err != nil {
				t.Fatal(err)
			}
			encoded, readErr := io.ReadAll(payload)
			if err := errors.Join(readErr, payload.Close()); err != nil {
				t.Fatal(err)
			}
			reencoded, err := object.ReadFile(bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if reencoded.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID {
				t.Fatalf("re-encoded transfer syntax = %s", reencoded.TransferSyntax.UID)
			}
			holder := reencoded.Dataset.Elements()
			if c.name == "FD12InItem" {
				sequence, ok := reencoded.Dataset.Get(core.NewTag(0x0009, 0x1002))
				items, isSequence := sequence.Value.(core.SequenceValue)
				if !ok || !isSequence || len(items.Items) != 1 {
					t.Fatalf("re-encoded sequence = %#v", sequence.Value)
				}
				holder = items.Items[0].Elements
			}
			for _, element := range holder {
				if element.Tag() != tag {
					continue
				}
				got, ok := element.RawBytes()
				if !ok || !bytes.Equal(got, c.reencoded) {
					t.Fatalf("re-encoded %s = %v, want %v", c.vr, got, c.reencoded)
				}
				return
			}
			t.Fatalf("the re-encoded object has no %s", tag)
		})
	}
}

// writeStowEncodingFile writes elements as a Part 10 object in syntax and
// returns its path and length.
func writeStowEncodingFile(t *testing.T, syntax transfer.Syntax, elements []core.Element) (string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.dcm")
	output, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	file := &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: syntax}
	if err := object.WriteFile(output, file); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	info, err := output.Stat()
	if closeErr := output.Close(); err != nil || closeErr != nil {
		t.Fatalf("stat %v close %v", err, closeErr)
	}
	return path, info.Size()
}

// The re-encoding is bounded by the source, not by a fixed element ceiling:
// an object whose Pixel Data or Float Pixel Data is longer than 64 MiB is
// sent in every syntax it can be re-encoded from.
func TestExplicitLittleEndianStorePayloadReencodesElementsOver64MiB(t *testing.T) {
	if testing.Short() {
		t.Skip("writes objects of 64 MiB")
	}
	const length = 64<<20 + 16
	pixels := make([]byte, length)
	for _, tc := range []struct {
		name   string
		syntax transfer.Syntax
		tag    core.Tag
		vr     core.VR
	}{
		{"implicit-pixel-data", transfer.ImplicitVRLittleEndian, core.TagPixelData, core.VROW},
		{"implicit-float-pixel-data", transfer.ImplicitVRLittleEndian, core.NewTag(0x7FE0, 0x0008), core.VROF},
		{"big-endian-pixel-data", transfer.ExplicitVRBigEndian, core.TagPixelData, core.VROW},
		{"deflated-pixel-data", transfer.DeflatedExplicitVRLittleEndian, core.TagPixelData, core.VROW},
	} {
		t.Run(tc.name, func(t *testing.T) {
			elements := append(dicomtest.MinimalDataset(), core.NewRawElement(tc.tag, tc.vr, pixels))
			path, size := writeStowEncodingFile(t, tc.syntax, elements)
			source, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			measured, err := ExplicitLittleEndianStorePayloadSize(source, size)
			if err != nil {
				t.Fatalf("measure: %v", err)
			}
			if source, err = os.Open(path); err != nil {
				t.Fatal(err)
			}
			payload, err := ExplicitLittleEndianStorePayload(source, size)
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(t.TempDir(), "payload.dcm")
			target, err := os.Create(output)
			if err != nil {
				t.Fatal(err)
			}
			written, copyErr := io.Copy(target, payload)
			if err := errors.Join(copyErr, payload.Close(), target.Close()); err != nil {
				t.Fatal(err)
			}
			if written != measured {
				t.Fatalf("payload of %d bytes, measured %d", written, measured)
			}
			reencoded, err := os.Open(output)
			if err != nil {
				t.Fatal(err)
			}
			defer reencoded.Close()
			file, err := object.ReadFileWithOptions(reencoded, object.ReadFileOptions{SkipPixelData: true})
			if err != nil {
				t.Fatal(err)
			}
			element, ok := file.Dataset.Get(tc.tag)
			if file.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID || !ok || int64(element.Header.Length) != length {
				t.Fatalf("re-encoded %s with %s present=%t length %d", file.TransferSyntax.UID, tc.tag, ok, element.Header.Length)
			}
		})
	}
}

// The measured size is the length of the payload sent, which differs from
// the source length: a Deflated object grows when it is inflated.
func TestExplicitLittleEndianStorePayloadSizeIsThePayloadLength(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			// Repeated values deflate well, as real objects do.
			elements := append(dicomtest.MinimalDataset(), core.NewRawElement(core.TagPixelData, core.VROW, bytes.Repeat([]byte{1, 0}, 64<<10)))
			path, size := writeStowEncodingFile(t, syntax, elements)
			source, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			measured, err := ExplicitLittleEndianStorePayloadSize(source, size)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := source.Read(make([]byte, 1)); err == nil {
				t.Fatal("measuring left the source open")
			}
			if source, err = os.Open(path); err != nil {
				t.Fatal(err)
			}
			payload, err := ExplicitLittleEndianStorePayload(source, size)
			if err != nil {
				t.Fatal(err)
			}
			encoded, readErr := io.ReadAll(payload)
			if err := errors.Join(readErr, payload.Close()); err != nil {
				t.Fatal(err)
			}
			if int64(len(encoded)) != measured {
				t.Fatalf("payload of %d bytes, measured %d", len(encoded), measured)
			}
			if syntax.UID == transfer.DeflatedExplicitVRLittleEndian.UID && measured < 4*size {
				t.Fatalf("an inflated payload of %d bytes from %d deflated bytes", measured, size)
			}
		})
	}
}

func TestExplicitLittleEndianStorePayloadRefusesEncapsulatedSources(t *testing.T) {
	elements := append(dicomtest.MinimalDataset(), core.Element{
		Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB},
		Value:  core.FragmentSequence{Fragments: [][]byte{{0xFF, 0xD8, 0xFF, 0xD9}}},
	})
	data, err := dicomtest.Part10File(transfer.JPEGBaseline, elements...)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "jpeg.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExplicitLittleEndianStorePayload(file, int64(len(data))); !errors.Is(err, ErrStoreReencodeUnsupported) {
		t.Fatalf("encapsulated source = %v, want ErrStoreReencodeUnsupported", err)
	}
	if _, err := file.Read(make([]byte, 1)); err == nil {
		t.Fatal("a refused source was left open")
	}
}

func TestStoreRequiresExplicitLittleEndian(t *testing.T) {
	for uid, want := range map[string]bool{
		transfer.ImplicitVRLittleEndian.UID:         true,
		transfer.ExplicitVRBigEndian.UID:            true,
		transfer.ExplicitVRLittleEndian.UID:         false,
		transfer.DeflatedExplicitVRLittleEndian.UID: false,
		transfer.JPEGBaseline.UID:                   false,
	} {
		if got := StoreRequiresExplicitLittleEndian(uid); got != want {
			t.Errorf("StoreRequiresExplicitLittleEndian(%s) = %t", uid, got)
		}
	}
}
