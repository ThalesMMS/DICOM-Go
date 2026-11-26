package parser

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

// Vectors below are synthetic data sets derived from the layouts of the
// DCMTK dcmdata tparser.cc tests (wrongDelimitationItemFor*,
// undefinedLengthUNSequence); no DCMTK bytes are copied.

func obValue(g, e uint16, length uint32, value ...byte) []byte {
	return append(explicitLongHeader(g, e, "OB", length), value...)
}

func explicitLongHeader(g, e uint16, vr string, length uint32) []byte {
	b := explicitLong(g, e, vr, nil)
	copy(b[8:], le32(length))
	return b
}

func iconItemWithPixel() []byte {
	var b []byte
	b = append(b, sqHeader(0x0088, 0x0200, 0xffffffff)...)   // Icon Image Sequence
	b = append(b, control(0xfffe, 0xe000, 0xffffffff)...)    // item, undefined
	b = append(b, obValue(0x7fe0, 0x0010, 4, 1, 2, 3, 4)...) // Pixel Data
	return b
}

func TestReplaceWrongDelimiterForSequence(t *testing.T) {
	data := iconItemWithPixel()
	data = append(data, control(0xfffe, 0xe00d, 0)...)
	data = append(data, control(0xfffe, 0xe00d, 0)...) // should be a Sequence Delimitation Item
	data = append(data, obValue(0xfffc, 0xfffc, 4, 5, 6, 7, 8)...)
	if _, _, err := readMixed(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted the wrong delimiter")
	}
	elements, recs, err := readMixed(t, data, ReaderOptions{SequenceRecovery: SequenceRecoveryOptions{ReplaceWrongDelimiter: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 2 || elements[1].Header.Tag != core.NewTag(0xfffc, 0xfffc) || len(recs) != 1 || recs[0].Reason != BoundaryWrongDelimiterReplaced {
		t.Fatalf("elements %d recoveries %+v", len(elements), recs)
	}
}

func TestReplaceWrongDelimiterForItem(t *testing.T) {
	data := iconItemWithPixel()
	data = append(data, control(0xfffe, 0xe0dd, 0)...) // should be an Item Delimitation Item
	data = append(data, control(0xfffe, 0xe0dd, 0)...)
	data = append(data, obValue(0xfffc, 0xfffc, 4, 5, 6, 7, 8)...)
	if _, _, err := readMixed(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted the wrong delimiter")
	}
	elements, recs, err := readMixed(t, data, ReaderOptions{SequenceRecovery: SequenceRecoveryOptions{ReplaceWrongDelimiter: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 2 || len(recs) != 1 {
		t.Fatalf("elements %d recoveries %+v", len(elements), recs)
	}
}

func TestUndefinedLengthOBAsSequence(t *testing.T) {
	var data []byte
	data = append(data, explicitLongHeader(0x0009, 0x1001, "OB", 0xffffffff)...)
	data = append(data, control(0xfffe, 0xe000, 0xffffffff)...)
	data = append(data, explicitShort(0x0010, 0x0010, "PN", []byte("ABCD"))...)
	data = append(data, control(0xfffe, 0xe00d, 0)...)
	data = append(data, control(0xfffe, 0xe0dd, 0)...)
	if _, _, err := readMixed(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted undefined-length OB")
	}
	elements, recs, err := readMixed(t, data, ReaderOptions{SequenceRecovery: SequenceRecoveryOptions{UndefinedLengthOBOWAsSQ: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 1 || elements[0].Header.Tag != core.NewTag(0x0010, 0x0010) || len(recs) != 1 || recs[0].UsedVR != core.VRSQ {
		t.Fatalf("elements %d recoveries %+v", len(elements), recs)
	}
	// Content that does not start with an Item is left to the strict error.
	bad := append(explicitLongHeader(0x0009, 0x1001, "OB", 0xffffffff), 1, 2, 3, 4, 5, 6, 7, 8)
	if _, recs, err := readMixed(t, bad, ReaderOptions{SequenceRecovery: SequenceRecoveryOptions{UndefinedLengthOBOWAsSQ: true}}); err == nil || len(recs) != 0 {
		t.Fatalf("non-item content accepted: %v %+v", err, recs)
	}
}

func TestVOILUTSequenceOWAsSequence(t *testing.T) {
	item := append(control(0xfffe, 0xe000, 12), explicitShort(0x0028, 0x3003, "LO", []byte("WIN1"))...)
	data := append(explicitLongHeader(0x0028, 0x3010, "OW", uint32(len(item))), item...)
	elements, recs, err := readMixed(t, data, ReaderOptions{SequenceRecovery: SequenceRecoveryOptions{VOILUTSequenceOWAsSQ: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 1 || elements[0].Header.Tag != core.NewTag(0x0028, 0x3003) || len(recs) != 1 || recs[0].Reason != BoundaryVOILUTAsSequence {
		t.Fatalf("elements %d recoveries %+v", len(elements), recs)
	}
	// Without the flag the value stays opaque OW bytes.
	elements, _, err = readMixed(t, data, ReaderOptions{})
	if err != nil || len(elements) != 1 || elements[0].Header.VR != core.VROW {
		t.Fatalf("strict: %v %+v", err, elements)
	}
}

func TestPrivateSequenceKeptAsBytes(t *testing.T) {
	garbage := []byte{0x20, 0x01, 0x10, 0x00, 'L', 'O', 0x00, 0x16, 'x', 'x'} // length 0x1600 overruns
	item := append(control(0xfffe, 0xe000, uint32(len(garbage))), garbage...)
	data := append(sqHeader(0x2001, 0x105f, uint32(len(item))), item...)
	data = append(data, explicitShort(0x0010, 0x0020, "LO", []byte("ID12"))...)
	if _, _, err := readMixed(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted the malformed private sequence")
	}
	elements, recs, err := readMixed(t, data, ReaderOptions{SequenceRecovery: SequenceRecoveryOptions{PrivateSequenceAsBytes: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 2 || elements[0].Header.VR != core.VRUN || len(recs) != 1 || recs[0].Reason != BoundaryPrivateSequenceBytes {
		t.Fatalf("elements %+v recoveries %+v", elements, recs)
	}
	if raw, _ := elements[0].RawBytes(); len(raw) != len(item) {
		t.Fatalf("kept %d bytes, want %d", len(raw), len(item))
	}
	// A well-formed private sequence is read normally, with no report.
	good := append(control(0xfffe, 0xe000, 12), explicitShort(0x2001, 0x1001, "LO", []byte("ABCD"))...)
	ok := append(sqHeader(0x2001, 0x105f, uint32(len(good))), good...)
	elements, recs, err = readMixed(t, ok, ReaderOptions{SequenceRecovery: SequenceRecoveryOptions{PrivateSequenceAsBytes: true}})
	if err != nil || len(elements) != 1 || elements[0].Header.Tag != core.NewTag(0x2001, 0x1001) || len(recs) != 0 {
		t.Fatalf("well-formed: %v %+v %+v", err, elements, recs)
	}
}

// Undefined-length UN sequences are always read as Implicit VR Little
// Endian (DCMTK dcmReadImplPrivAttribMaxLengthAsSQ is the default here).
func TestUndefinedLengthUNSequenceDefault(t *testing.T) {
	var data []byte
	data = append(data, explicitLongHeader(0x0088, 0x0200, "UN", 0xffffffff)...)
	data = append(data, control(0xfffe, 0xe000, 0xffffffff)...)
	data = append(data, implicitEl(0x0010, 0x0010, []byte("ABCD"))...)
	data = append(data, control(0xfffe, 0xe00d, 0)...)
	data = append(data, control(0xfffe, 0xe0dd, 0)...)
	elements, _, err := readMixed(t, data, ReaderOptions{})
	if err != nil || len(elements) != 1 || elements[0].Header.VR != core.VRPN {
		t.Fatalf("%v %+v", err, elements)
	}
}

func FuzzSequenceRecovery(f *testing.F) {
	f.Add(iconItemWithPixel())
	f.Add(append(explicitLongHeader(0x0009, 0x1001, "OB", 0xffffffff), control(0xfffe, 0xe000, 0xffffffff)...))
	f.Add(append(sqHeader(0x2001, 0x105f, 12), control(0xfffe, 0xe000, 4)...))
	f.Add(deepNestingStream(20, true)) // beyond the depth limit below (#1020)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = readMixed(t, data, ReaderOptions{
			SequenceRecovery: SequenceRecoveryOptions{UndefinedLengthOBOWAsSQ: true, VOILUTSequenceOWAsSQ: true, ReplaceWrongDelimiter: true, PrivateSequenceAsBytes: true},
			MaxSequenceDepth: 16, MaxElements: 4096, MaxElementBytes: 1 << 16, MaxTotalBytes: 1 << 16,
		})
	})
}
