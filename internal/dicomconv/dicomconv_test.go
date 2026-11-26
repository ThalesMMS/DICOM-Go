package dicomconv

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/encapimage"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func raw(tag core.Tag, vr core.VR, value []byte) core.Element {
	return core.NewRawElement(tag, vr, value)
}

func text(tag core.Tag, vr core.VR, value string) core.Element {
	if len(value)%2 != 0 {
		pad := " "
		if vr == core.VRUI {
			pad = "\x00"
		}
		value += pad
	}
	return raw(tag, vr, []byte(value))
}

func le16(values ...uint16) []byte {
	out := make([]byte, 2*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint16(out[2*i:], v)
	}
	return out
}

func sequence(tag core.Tag, items ...[]core.Element) core.Element {
	var sets []core.DataSet
	for _, item := range items {
		sets = append(sets, core.DataSet{Elements: item})
	}
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: sets}}
}

// richFile is a synthetic Explicit VR Little Endian object with Latin-1
// text, every numeric VR, AT, a nested and a private sequence, and native
// Pixel Data.
func richFile(t *testing.T) *object.File {
	t.Helper()
	fl := make([]byte, 8)
	binary.LittleEndian.PutUint32(fl, math.Float32bits(1.5))
	binary.LittleEndian.PutUint32(fl[4:], math.Float32bits(-2.25))
	fd := make([]byte, 8)
	binary.LittleEndian.PutUint64(fd, math.Float64bits(math.Pi))
	ss := le16(uint16(0xFFFE), 7)
	ul := make([]byte, 4)
	binary.LittleEndian.PutUint32(ul, 123456789)
	elements := []core.Element{
		text(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 100"),
		text(core.NewTag(0x0008, 0x0008), core.VRCS, `ORIGINAL\PRIMARY`),
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.543.988.1"),
		text(core.NewTag(0x0008, 0x0020), core.VRDA, "20260926"),
		text(core.NewTag(0x0008, 0x0030), core.VRTM, "101530.25"),
		text(core.NewTag(0x0008, 0x1030), core.VRLO, "Estudo cl\xednico"),
		text(core.NewTag(0x0010, 0x0010), core.VRPN, "M\xfcller^Jos\xe9"),
		text(core.NewTag(0x0010, 0x1030), core.VRDS, "72.5"),
		text(core.NewTag(0x0020, 0x0013), core.VRIS, "7"),
		text(core.NewTag(0x0020, 0x0032), core.VRDS, `-12.5\3\0.125`),
		raw(core.NewTag(0x0018, 0x1063), core.VRDS, []byte("33.3")),
		raw(core.NewTag(0x0028, 0x0034), core.VRIS, []byte(`1\1 `)),
		raw(core.NewTag(0x0018, 0x9219), core.VRSS, ss[:2]),
		raw(core.NewTag(0x0020, 0x9057), core.VRUL, ul),
		raw(core.NewTag(0x0018, 0x9087), core.VRFD, fd),
		raw(core.NewTag(0x0072, 0x0074), core.VRFL, fl),
		raw(core.NewTag(0x0028, 0x0009), core.VRAT, []byte{0x18, 0x00, 0x63, 0x10}),
		sequence(core.NewTag(0x0008, 0x1140),
			[]core.Element{
				text(core.NewTag(0x0008, 0x1150), core.VRUI, "1.2.840.10008.5.1.4.1.1.2"),
				text(core.NewTag(0x0008, 0x1155), core.VRUI, "1.2.826.0.1.3680043.10.543.988.2"),
				sequence(core.NewTag(0x0040, 0xA170), []core.Element{
					text(core.NewTag(0x0008, 0x0100), core.VRSH, "121311"),
					text(core.NewTag(0x0008, 0x0102), core.VRSH, "DCM"),
					text(core.NewTag(0x0008, 0x0104), core.VRLO, "Localizer"),
				}),
			},
			[]core.Element{text(core.NewTag(0x0008, 0x1150), core.VRUI, "1.2.840.10008.5.1.4.1.1.4")}),
		text(core.NewTag(0x0029, 0x0010), core.VRLO, "DICOMGO 988"),
		raw(core.NewTag(0x0029, 0x1001), core.VROB, []byte{1, 2, 3, 4}),
		text(core.NewTag(0x0029, 0x1002), core.VRLO, "private \xe0 texte"),
		raw(core.NewTag(0x0028, 0x0002), core.VRUS, le16(1)),
		text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		raw(core.NewTag(0x0028, 0x0010), core.VRUS, le16(4)),
		raw(core.NewTag(0x0028, 0x0011), core.VRUS, le16(4)),
		raw(core.NewTag(0x0028, 0x0100), core.VRUS, le16(16)),
		raw(core.NewTag(0x0028, 0x0101), core.VRUS, le16(12)),
		raw(core.NewTag(0x0028, 0x0102), core.VRUS, le16(11)),
		raw(core.NewTag(0x0028, 0x0103), core.VRUS, le16(0)),
		raw(core.NewTag(0x7FE0, 0x0010), core.VROW, le16(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 4095)),
	}
	return reread(t, &object.File{Dataset: object.FromElements(elements, std.Dictionary), TransferSyntax: transfer.ExplicitVRLittleEndian})
}

func reread(t *testing.T, file *object.File) *object.File {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	out, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func datasetBytes(t *testing.T, file *object.File) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteDataSet(&buf, file.Dataset, file.TransferSyntax); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegFile(t *testing.T) *object.File {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "encapimage", "testdata", "baseline_444.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	file, _, err := encapimage.FromJPEG(data, encapimage.JPEGOptions{PatientName: "João^Silva", SOPInstanceUID: "1.2.3.988"})
	if err != nil {
		t.Fatal(err)
	}
	return reread(t, file)
}

func TestRoundTripPreservesDataSetBytes(t *testing.T) {
	ctx := context.Background()
	for _, format := range []Format{JSON, XML} {
		for name, file := range map[string]*object.File{"rich": richFile(t), "jpeg": jpegFile(t)} {
			for _, bulk := range []bool{false, true} {
				dir := t.TempDir()
				opts := EncodeOptions{WithMeta: true, KeepCharacterSet: true}
				if bulk {
					opts.BulkDir, opts.BulkThreshold, opts.BaseDir = filepath.Join(dir, "bulk"), 4, dir
				}
				encoded, err := Encode(ctx, file, format, opts)
				if err != nil {
					t.Fatalf("%v %s: %v", format, name, err)
				}
				if bulk && !bytes.Contains(encoded, []byte("bulk/")) {
					t.Fatalf("%v %s: no BulkDataURI written", format, name)
				}
				back, err := Decode(ctx, encoded, format, DecodeOptions{BaseDir: dir})
				if err != nil {
					t.Fatalf("%v %s bulk=%v: decode: %v", format, name, bulk, err)
				}
				if back.TransferSyntax.UID != file.TransferSyntax.UID {
					t.Fatalf("%v %s: transfer syntax %s, want %s", format, name, back.TransferSyntax.UID, file.TransferSyntax.UID)
				}
				target := filepath.Join(dir, "out.dcm")
				if err := WritePart10(target, back); err != nil {
					t.Fatal(err)
				}
				written, err := ReadPart10(target)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(datasetBytes(t, written), datasetBytes(t, file)) {
					t.Fatalf("%v %s bulk=%v: data set bytes changed", format, name, bulk)
				}
				_ = written.Close()
			}
		}
	}
}

// TestDefaultEncodingDeclaresUTF8 checks the default conversion to UTF-8 and
// that -charset restores the original encoding byte for byte.
func TestDefaultEncodingDeclaresUTF8(t *testing.T) {
	ctx := context.Background()
	file := richFile(t)
	for _, format := range []Format{JSON, XML} {
		encoded, err := Encode(ctx, file, format, EncodeOptions{WithMeta: true})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(encoded, []byte("ISO_IR 192")) || bytes.Contains(encoded, []byte("ISO_IR 100")) {
			t.Fatalf("%v: Specific Character Set not declared as UTF-8", format)
		}
		back, err := Decode(ctx, encoded, format, DecodeOptions{CharacterSet: "ISO_IR 100"})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(datasetBytes(t, reread(t, back)), datasetBytes(t, file)) {
			t.Fatalf("%v: -charset ISO_IR 100 does not restore the source bytes", format)
		}
	}
}

func TestDecodeWithoutMetaUsesGivenTransferSyntax(t *testing.T) {
	ctx := context.Background()
	file := richFile(t)
	encoded, err := Encode(ctx, file, JSON, EncodeOptions{Compact: true})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"00020010"`)) {
		t.Fatal("meta written without -meta")
	}
	back, err := Decode(ctx, encoded, JSON, DecodeOptions{TransferSyntaxUID: transfer.ImplicitVRLittleEndian.UID})
	if err != nil || back.TransferSyntax.UID != transfer.ImplicitVRLittleEndian.UID {
		t.Fatalf("decode: %v, %s", err, back.TransferSyntax.UID)
	}
	if _, err := Decode(ctx, encoded, JSON, DecodeOptions{TransferSyntaxUID: "1.2.3"}); !errors.Is(err, ErrTransferSyntax) {
		t.Fatalf("unknown syntax: %v", err)
	}
}

func TestDecodeCharsetPolicy(t *testing.T) {
	ctx := context.Background()
	encoded, err := Encode(ctx, richFile(t), JSON, EncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(ctx, encoded, JSON, DecodeOptions{CharacterSet: "ISO_IR 192"})
	if err != nil {
		t.Fatal(err)
	}
	written := reread(t, back)
	if cs, _ := written.Dataset.GetString(core.NewTag(0x0008, 0x0005)); cs != "ISO_IR 192" {
		t.Fatalf("charset %q", cs)
	}
	if name, _ := written.Dataset.GetRaw(core.NewTag(0x0010, 0x0010)); !bytes.HasPrefix(name, []byte("Müller^José")) {
		t.Fatalf("UTF-8 name %q", name)
	}
	if _, err := Decode(ctx, encoded, JSON, DecodeOptions{CharacterSet: ""}); err != nil {
		t.Fatal(err)
	}
	// Greek cannot hold ü: fail by default, transliterate on request.
	if _, err := Decode(ctx, encoded, JSON, DecodeOptions{CharacterSet: "ISO_IR 126"}); err == nil {
		t.Fatal("unrepresentable text accepted")
	}
	back, err = Decode(ctx, encoded, JSON, DecodeOptions{CharacterSet: "ISO_IR 126", Unrepresentable: object.UnrepresentableTransliterate})
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := reread(t, back).Dataset.GetRaw(core.NewTag(0x0010, 0x0010)); !bytes.HasPrefix(name, []byte("Muller^Jose")) {
		t.Fatalf("transliterated name %q", name)
	}
}

func TestBulkDataURIMustStayInsideTheDocumentDirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, uri := range []string{"../secret.bin", "/etc/passwd", "file:///etc/passwd", `bulk\..\..\x`, "http://example.test/x"} {
		document := `{"00291001":{"vr":"OB","BulkDataURI":` + strings.ReplaceAll(`"`+uri+`"`, `\`, `\\`) + `}}`
		if _, err := Decode(ctx, []byte(document), JSON, DecodeOptions{BaseDir: dir}); !errors.Is(err, ErrBulkDataURI) {
			t.Errorf("%q: %v", uri, err)
		}
	}
	if _, err := Decode(ctx, []byte(`{"00291001":{"vr":"OB","BulkDataURI":"x.bin"}}`), JSON, DecodeOptions{}); !errors.Is(err, ErrBulkDataURI) {
		t.Fatalf("no base directory: %v", err)
	}
	if _, err := Encode(ctx, richFile(t), JSON, EncodeOptions{BulkDir: t.TempDir(), BulkThreshold: 1, BaseDir: dir}); !errors.Is(err, ErrBulkDataURI) {
		t.Fatalf("bulk directory outside the output directory: %v", err)
	}
}

// TestXMLPrivateRecoveryIsOptIn checks that an orphan private element fails
// XML output unless PrivateRecovered asks for the reported recovery.
func TestXMLPrivateRecoveryIsOptIn(t *testing.T) {
	ctx := context.Background()
	file := richFile(t)
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0009, 0x1214), VR: core.VRUN}, Value: core.RawValue("ab")})
	if _, err := Encode(ctx, file, XML, EncodeOptions{}); err == nil {
		t.Fatal("orphan private element exported without recovery")
	}
	var paths []string
	encoded, err := Encode(ctx, file, XML, EncodeOptions{PrivateRecovered: func(r dicomxml.PrivateRecovery) { paths = append(paths, r.Path) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || !strings.Contains(paths[0], "00091214") || bytes.Contains(encoded, []byte("00091214")) {
		t.Fatalf("recoveries %q", paths)
	}
}

// TestXMLTextRecoveryIsOptIn checks that trailing NUL padding of a text
// value, which reads as padding since #2032, exports to XML without a
// recovery and reads back as the unpadded value.
func TestXMLTextRecoveryIsOptIn(t *testing.T) {
	ctx := context.Background()
	file := richFile(t)
	file.Dataset.Put(raw(core.NewTag(0x0008, 0x0080), core.VRLO, []byte("Clinic\x00")))
	var paths []string
	encoded, err := Encode(ctx, file, XML, EncodeOptions{TextRecovered: func(r dicomxml.TextRecovery) { paths = append(paths, r.Path) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 || bytes.Contains(encoded, []byte("�")) || bytes.Contains(encoded, []byte("Clinic\x00")) {
		t.Fatalf("recoveries %q for trailing NUL padding", paths)
	}
	plain, err := Encode(ctx, file, XML, EncodeOptions{})
	if err != nil || !bytes.Equal(plain, encoded) {
		t.Fatalf("export without recovery: %v", err)
	}
	back, err := Decode(ctx, encoded, XML, DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePart10(filepath.Join(t.TempDir(), "out.dcm"), back); err != nil {
		t.Fatal(err)
	}
	if got, _ := back.Dataset.LookupString(core.NewTag(0x0008, 0x0080)); got != "Clinic" {
		t.Fatalf("read back %q", got)
	}
}
