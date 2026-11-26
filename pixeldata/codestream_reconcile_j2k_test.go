package pixeldata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// j2kHeaderOnly is SOC and a SIZ marker segment (ISO/IEC 15444-1 A.5.1) with
// one tile, components of the given Ssiz sampled at full resolution, then EOC.
func j2kHeaderOnly(columns, rows, components int, ssiz byte) []byte {
	b := []byte{0xff, 0x4f, 0xff, 0x51}
	b = binary.BigEndian.AppendUint16(b, uint16(38+3*components))
	b = binary.BigEndian.AppendUint16(b, 0) // Rsiz
	for _, v := range []uint32{uint32(columns + 3), uint32(rows + 5), 3, 5, uint32(columns), uint32(rows), 0, 0} {
		b = binary.BigEndian.AppendUint32(b, v) // Xsiz, Ysiz, XOsiz, YOsiz, XTsiz, YTsiz, XTOsiz, YTOsiz
	}
	b = binary.BigEndian.AppendUint16(b, uint16(components))
	for i := 0; i < components; i++ {
		b = append(b, ssiz, 1, 1)
	}
	return append(b, 0xff, 0xd9)
}

func reconciliationStrings(recs []MetadataReconciliation) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Attribute + ":" + r.Declared + ">" + r.Used
	}
	return out
}

func TestReconcileJPEG2000SIZ(t *testing.T) {
	tests := []struct {
		name     string
		stream   []byte
		elements []core.Element
		want     []string
	}{
		{
			name:   "signed codestream, unsigned data set",
			stream: j2kHeaderOnly(32, 16, 1, 0x80|11),
			elements: []core.Element{uint16RawElement(tagRows, 16), uint16RawElement(tagColumns, 32), uint16RawElement(tagSamplesPerPixel, 1),
				photometricInterpretationElement("MONOCHROME2"), uint16RawElement(tagBitsAllocated, 16), uint16RawElement(tagBitsStored, 12), uint16RawElement(tagHighBit, 11)},
			want: []string{"PixelRepresentation:0>1"},
		},
		{
			name:   "precision above Bits Stored",
			stream: j2kHeaderOnly(32, 16, 1, 9),
			elements: []core.Element{uint16RawElement(tagRows, 16), uint16RawElement(tagColumns, 32), uint16RawElement(tagSamplesPerPixel, 1),
				photometricInterpretationElement("MONOCHROME2"), uint16RawElement(tagBitsAllocated, 16), uint16RawElement(tagBitsStored, 8), uint16RawElement(tagHighBit, 7)},
			want: []string{"BitsStored:8>10", "HighBit:7>9"},
		},
		{
			name:   "color with Planar Configuration 1 and Bits Allocated 0xFFFF",
			stream: j2kHeaderOnly(32, 16, 3, 7),
			elements: []core.Element{uint16RawElement(tagRows, 16), uint16RawElement(tagColumns, 32), uint16RawElement(tagSamplesPerPixel, 3),
				photometricInterpretationElement("YBR_RCT"), uint16RawElement(tagPlanarConfiguration, 1), uint16RawElement(tagBitsAllocated, 0xffff),
				uint16RawElement(tagBitsStored, 8), uint16RawElement(tagHighBit, 7)},
			want: []string{"PlanarConfiguration:1>0", "BitsAllocated:65535>8"},
		},
		{
			name:     "missing attributes",
			stream:   j2kHeaderOnly(32, 16, 3, 7),
			elements: nil,
			want: []string{"Rows:>16", "Columns:>32", "SamplesPerPixel:>3", "PhotometricInterpretation:>RGB", "PlanarConfiguration:>0",
				"BitsAllocated:>8", "BitsStored:>8", "HighBit:>7"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := encapsulatedObject(tc.stream, tc.elements...)
			out, recs := reconcileWithCodestream(obj, transfer.JPEG2000LosslessOnly)
			if got := reconciliationStrings(recs); !slices.Equal(got, tc.want) {
				t.Fatalf("reconciliations %v, want %v", got, tc.want)
			}
			if again, recs := reconcileWithCodestream(out, transfer.JPEG2000LosslessOnly); again != out || recs != nil {
				t.Fatalf("reconciled data set changed again: %v", reconciliationStrings(recs))
			}
		})
	}
	// HTJ2K shares the SIZ marker segment.
	obj := encapsulatedObject(j2kHeaderOnly(8, 8, 1, 0x80|15))
	if _, recs := reconcileWithCodestream(obj, transfer.HTJ2KLossless); !slices.Contains(reconciliationStrings(recs), "PixelRepresentation:0>1") {
		t.Fatalf("HTJ2K reconciliations %v", reconciliationStrings(recs))
	}
}

func TestParseJPEG2000SIZRefuses(t *testing.T) {
	mixed := j2kHeaderOnly(8, 8, 3, 7)
	mixed[len(mixed)-2-3] = 11 // the third component's Ssiz
	subsampled := j2kHeaderOnly(8, 8, 3, 7)
	subsampled[len(subsampled)-2-2] = 2 // its XRsiz
	for name, data := range map[string][]byte{
		"no SOC":           j2kHeaderOnly(8, 8, 1, 7)[2:],
		"truncated":        j2kHeaderOnly(8, 8, 3, 7)[:45],
		"mixed precision":  mixed,
		"subsampled":       subsampled,
		"empty image area": j2kHeaderOnly(0, 8, 1, 7),
	} {
		if header, ok := parseJPEG2000SIZ(data); ok {
			t.Errorf("%s: parsed %+v", name, header)
		}
	}
}

// jp2Box is one JP2 box with a 32-bit length.
func jp2Box(boxType string, contents []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(contents)))
	return append(append(b, boxType...), contents...)
}

func jp2File(codestream []byte) []byte {
	b := append([]byte(nil), jp2Signature...)
	b = append(b, jp2Box("ftyp", []byte("jp2 \x00\x00\x00\x00jp2 "))...)
	b = append(b, jp2Box("jp2h", make([]byte, 22))...)
	return append(b, jp2Box("jp2c", codestream)...)
}

func TestJP2Codestream(t *testing.T) {
	codestream := j2kHeaderOnly(8, 8, 1, 7)
	toEnd := append([]byte(nil), jp2File(nil)...)
	toEnd = append(toEnd[:len(toEnd)-8], 0, 0, 0, 0, 'j', 'p', '2', 'c')
	toEnd = append(toEnd, codestream...)
	extended := append([]byte(nil), jp2Signature...)
	extended = append(extended, 0, 0, 0, 1, 'j', 'p', '2', 'c')
	extended = binary.BigEndian.AppendUint64(extended, uint64(16+len(codestream)))
	extended = append(extended, codestream...)
	for name, data := range map[string][]byte{"lengths": jp2File(codestream), "last box to the end": toEnd, "64-bit length": extended} {
		got, ok := jp2Codestream(data)
		if !ok || !bytes.Equal(got, codestream) {
			t.Errorf("%s: %v %x", name, ok, got)
		}
	}
	broken := jp2File(codestream)
	binary.BigEndian.PutUint32(broken[len(jp2Signature):], 0xffff)
	for name, data := range map[string][]byte{"codestream": codestream, "overlong box": broken, "no jp2c": jp2File(nil)[:len(jp2File(nil))-8]} {
		if got, ok := jp2Codestream(data); ok {
			t.Errorf("%s: found %x", name, got)
		}
	}
}

func encapsulatedFragments(fragments [][]byte, frames string) *object.Object {
	pixel := core.Element{
		Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength},
		Value:  core.FragmentSequence{Fragments: fragments},
	}
	elements := []core.Element{{Header: core.ElementHeader{Tag: tagNumberOfFrames, VR: core.VRIS}, Value: core.StringValue{frames}}, pixel}
	return object.FromElements(elements, nil)
}

func TestUnwrapJP2(t *testing.T) {
	// The one-component header is 47 bytes: fragments get a pad byte.
	codestream := j2kHeaderOnly(8, 8, 1, 7)
	padded := append(append([]byte(nil), codestream...), 0)
	file := jp2File(codestream)
	fragmentsOf := func(obj *object.Object) [][]byte {
		pixel, err := ExtractView(obj)
		if err != nil {
			t.Fatal(err)
		}
		return pixel.Sequence.Fragments
	}

	// One frame split over two fragments.
	out, ok := unwrapJP2(encapsulatedFragments([][]byte{file[:20], file[20:]}, "1"), transfer.JPEG2000)
	if got := fragmentsOf(out); !ok || len(got) != 1 || !bytes.Equal(got[0], padded) {
		t.Fatalf("single frame: %v %x", ok, got)
	}
	// One JP2 file per frame.
	out, ok = unwrapJP2(encapsulatedFragments([][]byte{file, file}, "2"), transfer.JPEG2000LosslessOnly)
	if got := fragmentsOf(out); !ok || len(got) != 2 || !bytes.Equal(got[1], padded) {
		t.Fatalf("two frames: %v %d", ok, len(got))
	}
	// An even codestream is kept as is.
	even := j2kHeaderOnly(8, 8, 2, 7)
	out, ok = unwrapJP2(encapsulatedFragments([][]byte{jp2File(even)}, "1"), transfer.JPEG2000)
	if got := fragmentsOf(out); !ok || !bytes.Equal(got[0], even) {
		t.Fatalf("even codestream: %v %x", ok, got[0])
	}
	// Left alone: a bare codestream, another syntax, frames that do not
	// match the fragments.
	for name, tc := range map[string]struct {
		obj    *object.Object
		syntax transfer.Syntax
	}{
		"codestream":     {encapsulatedFragments([][]byte{codestream}, "1"), transfer.JPEG2000},
		"not JPEG 2000":  {encapsulatedFragments([][]byte{file}, "1"), transfer.JPEGBaseline},
		"frame mismatch": {encapsulatedFragments([][]byte{file, file, file}, "2"), transfer.JPEG2000},
	} {
		if same, ok := unwrapJP2(tc.obj, tc.syntax); ok || same != tc.obj {
			t.Errorf("%s: unwrapped", name)
		}
	}
}

// socCheckingCodec decodes one 8x8 8-bit frame, refusing a fragment that is
// not a bare JPEG 2000 codestream.
type socCheckingCodec struct{}

func (socCheckingCodec) Decode(pixel PixelData, _ *object.Object) (Frames, error) {
	if len(pixel.Sequence.Fragments) == 0 || !bytes.HasPrefix(pixel.Sequence.Fragments[0], []byte{0xff, 0x4f}) {
		return Frames{}, errors.New("missing JPEG 2000 SOC")
	}
	return Frames{Rows: 8, Columns: 8, Data: [][]byte{make([]byte, 64)}}, nil
}

func TestDecompressUnwrapsJP2WhenAsked(t *testing.T) {
	registry := NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.JPEG2000LosslessOnly.UID, socCheckingCodec{}); err != nil {
		t.Fatal(err)
	}
	obj := encapsulatedObject(jp2File(j2kHeaderOnly(8, 8, 1, 7)),
		uint16RawElement(tagRows, 8), uint16RawElement(tagColumns, 8), uint16RawElement(tagSamplesPerPixel, 1),
		photometricInterpretationElement("MONOCHROME2"), uint16RawElement(tagBitsAllocated, 8), uint16RawElement(tagBitsStored, 8), uint16RawElement(tagHighBit, 7))
	file := &object.File{Dataset: obj, TransferSyntax: transfer.JPEG2000LosslessOnly}
	if _, err := DecompressFile(file, DecompressOptions{Registry: registry}); err == nil {
		t.Fatal("strict decompression of a JP2 file succeeded")
	}
	var report DecompressReport
	if _, err := DecompressFile(file, DecompressOptions{Registry: registry, Recovery: DecodeRecovery{UnwrapJP2Container: true}, Report: &report}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(report.Diagnostics, DiagnosticJP2ContainerUnwrapped) {
		t.Fatalf("diagnostics %v", report.Diagnostics)
	}
}

func FuzzParseJPEG2000SIZ(f *testing.F) {
	f.Add(j2kHeaderOnly(512, 512, 3, 7))
	f.Add(jp2File(j2kHeaderOnly(8, 8, 1, 0x8f)))
	f.Fuzz(func(t *testing.T, data []byte) {
		if header, ok := parseJPEG2000SIZ(data); ok && (header.rows <= 0 || header.columns <= 0 || header.precision < 1 || header.precision > 128) {
			t.Fatalf("header %+v", header)
		}
		if codestream, ok := jp2Codestream(data); ok && len(codestream) > len(data) {
			t.Fatalf("codestream of %d bytes from %d", len(codestream), len(data))
		}
	})
}
