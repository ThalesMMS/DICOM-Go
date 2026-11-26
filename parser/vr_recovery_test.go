package parser

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func loadWrongVRVectors(t *testing.T) (def, dictLen []byte, asWritten, dictionary map[string]string) {
	t.Helper()
	raw, err := os.ReadFile("../testdata/upstream/dcmtk/tparser_wrong_explicit_vr.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Default    string            `json:"default"`
		DictLength string            `json:"dict_length"`
		AsWritten  map[string]string `json:"expected_vrs_as_written"`
		Dictionary map[string]string `json:"expected_vrs_dictionary_preferred"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	def, _ = hex.DecodeString(v.Default)
	dictLen, _ = hex.DecodeString(v.DictLength)
	return def, dictLen, v.AsWritten, v.Dictionary
}

// The DCMTK tparser matrix: dcmPreferVRFromDataDictionary x
// dcmPreferLengthFieldSizeFromDataDictionary.
func TestDCMTKPreferDictionaryVRMatrix(t *testing.T) {
	def, dictLen, asWritten, dictionary := loadWrongVRVectors(t)
	for _, c := range []struct{ vr, length bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		opts := ReaderOptions{VRRecovery: VRRecoveryOptions{PreferDictionaryVR: c.vr, PreferDictionaryLengthFieldSize: c.length}, MaxElementBytes: 1 << 20}
		working, broken := def, dictLen
		if c.length {
			working, broken = dictLen, def
		}
		elements, recs, err := readMixed(t, working, opts)
		if err != nil {
			t.Fatalf("%+v: working vector: %v", c, err)
		}
		want := asWritten
		if c.vr {
			want = dictionary
		}
		got := map[string]string{}
		for _, el := range elements {
			got[el.Header.Tag.String()] = string(el.Header.VR)
		}
		for tag, vr := range want {
			if got[tag] != vr {
				t.Errorf("%+v: %s VR = %s, want %s", c, tag, got[tag], vr)
			}
		}
		if (c.vr || c.length) && len(recs) == 0 {
			t.Errorf("%+v: no recovery reported", c)
		}
		for _, rec := range recs {
			if rec.DeclaredVR == "" || rec.UsedVR == "" {
				t.Errorf("%+v: recovery without VRs: %+v", c, rec)
			}
		}
		if _, _, err := readMixed(t, broken, opts); err == nil {
			t.Errorf("%+v: broken vector accepted", c)
		}
	}
}

func TestOXPixelDataReadAsOW(t *testing.T) {
	data := explicitLong(0x7fe0, 0x0010, "OX", []byte{1, 2, 3, 4})
	if _, _, err := readMixed(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted OX")
	}
	elements, recs, err := readMixed(t, data, ReaderOptions{VRRecovery: VRRecoveryOptions{MapOXToOW: true}})
	if err != nil {
		t.Fatal(err)
	}
	if elements[0].Header.VR != core.VROW || len(recs) != 1 || recs[0].DeclaredVR != "OX" || recs[0].UsedVR != core.VROW {
		t.Fatalf("element %+v recoveries %+v", elements[0].Header, recs)
	}
}

func explicitLong(g, e uint16, vr string, value []byte) []byte {
	b := make([]byte, 12, 12+len(value))
	binary.LittleEndian.PutUint16(b[0:], g)
	binary.LittleEndian.PutUint16(b[2:], e)
	copy(b[4:], vr)
	binary.LittleEndian.PutUint32(b[8:], uint32(len(value)))
	return append(b, value...)
}

func TestPrivateULLength6(t *testing.T) {
	var data []byte
	ul := explicitShort(0x0009, 0x1113, "UL", []byte("    "))
	binary.LittleEndian.PutUint16(ul[6:], 6) // declared 6, four value bytes follow
	data = append(data, ul...)
	data = append(data, explicitShort(0x0009, 0x1114, "UL", []byte{1, 0, 0, 0})...)
	if _, _, err := readMixed(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted the misaligned stream")
	}
	elements, recs, err := readMixed(t, data, ReaderOptions{VRRecovery: VRRecoveryOptions{CorrectPrivateULLength: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 2 || elements[0].Header.Length != 4 || len(recs) != 1 || recs[0].Reason != BoundaryPrivateULLength {
		t.Fatalf("elements %d recoveries %+v", len(elements), recs)
	}
}

func TestShortLengthUN(t *testing.T) {
	short := func(g, e uint16, value []byte) []byte { return explicitShort(g, e, "UN", value) }
	var data []byte
	data = append(data, short(0x0019, 0x1214, []byte("     0"))...) // nonzero reserved bytes
	data = append(data, short(0x0019, 0x1241, nil)...)              // zero length: implausible 32-bit length
	data = append(data, short(0x0019, 0x1242, []byte("1 "))...)
	for _, recover := range []bool{false, true} {
		elements, recs, err := readMixed(t, data, ReaderOptions{VRRecovery: VRRecoveryOptions{ShortLengthUN: recover}})
		if !recover {
			if err == nil {
				t.Fatal("strict read accepted 16-bit UN lengths")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(elements) != 3 || len(recs) != 3 {
			t.Fatalf("elements %d recoveries %+v", len(elements), recs)
		}
		if raw, _ := elements[2].RawBytes(); string(raw) != "1 " {
			t.Fatalf("last value %q", raw)
		}
	}
	// A standard UN is unaffected.
	std := explicitLong(0x0019, 0x1010, "UN", []byte{1, 2})
	if _, recs, err := readMixed(t, std, ReaderOptions{VRRecovery: VRRecoveryOptions{ShortLengthUN: true}}); err != nil || len(recs) != 0 {
		t.Fatalf("standard UN: %v %+v", err, recs)
	}
}

// DCMTK telemlen: Explicit VR Little Endian header size of an empty element
// is 12 bytes for the 4-byte length VRs and 8 bytes otherwise.
func TestExplicitHeaderLengthByVR(t *testing.T) {
	long := []core.VR{"OB", "OD", "OF", "OL", "OV", "OW", "SV", "UV", "SQ", "UC", "UR", "UT", "UN"}
	short := []core.VR{"AE", "AS", "AT", "CS", "DA", "DS", "DT", "FL", "FD", "IS", "LO", "PN", "SH", "SL", "SS", "ST", "TM", "UI", "UL", "US", "LT"}
	check := func(vr core.VR, want int) {
		if got := vr.UsesLongExplicitLength(); got != (want == 12) {
			t.Errorf("%s UsesLongExplicitLength = %v", vr, got)
		}
		var buf bytes.Buffer
		w := NewWriter(&buf, transfer.ExplicitVRLittleEndian)
		el := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: vr}, Value: core.RawValue{}}
		if vr == core.VRSQ {
			el.Value = core.SequenceValue{}
		}
		if err := w.WriteElement(el); err != nil {
			t.Errorf("%s write: %v", vr, err)
			return
		}
		if vr == core.VRSQ {
			want += 8 // written with undefined length and a Sequence Delimitation Item
		}
		if buf.Len() != want {
			t.Errorf("%s header = %d bytes, want %d", vr, buf.Len(), want)
		}
	}
	for _, vr := range long {
		check(vr, 12)
	}
	for _, vr := range short {
		check(vr, 8)
	}
}
