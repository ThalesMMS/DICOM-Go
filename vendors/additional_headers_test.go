package vendors_test

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/vendors"
	"github.com/ThalesMMS/dicom-go/vendors/vendorfixture"
)

func TestAdditionalHeadersRenderThroughCreatorLocations(t *testing.T) {
	o := object.New(std.Dictionary)
	vendorfixture.PutAdditional(o)
	headers := vendors.DecodePrivateHeaders(o, vendors.PrivateHeaderOptions{})
	if len(headers) != 8 {
		t.Fatalf("headers %d", len(headers))
	}
	seen := map[vendors.PrivateHeaderKind]int{}
	for _, h := range headers {
		if h.Err != nil {
			t.Errorf("%s: %v", h.Kind, h.Err)
			continue
		}
		if !h.MayIdentify || len(h.Lines) == 0 {
			t.Errorf("header %+v", h)
		}
		seen[h.Kind]++
		text := strings.Join(h.Lines, "\n")
		want := vendorfixture.AdditionalValue
		switch h.Kind {
		case vendors.PhilipsSDSHeader:
			want = "AcquisitionCount = 42"
			if !strings.Contains(text, "ScanLabel = Synthetic Scan") {
				t.Error(text)
			}
		case vendors.CanonPASHeader:
			want = "Synthetic PAS"
		}
		if !strings.Contains(text, want) {
			t.Errorf("%s missing %q: %s", h.Kind, want, text)
		}
	}
	if seen[vendors.ToshibaPMTFHeader] != 2 {
		t.Errorf("Toshiba offset01+02 %v", seen)
	}
	only := vendors.DecodePrivateHeaders(o, vendors.PrivateHeaderOptions{Kinds: []vendors.PrivateHeaderKind{vendors.PhilipsSDSHeader}, MaxLines: 1})
	if len(only) != 1 || !only[0].Truncated || len(only[0].Lines) != 1 {
		t.Fatalf("selection %+v", only)
	}
	// A numeric block without the correct reservation is never detected.
	noCreator := object.New(nil)
	noCreator.Put(core.NewRawElement(core.NewTag(0x700d, 0x1019), core.VROB, []byte("<PAS/>")))
	if len(vendors.DetectPrivateHeaders(noCreator)) != 0 {
		t.Fatal("guessed creator")
	}
}
func TestPhilipsSDSVersion8TypedValuesAndMalformedSpans(t *testing.T) {
	raw := vendorfixture.PhilipsSDSBlob()
	before := bytes.Clone(raw)
	v, err := vendors.DecodePhilipsSDS(raw, vendors.Limits{})
	if err != nil || len(v) != 2 || v[0].Values[0] != "42" || v[1].Values[0] != "Synthetic Scan" {
		t.Fatalf("%+v %v", v, err)
	}
	v[0].Values[0] = "changed"
	if !bytes.Equal(raw, before) {
		t.Fatal("mutated input")
	}
	for name, edit := range map[string]func([]byte){"directory-count": func(b []byte) { binary.LittleEndian.PutUint32(b[28:], 3) }, "offset-overflow": func(b []byte) { binary.LittleEndian.PutUint32(b[78:], math.MaxUint32) }, "payload-in-table": func(b []byte) { binary.LittleEndian.PutUint32(b[78:], 0) }, "unterminated-name": func(b []byte) { copy(b[32:66], bytes.Repeat([]byte{'a'}, 34)) }, "unterminated-value": func(b []byte) { copy(b[136:], bytes.Repeat([]byte{'b'}, 81)) }, "zero-values": func(b []byte) { binary.LittleEndian.PutUint32(b[70:], 0) }} {
		t.Run(name, func(t *testing.T) {
			b := bytes.Clone(raw)
			edit(b)
			if _, err := vendors.DecodePhilipsSDS(b, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
				t.Fatalf("%v", err)
			}
		})
	}
	for _, l := range []vendors.Limits{{MaxBytes: len(raw) - 1}, {MaxEntries: 2}, {MaxDecodedBytes: 1}} {
		if _, err := vendors.DecodePhilipsSDS(raw, l); !errors.Is(err, vendors.ErrLimit) {
			t.Fatalf("limit %+v: %v", l, err)
		}
	}
	for name, b := range map[string][]byte{"direct": {1}, "version": func() []byte { b := bytes.Clone(raw); binary.LittleEndian.PutUint32(b[24:], 9); return b }(), "type": func() []byte { b := bytes.Clone(raw); binary.LittleEndian.PutUint32(b[66:], 3); return b }()} {
		t.Run(name, func(t *testing.T) {
			if _, err := vendors.DecodePhilipsSDS(b, vendors.Limits{}); !errors.Is(err, vendors.ErrUnsupportedHeader) {
				t.Fatal(err)
			}
		})
	}
}
func TestCT3AndReversedPMTFRejectMalformedInput(t *testing.T) {
	raw := vendorfixture.ToshibaCT3Blob()
	o, err := vendors.DecodeToshibaCT3(raw, vendors.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := o.GetString(core.NewTag(0x0010, 0x0010)); s != vendorfixture.AdditionalValue {
		t.Fatal(s)
	}
	for _, pos := range []int{0, 2, 4, 8, 12, 14, 16, 20} {
		b := bytes.Clone(raw)
		b[pos]++
		if _, err := vendors.DecodeToshibaCT3(b, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
			t.Fatalf("offset%d %v", pos, err)
		}
	}
	if _, err := vendors.DecodeToshibaCT3(raw, vendors.Limits{MaxBytes: 24}); !errors.Is(err, vendors.ErrLimit) {
		t.Fatal(err)
	}
	rev := vendorfixture.PMTFBlob()
	before := bytes.Clone(rev)
	o, err = vendors.DecodePMTF(rev, vendors.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rev, before) {
		t.Fatal("reverse mutated input")
	}
	if s, _ := o.GetString(core.NewTag(0x0010, 0x0010)); s != vendorfixture.AdditionalValue {
		t.Fatal(s)
	}
	for _, bad := range [][]byte{nil, {1}, bytes.Repeat([]byte{0xff}, 10)} {
		if _, err := vendors.DecodePMTF(bad, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err := vendors.DecodePMTF(rev, vendors.Limits{MaxBytes: 1}); !errors.Is(err, vendors.ErrLimit) {
		t.Fatal(err)
	}
}
func TestPrivateSequenceRequiresItemCreatorAndBoundsBeforeFacades(t *testing.T) {
	o := object.New(nil)
	vendorfixture.PutAdditional(o)
	tag := core.NewTag(0x0029, 0x1101)
	el, _ := o.Get(tag)
	sq := el.Value.(core.SequenceValue)
	sq.Items[0].Elements = sq.Items[0].Elements[1:]
	el.Value = sq
	o.Put(el)
	if _, err := vendors.PMTFOf(o, "PMTF INFORMATION DATA", 1, vendors.Limits{}); !errors.Is(err, vendors.ErrNotFound) {
		t.Fatal(err)
	}
	o = object.New(nil)
	vendorfixture.PutAdditional(o)
	if _, err := vendors.PhilipsSDSOf(o, vendors.Limits{MaxEntries: 1}); !errors.Is(err, vendors.ErrLimit) {
		t.Fatal(err)
	}
	if _, err := vendors.PMTFOf(o, "other", 1, vendors.Limits{}); !errors.Is(err, vendors.ErrUnsupportedHeader) {
		t.Fatal(err)
	}
}
func TestCanonPASUTF8XMLAndLimits(t *testing.T) {
	raw := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?><PAS><Name>José</Name></PAS>\x00")
	s, err := vendors.DecodeCanonPAS(raw, vendors.Limits{})
	if err != nil || !strings.Contains(s, "José") || strings.HasSuffix(s, "\x00") {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := vendors.DecodeCanonPAS(append([]byte{0xef, 0xbb, 0xbf}, raw...), vendors.Limits{}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte("<a/><b/>"), []byte("<a><?xml version=\"1.0\"?></a>"), []byte("<a>"), []byte("text<a/>"), []byte("<a>&unbound;</a>"), []byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><a/>"), {'<', 'a', '>', 0xff, '<', '/', 'a', '>'}} {
		if _, err := vendors.DecodeCanonPAS(bad, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := vendors.DecodeCanonPAS([]byte("<!DOCTYPE a><a/>"), vendors.Limits{}); !errors.Is(err, vendors.ErrUnsupportedHeader) {
		t.Fatal(err)
	}
	for _, l := range []vendors.Limits{{MaxBytes: 1}, {MaxDecodedBytes: 1}, {MaxEntries: 1}} {
		if _, err := vendors.DecodeCanonPAS(raw, l); !errors.Is(err, vendors.ErrLimit) {
			t.Fatal(err)
		}
	}
	if _, err := vendors.DecodeCanonPAS([]byte(strings.Repeat("<a>", 65)+strings.Repeat("</a>", 65)), vendors.Limits{}); !errors.Is(err, vendors.ErrLimit) {
		t.Fatal(err)
	}
}
func TestMECMR3GroupsValuesErrorsAndExplicitEncoding(t *testing.T) {
	raw := vendorfixture.MECMR3Blob()
	before := bytes.Clone(raw)
	records, err := vendors.DecodeMECMR3(raw, vendors.Limits{})
	if err != nil || len(records) != 6 || records[0].Values[0] != vendorfixture.AdditionalValue || records[5].Group != 6 || records[5].Values[0] != "6" {
		t.Fatalf("%+v %v", records, err)
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("mutated")
	}
	for _, l := range []vendors.Limits{{MaxBytes: 1}, {MaxEntries: 1}, {MaxDecodedBytes: 1}} {
		if _, err := vendors.DecodeMECMR3(raw, l); !errors.Is(err, vendors.ErrLimit) {
			t.Fatal(err)
		}
	}
	for name, edit := range map[string]func([]byte){"count": func(b []byte) { binary.LittleEndian.PutUint32(b, 0) }, "span": func(b []byte) { binary.LittleEndian.PutUint32(b[12:], math.MaxUint32) }, "signature": func(b []byte) { binary.LittleEndian.PutUint32(b[24:], 11) }} {
		t.Run(name, func(t *testing.T) {
			b := bytes.Clone(raw)
			edit(b)
			if _, err := vendors.DecodeMECMR3(b, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
				t.Fatal(err)
			}
		})
	}
	b := bytes.Clone(raw)
	binary.LittleEndian.PutUint32(b[8:], 0xdeadbeef)
	if _, err := vendors.DecodeMECMR3(b, vendors.Limits{}); !errors.Is(err, vendors.ErrUnsupportedHeader) {
		t.Fatal(err)
	}
	if _, err := vendors.DecodeMECMR3(append(raw, 1), vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
		t.Fatal(err)
	}
	// Replace the first ASCII payload with Shift-JIS bytes for the word "日本".
	n := int(binary.LittleEndian.Uint32(raw[12:]))
	sjis := []byte{0x93, 0xfa, 0x96, 0x7b, 0}
	b = append(bytes.Clone(raw[:36]), sjis...)
	b = append(b, raw[36+n:]...)
	binary.LittleEndian.PutUint32(b[12:], uint32(len(sjis)))
	if _, err := vendors.DecodeMECMR3(b, vendors.Limits{}); !errors.Is(err, vendors.ErrUnsupportedHeader) {
		t.Fatal(err)
	}
	v, err := vendors.DecodeMECMR3WithOptions(b, vendors.MECMR3Options{TextEncoding: "Shift-JIS"})
	if err != nil || v[0].Values[0] != "日本" {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestPhilipsSDSNumericTypes(t *testing.T) {
	for _, tc := range []struct {
		typ, bits uint32
		want      string
	}{{0, math.Float32bits(-1.25), "-1.25"}, {1, uint32(0xfffffff9), "-7"}, {4, 42, "42"}} {
		raw := vendorfixture.PhilipsSDSBlob()
		binary.LittleEndian.PutUint32(raw[66:], tc.typ)
		binary.LittleEndian.PutUint32(raw[132:], tc.bits)
		v, err := vendors.DecodePhilipsSDS(raw, vendors.Limits{})
		if err != nil || v[0].Values[0] != tc.want {
			t.Fatalf("type%d %+v %v", tc.typ, v, err)
		}
	}
	raw := vendorfixture.PhilipsSDSBlob()
	binary.LittleEndian.PutUint32(raw[66:], 0)
	binary.LittleEndian.PutUint32(raw[132:], math.Float32bits(float32(math.Inf(1))))
	if _, err := vendors.DecodePhilipsSDS(raw, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
		t.Fatal(err)
	}
}
func mecRecordFixture(typ uint32, payload []byte) []byte {
	raw := vendorfixture.MECMR3Blob()
	n := int(binary.LittleEndian.Uint32(raw[12:]))
	b := append(bytes.Clone(raw[:36]), payload...)
	b = append(b, raw[36+n:]...)
	binary.LittleEndian.PutUint32(b[8:], typ)
	binary.LittleEndian.PutUint32(b[12:], uint32(len(payload)))
	return b
}
func TestMECMR3KnownTypesAndNumericDomains(t *testing.T) {
	u32 := func(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
	fixed := func(n int, s string, offset int) []byte { b := make([]byte, n); copy(b[offset:], s); return b }
	latinMarked := append([]byte{0xdf, 0xff, 0x79, 0, 1, 9, 0}, []byte("ISO8859-1")...)
	latinMarked = append(latinMarked, 2, 2, 0, 'A', 0)
	latinMarked[3] = byte(len(latinMarked) - 4)
	for _, tc := range []struct {
		typ     uint32
		payload []byte
		want    string
	}{
		{0xff002a00, u32(1), "true"}, {0xff002400, u32(0xfffffff9), "-7"}, {0xff000800, u32(math.Float32bits(1.25)), "1.25"}, {0xff002800, append(u32(0), u32(math.Float32bits(1.5))...), "1.5"}, {0xff002900, binary.LittleEndian.AppendUint64(nil, math.Float64bits(2.5)), "2.5"},
		{0x500, append(u32(0), u32(math.Float32bits(1.5))...), "1.5"}, {0x600, append(append(u32(0), u32(math.Float32bits(1.5))...), u32(0)...), "1.5"},
		{0x300, []byte("ASCII\x00"), "ASCII"}, {0x300, latinMarked, "ISO8859-1 marker: A"}, {0xe00, []byte("11/12/2002,11:27:32\x00"), "11/12/2002,11:27:32"},
		{0x1f4000, fixed(340, "STR40", 4), "STR40"}, {0x1f4100, fixed(136, "1.2.3", 4), "1.2.3"}, {0x1f4300, fixed(436, "TM_MR_DCM", 4), "TM_MR_DCM"}, {0x1f4400, fixed(516, "none", 0), "none"}, {0x1f4600, fixed(325, "Synthetic", 0), "Synthetic"}, {0xff002300, []byte{1, 2}, "charset bytes: 0102 (not interpreted)"},
	} {
		t.Run(strconv.FormatUint(uint64(tc.typ), 16)+tc.want, func(t *testing.T) {
			v, err := vendors.DecodeMECMR3(mecRecordFixture(tc.typ, tc.payload), vendors.Limits{})
			if err != nil || !strings.Contains(strings.Join(v[0].Values, "|"), tc.want) {
				t.Fatalf("%+v %v", v, err)
			}
		})
	}
	for _, tc := range []struct {
		typ uint32
		p   []byte
	}{{0xff002a00, u32(2)}, {0xff000800, u32(math.Float32bits(float32(math.NaN())))}, {0xff002900, binary.LittleEndian.AppendUint64(nil, math.Float64bits(math.Inf(1)))}, {0x500, u32(0)}, {0x600, append(u32(0), u32(0)...)}, {0x1f4600, make([]byte, 324)}} {
		if _, err := vendors.DecodeMECMR3(mecRecordFixture(tc.typ, tc.p), vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
			t.Fatalf("type%x %v", tc.typ, err)
		}
	}
}
func TestTruncatedAdditionalBinaryFormatsNeverPanic(t *testing.T) {
	for name, tc := range map[string]struct {
		data   []byte
		decode func([]byte) error
	}{
		"SDS":  {vendorfixture.PhilipsSDSBlob(), func(b []byte) error { _, err := vendors.DecodePhilipsSDS(b, vendors.Limits{}); return err }},
		"CT3":  {vendorfixture.ToshibaCT3Blob(), func(b []byte) error { _, err := vendors.DecodeToshibaCT3(b, vendors.Limits{}); return err }},
		"PMTF": {vendorfixture.PMTFBlob(), func(b []byte) error { _, err := vendors.DecodePMTF(b, vendors.Limits{}); return err }},
		"MEC":  {vendorfixture.MECMR3Blob(), func(b []byte) error { _, err := vendors.DecodeMECMR3(b, vendors.Limits{}); return err }},
	} {
		t.Run(name, func(t *testing.T) {
			for n := 0; n < len(tc.data); n++ {
				if err := tc.decode(tc.data[:n]); err == nil {
					t.Fatalf("prefix%d accepted", n)
				}
			}
		})
	}
}

func TestCT3Part10SyntaxAndContainerBudget(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian} {
		o := object.New(nil)
		for _, v := range []struct {
			tag   core.Tag
			value string
		}{{core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.7"}, {core.NewTag(0x0008, 0x0018), "1.2.3.1320.3"}, {core.NewTag(0x0010, 0x0010), vendorfixture.AdditionalValue}} {
			o.Put(core.Element{Header: core.ElementHeader{Tag: v.tag, VR: core.VRUI}, Value: core.StringValue{v.value}})
		}
		// PN text VR for the invented name; UIDs retain UI.
		el, _ := o.Get(core.NewTag(0x0010, 0x0010))
		el.Header.VR = core.VRPN
		o.Put(el)
		o.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1115), VR: core.VRSQ}, Value: core.SequenceValue{Items: make([]core.DataSet, 8)}})
		f := &object.File{Dataset: o, TransferSyntax: syntax}
		if err := f.RebuildFileMeta(); err != nil {
			t.Fatal(err)
		}
		var encoded bytes.Buffer
		if err := object.WriteFile(&encoded, f); err != nil {
			t.Fatal(err)
		}
		payload := encoded.Bytes()
		var raw []byte
		for i, v := range []uint32{12, uint32(len(payload))} {
			raw = binary.LittleEndian.AppendUint16(raw, 0)
			raw = binary.LittleEndian.AppendUint16(raw, uint16(i))
			raw = binary.LittleEndian.AppendUint32(raw, 4)
			raw = binary.LittleEndian.AppendUint32(raw, v)
		}
		raw = append(raw, payload...)
		decoded, err := vendors.DecodeToshibaCT3(raw, vendors.Limits{})
		if err != nil {
			t.Fatalf("%s: %v", syntax.UID, err)
		}
		if s, _ := decoded.GetString(core.NewTag(0x0010, 0x0010)); s != vendorfixture.AdditionalValue {
			t.Fatal(s)
		}
		if _, err := vendors.DecodeToshibaCT3(raw, vendors.Limits{MaxEntries: 12}); !errors.Is(err, vendors.ErrLimit) {
			t.Fatalf("%s countlimit %v", syntax.UID, err)
		}
		if syntax.Deflated {
			if _, err := vendors.DecodeToshibaCT3(raw, vendors.Limits{MaxDecodedBytes: 1}); !errors.Is(err, vendors.ErrLimit) {
				t.Fatal(err)
			}
		}
	}
}

func TestCanonPASCountsAttributes(t *testing.T) {
	if _, err := vendors.DecodeCanonPAS([]byte(`<a x="1" y="2"/>`), vendors.Limits{MaxEntries: 2}); !errors.Is(err, vendors.ErrLimit) {
		t.Fatal(err)
	}
}
func TestCT3DeflatePaddingLimitAndTrailingBytes(t *testing.T) {
	o := object.New(nil)
	for _, v := range []struct {
		tag   core.Tag
		value string
	}{{core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.7"}, {core.NewTag(0x0008, 0x0018), "1.2.3.1320.4"}} {
		o.Put(core.Element{Header: core.ElementHeader{Tag: v.tag, VR: core.VRUI}, Value: core.StringValue{v.value}})
	}
	var found bool
	for n := 1; n <= 64; n++ {
		o.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0020), VR: core.VRLO}, Value: core.StringValue{strings.Repeat("X", n)}})
		f := &object.File{Dataset: o, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}
		if err := f.RebuildFileMeta(); err != nil {
			t.Fatal(err)
		}
		var encoded bytes.Buffer
		if err := object.WriteFile(&encoded, f); err != nil {
			t.Fatal(err)
		}
		payload := encoded.Bytes()
		source := bytes.NewReader(payload)
		h, err := object.ReadPart10Header(source, object.ReadFileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, flate.NewReader(source)); err != nil {
			t.Fatal(err)
		}
		if source.Len() != 1 {
			continue
		}
		found = true
		wrapper := func(p []byte) []byte {
			var raw []byte
			for i, v := range []uint32{12, uint32(len(p))} {
				raw = binary.LittleEndian.AppendUint16(raw, 0)
				raw = binary.LittleEndian.AppendUint16(raw, uint16(i))
				raw = binary.LittleEndian.AppendUint32(raw, 4)
				raw = binary.LittleEndian.AppendUint32(raw, v)
			}
			return append(raw, p...)
		}
		raw := wrapper(payload)
		if _, err := vendors.DecodeToshibaCT3(raw, vendors.Limits{}); err != nil {
			t.Fatalf("valid odd stream %v", err)
		}
		inflated, err := io.ReadAll(flate.NewReader(bytes.NewReader(payload[h.DataSetOffset:])))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vendors.DecodeToshibaCT3(raw, vendors.Limits{MaxDecodedBytes: len(inflated)}); err != nil {
			t.Fatalf("exact bound: %v", err)
		}
		if _, err := vendors.DecodeToshibaCT3(raw, vendors.Limits{MaxDecodedBytes: len(inflated) - 1}); !errors.Is(err, vendors.ErrLimit) {
			t.Fatalf("over bound: %v", err)
		}
		if _, err := vendors.DecodeToshibaCT3(raw, vendors.Limits{MaxDecodedBytes: int(^uint(0) >> 1)}); err != nil {
			t.Fatalf("MaxInt bound: %v", err)
		}
		bad := bytes.Clone(payload)
		bad[len(bad)-1] = 1
		if _, err := vendors.DecodeToshibaCT3(wrapper(bad), vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
			t.Fatalf("nonzero pad: %v", err)
		}
		if _, err := vendors.DecodeToshibaCT3(wrapper(append(bytes.Clone(payload), 0)), vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
			t.Fatalf("extra pad: %v", err)
		}
		break
	}
	if !found {
		t.Fatal("finite corpus has no odd deflate stream")
	}
}

func TestCT3Part10FileMetaRejectsContainersBeforeObjectRead(t *testing.T) {
	element := func(group, number uint16, vr string, value []byte) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint16(b, group)
		binary.LittleEndian.PutUint16(b[2:], number)
		copy(b[4:], vr)
		if vr == "SQ" {
			b = append(b, make([]byte, 4)...)
			binary.LittleEndian.PutUint32(b[8:], uint32(len(value)))
		} else {
			binary.LittleEndian.PutUint16(b[6:], uint16(len(value)))
		}
		return append(b, value...)
	}
	wrapper := func(meta []byte) []byte {
		length := make([]byte, 4)
		binary.LittleEndian.PutUint32(length, uint32(len(meta)))
		part := append(make([]byte, 128), []byte("DICM")...)
		part = append(part, element(2, 0, "UL", length)...)
		part = append(part, meta...)
		part = append(part, element(0x10, 0x10, "PN", []byte("TEST"))...)
		raw := make([]byte, 24)
		binary.LittleEndian.PutUint32(raw[4:], 4)
		binary.LittleEndian.PutUint32(raw[8:], 12)
		binary.LittleEndian.PutUint16(raw[14:], 1)
		binary.LittleEndian.PutUint32(raw[16:], 4)
		binary.LittleEndian.PutUint32(raw[20:], uint32(len(part)))
		return append(raw, part...)
	}
	meta := element(2, 16, "UI", []byte("1.2.840.10008.1.2.1\x00"))
	valid := wrapper(meta)
	if _, err := vendors.DecodeToshibaCT3(valid, vendors.Limits{MaxEntries: 3}); err != nil {
		t.Fatalf("exact aggregate budget: %v", err)
	}
	if _, err := vendors.DecodeToshibaCT3(valid, vendors.Limits{MaxEntries: 2}); !errors.Is(err, vendors.ErrLimit) {
		t.Fatalf("meta/dataset shared budget: %v", err)
	}
	for _, count := range []int{0, 64} {
		var items []byte
		for i := 0; i < count; i++ {
			b := make([]byte, 8)
			binary.LittleEndian.PutUint16(b, 0xfffe)
			binary.LittleEndian.PutUint16(b[2:], 0xe000)
			items = append(items, b...)
		}
		malformed := wrapper(append(bytes.Clone(meta), element(2, 0xffff, "SQ", items)...))
		if _, err := vendors.DecodeToshibaCT3(malformed, vendors.Limits{MaxEntries: 16}); !errors.Is(err, vendors.ErrInvalid) {
			t.Fatalf("%d empty File Meta items: %v", count, err)
		}
	}
	// Group length must not smuggle a dataset element into File Meta.
	malformed := wrapper(append(bytes.Clone(meta), element(0x10, 0x20, "LO", []byte("ID"))...))
	if _, err := vendors.DecodeToshibaCT3(malformed, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
		t.Fatalf("wrong File Meta group: %v", err)
	}
	truncated := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(truncated[24+140:], uint32(len(valid)))
	if _, err := vendors.DecodeToshibaCT3(truncated, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
		t.Fatalf("File Meta exceeds container: %v", err)
	}
}

// Methods must never be consulted while measuring an untrusted opaque value.
type opaquePrivateHeaderValue struct{}

func (opaquePrivateHeaderValue) Kind() core.ValueKind { panic("opaque Kind called") }
func (opaquePrivateHeaderValue) EncodedLength() (core.Length, bool) {
	panic("opaque EncodedLength called")
}

func TestPrivateSequenceBudgetUsesResidentSizeWithShortHeaders(t *testing.T) {
	makeObject := func() (*object.Object, core.Tag, core.Element, core.SequenceValue) {
		o := object.New(nil)
		vendorfixture.PutAdditional(o)
		tag, err := o.PrivateTag(0x2005, "Philips MR Imaging DD 002", 0x32)
		if err != nil {
			t.Fatal(err)
		}
		el, _ := o.Get(tag)
		sq := el.Value.(core.SequenceValue)
		return o, tag, el, sq
	}
	for _, short := range []bool{false, true} {
		o, _, el, sq := makeObject()
		sq.Items = append(sq.Items, sq.Items[0])
		for i := range sq.Items {
			sq.Items[i].Elements = append([]core.Element(nil), sq.Items[i].Elements...)
			for j := range sq.Items[i].Elements {
				e := &sq.Items[i].Elements[j]
				if e.Tag().Element == 0x1944 && short {
					e.Header.Length = 1
					e.Header.LengthSet = true
				}
			}
		}
		el.Value = sq
		o.Put(el)
		if _, err := vendors.PhilipsSDSOf(o, vendors.Limits{MaxBytes: 300}); !errors.Is(err, vendors.ErrLimit) {
			t.Fatalf("shortHeader=%v aggregate: %v", short, err)
		}
	}
	cases := []struct {
		name  string
		value core.Value
		bytes int
	}{
		{"raw", core.RawValue(make([]byte, 21)), 21},
		{"strings", core.StringValue{"abc", "defgh"}, 10},
		{"u16", core.Uint16Value{1, 2, 3}, 6}, {"i16", core.Int16Value{1, 2, 3}, 6},
		{"u32", core.Uint32Value{1, 2, 3}, 12}, {"i32", core.Int32Value{1, 2, 3}, 12},
		{"f32", core.Float32Value{1, 2, 3}, 12}, {"tag", core.TagValue{{Group: 1}, {Group: 2}, {Group: 3}}, 12},
		{"u64", core.Uint64Value{1, 2, 3}, 24}, {"i64", core.Int64Value{1, 2, 3}, 24}, {"f64", core.Float64Value{1, 2, 3}, 24},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, _, el, sq := makeObject()
			base := 0
			for _, e := range sq.Items[0].Elements {
				n, ok := e.CalculatedLength()
				if !ok {
					t.Fatal("fixture primitive")
				}
				base += int(n)
			}
			sq.Items[0].Elements = append(sq.Items[0].Elements, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x2005, 0x19aa), VR: core.VROB, Length: 1, LengthSet: true}, Value: tc.value})
			el.Value = sq
			o.Put(el)
			if _, err := vendors.PhilipsSDSOf(o, vendors.Limits{MaxBytes: base + tc.bytes}); err != nil {
				t.Fatalf("exact resident budget: %v", err)
			}
			if _, err := vendors.PhilipsSDSOf(o, vendors.Limits{MaxBytes: base + tc.bytes - 1}); !errors.Is(err, vendors.ErrLimit) {
				t.Fatalf("resident budget minus one: %v", err)
			}
		})
	}
	for _, value := range []core.Value{opaquePrivateHeaderValue{}, nil, core.BulkDataValue{URI: "urn:synthetic"}, core.FragmentSequence{}} {
		o, _, el, sq := makeObject()
		sq.Items[0].Elements = append(sq.Items[0].Elements, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x2005, 0x19aa), VR: core.VROB, Length: 1, LengthSet: true}, Value: value})
		el.Value = sq
		o.Put(el)
		if _, err := vendors.PhilipsSDSOf(o, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
			t.Fatalf("unsupported value %T: %v", value, err)
		}
	}
}

func TestDirectPrivateHeaderValueIsCheckedBeforeOpaqueMethods(t *testing.T) {
	cases := []struct {
		name    string
		group   uint16
		creator string
		offset  uint8
		decode  func(*object.Object, vendors.Limits) error
	}{
		{"CT3", 0x7005, "TOSHIBA_MEC_CT3", 0x10, func(o *object.Object, l vendors.Limits) error {
			_, err := vendors.ToshibaCT3Of(o, l)
			return err
		}},
		{"PAS", 0x700d, "CANON_MEC_MR3^10", 0x19, func(o *object.Object, l vendors.Limits) error {
			_, err := vendors.CanonPASOf(o, l)
			return err
		}},
		{"MR3", 0x700d, "TOSHIBA_MEC_MR3", 0x08, func(o *object.Object, l vendors.Limits) error {
			_, err := vendors.ToshibaMECMR3Of(o, l)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			makeObject := func(value core.Value, length core.Length, set bool) *object.Object {
				o := object.New(nil)
				_, err := o.ReservePrivateCreator(tc.group, tc.creator)
				if err != nil {
					t.Fatal(err)
				}
				tag, err := o.PrivateTag(tc.group, tc.creator, tc.offset)
				if err != nil {
					t.Fatal(err)
				}
				o.Put(core.Element{
					Header: core.ElementHeader{Tag: tag, VR: core.VROB, Length: length, LengthSet: set},
					Value:  value,
				})
				return o
			}
			// Finite oversized storage with a misleading short header must be
			// refused before the privateValue path clones the payload.
			for _, set := range []bool{false, true} {
				o := makeObject(core.RawValue(make([]byte, 17)), 1, set)
				if err := tc.decode(o, vendors.Limits{MaxBytes: 16}); !errors.Is(err, vendors.ErrLimit) {
					t.Fatalf("short header set=%v: %v", set, err)
				}
			}
			for _, set := range []bool{false, true} {
				for _, value := range []core.Value{
					opaquePrivateHeaderValue{}, nil, core.BulkDataValue{URI: "urn:synthetic"}, core.FragmentSequence{},
					core.StringValue{"not a raw blob"}, core.Uint16Value{1},
				} {
					o := makeObject(value, 1, set)
					if err := tc.decode(o, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
						t.Fatalf("value %T header set=%v: %v", value, set, err)
					}
				}
			}
			o := makeObject(core.RawValue{0}, core.UndefinedLength, true)
			if err := tc.decode(o, vendors.Limits{}); !errors.Is(err, vendors.ErrInvalid) {
				t.Fatalf("undefined primitive header: %v", err)
			}
		})
	}
}

func TestDirectPrivateHeaderValueByteBudgetAtBoundary(t *testing.T) {
	cases := []struct {
		name    string
		group   uint16
		creator string
		offset  uint8
		decode  func(*object.Object, vendors.Limits) error
	}{
		{"CT3", 0x7005, "TOSHIBA_MEC_CT3", 0x10, func(o *object.Object, l vendors.Limits) error {
			_, err := vendors.ToshibaCT3Of(o, l)
			return err
		}},
		{"PAS", 0x700d, "CANON_MEC_MR3^10", 0x19, func(o *object.Object, l vendors.Limits) error {
			_, err := vendors.CanonPASOf(o, l)
			return err
		}},
		{"MR3", 0x700d, "TOSHIBA_MEC_MR3", 0x08, func(o *object.Object, l vendors.Limits) error {
			_, err := vendors.ToshibaMECMR3Of(o, l)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := object.New(nil)
			vendorfixture.PutAdditional(o)
			tag, err := o.PrivateTag(tc.group, tc.creator, tc.offset)
			if err != nil {
				t.Fatal(err)
			}
			el, ok := o.Get(tag)
			if !ok {
				t.Fatal("missing fixture private value")
			}
			raw, ok := el.Value.(core.RawValue)
			if !ok {
				t.Fatalf("fixture %T", el.Value)
			}
			for _, set := range []bool{false, true} {
				el.Header.Length, el.Header.LengthSet = 1, set
				o.Put(el)
				if err := tc.decode(o, vendors.Limits{MaxBytes: len(raw)}); err != nil {
					t.Fatalf("exact budget header set=%v: %v", set, err)
				}
				if err := tc.decode(o, vendors.Limits{MaxBytes: len(raw) - 1}); !errors.Is(err, vendors.ErrLimit) {
					t.Fatalf("minus one header set=%v: %v", set, err)
				}
			}
			el.Header.Length, el.Header.LengthSet = core.Length(len(raw)+1), true
			o.Put(el)
			if err := tc.decode(o, vendors.Limits{MaxBytes: len(raw)}); !errors.Is(err, vendors.ErrLimit) {
				t.Fatalf("larger declared header: %v", err)
			}
		})
	}
}
