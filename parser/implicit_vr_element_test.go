package parser

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func explicitShort(g, e uint16, vr string, value []byte) []byte {
	b := make([]byte, 8, 8+len(value))
	binary.LittleEndian.PutUint16(b[0:], g)
	binary.LittleEndian.PutUint16(b[2:], e)
	copy(b[4:], vr)
	binary.LittleEndian.PutUint16(b[6:], uint16(len(value)))
	return append(b, value...)
}

func implicitEl(g, e uint16, value []byte) []byte {
	b := make([]byte, 8, 8+len(value))
	binary.LittleEndian.PutUint16(b[0:], g)
	binary.LittleEndian.PutUint16(b[2:], e)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(value)))
	return append(b, value...)
}

func readMixed(t *testing.T, data []byte, opts ReaderOptions) ([]core.Element, []BoundaryRecovery, error) {
	t.Helper()
	opts.Dictionary = std.Dictionary
	r := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, opts)
	elements, err := r.ReadAll()
	recs, _ := r.BoundaryRecoveries()
	return elements, recs, err
}

func TestImplicitVRElementRecovery(t *testing.T) {
	// Public elements explicit, a private creator and element implicit.
	var data []byte
	data = append(data, explicitShort(0x0008, 0x0060, "CS", []byte("CT"))...)
	data = append(data, implicitEl(0x0009, 0x0010, []byte("ACME"))...)
	data = append(data, implicitEl(0x0009, 0x1001, []byte{1, 2, 3, 4})...)
	data = append(data, explicitShort(0x0010, 0x0020, "LO", []byte("ID12"))...)

	if _, _, err := readMixed(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted a mixed data set")
	}
	elements, recs, err := readMixed(t, data, ReaderOptions{RecoverImplicitVRElements: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 4 {
		t.Fatalf("elements = %d", len(elements))
	}
	// Implicit private creators resolve like any Implicit VR private element
	// (UN today; PS3.5 7.8.1 LO once creators are typed).
	if (elements[1].Header.VR != core.VRLO && elements[1].Header.VR != core.VRUN) || elements[2].Header.VR != core.VRUN {
		t.Fatalf("VRs = %s, %s", elements[1].Header.VR, elements[2].Header.VR)
	}
	if raw, _ := elements[2].RawBytes(); !bytes.Equal(raw, []byte{1, 2, 3, 4}) {
		t.Fatalf("private value = % x", raw)
	}
	if len(recs) != 2 || recs[0].Reason != BoundaryImplicitVRElement || recs[0].Path != "(0009,0010)" || recs[0].Offset != 10 || recs[1].DeclaredLength != 4 {
		t.Fatalf("recoveries = %+v", recs)
	}
}

func TestImplicitVRElementRecoveryRefusesImplausibleLength(t *testing.T) {
	// "\x10\x00" + "\xFF\x7F" reads as a length of 0x7FFF0010, past the source.
	data := append(explicitShort(0x0008, 0x0060, "CS", []byte("CT")), 0x09, 0x00, 0x10, 0x00, 0x10, 0x00, 0xff, 0x7f, 'A', 'B')
	if _, recs, err := readMixed(t, data, ReaderOptions{RecoverImplicitVRElements: true}); err == nil || len(recs) != 0 {
		t.Fatalf("implausible length accepted: %v %+v", err, recs)
	}
	// Pixel Data is never switched.
	pixel := implicitEl(0x7fe0, 0x0010, []byte{0, 0})
	if _, recs, err := readMixed(t, pixel, ReaderOptions{RecoverImplicitVRElements: true}); err == nil || len(recs) != 0 {
		t.Fatalf("pixel data switched: %v %+v", err, recs)
	}
}

func TestImplicitVRElementRecoveryHonoursContainerEnd(t *testing.T) {
	// A defined-length item that holds 8 bytes; the implicit element inside
	// declares 16 and must not be taken as implicit.
	inner := implicitEl(0x0009, 0x1001, make([]byte, 16))[:8]
	item := append([]byte{0xfe, 0xff, 0x00, 0xe0}, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(item[4:], uint32(len(inner)))
	item = append(item, inner...)
	seq := make([]byte, 12)
	binary.LittleEndian.PutUint16(seq[0:], 0x0008)
	binary.LittleEndian.PutUint16(seq[2:], 0x1140)
	copy(seq[4:], "SQ")
	binary.LittleEndian.PutUint32(seq[8:], uint32(len(item)))
	data := append(seq, item...)
	if _, recs, err := readMixed(t, data, ReaderOptions{RecoverImplicitVRElements: true}); err == nil || len(recs) != 0 {
		t.Fatalf("element crossing the item end accepted: %v %+v", err, recs)
	}
}

func TestImplicitVRElementRecoveryLimit(t *testing.T) {
	var data []byte
	for i := 0; i < 3; i++ {
		data = append(data, implicitEl(0x0009, uint16(0x1001+i), []byte{0, 0})...)
	}
	_, _, err := readMixed(t, data, ReaderOptions{RecoverImplicitVRElements: true, MaxImplicitVRElements: 2})
	if !errors.Is(err, ErrMaxImplicitVRElements) {
		t.Fatalf("error = %v, want ErrMaxImplicitVRElements", err)
	}
}

// DCMTK tparser wrongExplicitVRinDataset vectors (see THIRD_PARTY_NOTICES.md):
// wrong but valid VRs are kept as written, and the variant that only works
// with dictionary length sizes is rejected. The implicit switch never
// applies, because every VR is a known VR.
func TestDCMTKWrongExplicitVRVectors(t *testing.T) {
	raw, err := os.ReadFile("../testdata/upstream/dcmtk/tparser_wrong_explicit_vr.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Default    string            `json:"default"`
		DictLength string            `json:"dict_length"`
		Expected   map[string]string `json:"expected_vrs_as_written"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, recover := range []bool{false, true} {
		data, _ := hex.DecodeString(v.Default)
		elements, recs, err := readMixed(t, data, ReaderOptions{RecoverImplicitVRElements: recover})
		if err != nil {
			t.Fatalf("default vector (recover=%v): %v", recover, err)
		}
		if len(recs) != 0 {
			t.Fatalf("known VRs triggered recovery: %+v", recs)
		}
		got := map[string]string{}
		for _, el := range elements {
			got[el.Header.Tag.String()] = string(el.Header.VR)
		}
		for tag, vr := range v.Expected {
			if got[tag] != vr {
				t.Errorf("%s VR = %s, want %s", tag, got[tag], vr)
			}
		}
		data, _ = hex.DecodeString(v.DictLength)
		if _, _, err := readMixed(t, data, ReaderOptions{RecoverImplicitVRElements: recover, MaxElementBytes: 1 << 20}); err == nil {
			t.Fatalf("dictionary-length vector accepted (recover=%v)", recover)
		}
	}
}

// FuzzImplicitVRElements reads arbitrary Explicit VR data with the recovery
// on: it must never panic, exceed the switch cap or allocate past the limits.
func FuzzImplicitVRElements(f *testing.F) {
	f.Add(append(explicitShort(0x0008, 0x0060, "CS", []byte("CT")), implicitEl(0x0009, 0x1001, []byte{1, 2})...))
	f.Add(implicitEl(0x0009, 0x0010, []byte("ACME")))
	f.Add([]byte{0x08, 0x00, 0x60, 0x00, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, ReaderOptions{
			Dictionary: std.Dictionary, RecoverImplicitVRElements: true, MaxImplicitVRElements: 8,
			MaxElementBytes: 1 << 16, MaxTotalBytes: 1 << 16, MaxElements: 4096,
		})
		_, _ = r.ReadAll()
		recs, _ := r.BoundaryRecoveries()
		switches := 0
		for _, rec := range recs {
			if rec.Reason == BoundaryImplicitVRElement {
				switches++
			}
		}
		if switches > 8 {
			t.Fatalf("switches = %d", switches)
		}
	})
}
