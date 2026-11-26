package pathtemplate

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/object"
)

var patientID = core.NewTag(0x0010, 0x0020)
var patientName = core.NewTag(0x0010, 0x0010)
var studyDate = core.NewTag(0x0008, 0x0020)

func textElement(tag core.Tag, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}
func newObject(elements ...core.Element) *object.Object {
	return object.FromElements(elements, std.Dictionary)
}
func mustCharset(t *testing.T, name string) dicomenc.SpecificCharacterSet {
	t.Helper()
	charset, err := dicomenc.ParseCharacterSet(name)
	if err != nil {
		t.Fatal(err)
	}
	return charset
}
func format(t *testing.T, pattern string, opts Options, src *object.Object) (string, error) {
	t.Helper()
	compiled, err := Compile(pattern, opts)
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Format(src)
}

func TestTagsKeywordsIndexesAndLiteralEscapes(t *testing.T) {
	src := newObject(textElement(patientID, core.VRLO, "abc"), textElement(core.NewTag(0x0008, 0x0060), core.VRCS, "MR"), textElement(studyDate, core.VRDA, "20240228"), textElement(core.NewTag(0x0008, 0x0008), core.VRCS, "ORIGINAL", "PRIMARY"), textElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.3"))
	pattern := "archive/{Modality}/{00100020}/{StudyDate,date,+1d}/{ImageType[1]}/{{object}}-{SOPInstanceUID}.dcm"
	got, err := format(t, pattern, Options{}, src)
	want := "archive/MR/abc/20240229/PRIMARY/{object}-1.2.3.dcm"
	if err != nil || got != want {
		t.Fatalf("got %q err %v want %q", got, err, want)
	}
	if id, _ := src.GetString(patientID); id != "abc" {
		t.Fatal("source attribute changed")
	}
	got, err = format(t, "{patientid}", Options{}, src)
	if err != nil || got != "abc" {
		t.Fatal("case-insensitive keyword resolution")
	}
}

func TestDigestEncodingIsExplicitAndNotDcm4che(t *testing.T) {
	// Published SHA-256/MD5 test-vector input "abc"; these are UTF-8 hex,
	// not Java String.hashCode or dcm4che's custom 26 character MD5 encoding.
	src := newObject(core.NewRawElement(patientID, core.VRLO, []byte("abc ")))
	for _, tc := range []struct{ modifier, want string }{
		{"hash", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"sha256", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"md5", "900150983cd24fb0d6963f7d28e17f72"},
	} {
		got, err := format(t, "{PatientID,"+tc.modifier+"}", Options{}, src)
		if err != nil || got != tc.want {
			t.Fatalf("%s => %q %v", tc.modifier, got, err)
		}
	}
	// Source input budgets must not be applied to the longer digest output.
	got, err := format(t, "{PatientID,sha256}", Options{MaxAttributeBytes: 3}, newObject(textElement(patientID, core.VRLO, "abc")))
	if err != nil || len(got) != 64 {
		t.Fatalf("digest expansion/input budget: %q %v", got, err)
	}
}

func TestMissingAndMultiplicityPolicies(t *testing.T) {
	for _, src := range []*object.Object{nil, newObject(), newObject(textElement(patientID, core.VRLO, "")), newObject(core.NewRawElement(patientID, core.VRLO, nil))} {
		got, err := format(t, "{PatientID}.dcm", Options{}, src)
		if !errors.Is(err, ErrMissing) || got != "" {
			t.Fatalf("missing: %q %v", got, err)
		}
		got, err = format(t, "{PatientID,md5}.dcm", Options{Missing: MissingSubstitute, Replacement: "unknown/name"}, src)
		if err != nil || got != "unknown_name.dcm" {
			t.Fatalf("fallback is literal component, not hashed/path: %q %v", got, err)
		}
	}
	src := newObject(textElement(patientID, core.VRLO, "first", "second"))
	if got, err := format(t, "{PatientID}", Options{}, src); !errors.Is(err, ErrMultiplicity) || got != "" {
		t.Fatalf("defaultVM: %q %v", got, err)
	}
	if got, err := format(t, "{PatientID}", Options{Multiplicity: VMFirst}, src); err != nil || got != "first" {
		t.Fatalf("firstVM: %q %v", got, err)
	}
	if got, err := format(t, "{PatientID[1]}", Options{}, src); err != nil || got != "second" {
		t.Fatalf("indexedVM: %q %v", got, err)
	}
	if got, err := format(t, "{PatientID[2]}", Options{}, src); !errors.Is(err, ErrMissing) || got != "" {
		t.Fatalf("absentVM: %q %v", got, err)
	}
	if got, err := format(t, "{PatientID[2]}", Options{Missing: MissingSubstitute}, src); err != nil || got != "_" {
		t.Fatalf("VMfallback: %q %v", got, err)
	}
	if got, err := format(t, "literal/file.dcm", Options{}, nil); err != nil || got != "literal/file.dcm" {
		t.Fatalf("nilsource literal: %q %v", got, err)
	}
	for _, tpl := range []*Template{nil, {}} {
		if _, err := tpl.Format(nil); !errors.Is(err, ErrSyntax) {
			t.Fatalf("uncompiledTemplate: %v", err)
		}
	}
}

func TestAttributeSanitizationKeepsOneComponent(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"../a/b\\c:stream", ".._a_b_c_stream"}, {".", "_"}, {"..", "_"}, {"", "_"},
		{"abc. ", "abc"}, {"CON.txt", "_CON.txt"}, {"con .txt", "_con .txt"}, {"LPT¹.txt", "_LPT¹.txt"},
		{"a\x00\nb", "a__b"}, {"a<>:\"|?*b", "a_______b"}, {"a\u202eb", "a_b"},
	} {
		got, err := SanitizeAttribute(tc.value)
		if err != nil || got != tc.want {
			t.Fatalf("%q => %q %v want %q", tc.value, got, err, tc.want)
		}
		if err := ValidateRelativePath(got); err != nil || strings.ContainsAny(got, "/\\") {
			t.Fatalf("not one safe component %q %v", got, err)
		}
		if tc.value != "" {
			path, err := format(t, "folder/{PatientID}.dcm", Options{}, newObject(textElement(patientID, core.VRLO, tc.value)))
			if err != nil || path != "folder/"+tc.want+".dcm" {
				t.Fatalf("inserted separator/component: %q %v", path, err)
			}
		}
	}
	for c := 0; c < 256; c++ {
		value := "prefix" + string(rune(c)) + "suffix"
		got, err := SanitizeAttribute(value)
		if err != nil || ValidateRelativePath("folder/"+got) != nil || strings.ContainsAny(got, "/\\") {
			t.Fatalf("rune%d escaped component: %q %v", c, got, err)
		}
	}
	if _, err := SanitizeAttribute(string([]byte{0xff})); !errors.Is(err, ErrValue) {
		t.Fatal("invalidUTF8 accepted")
	}
}

func TestLiteralValidationAndMixedComponentRecheck(t *testing.T) {
	unsafe := []string{"", "/root/file", "//server/share", "../file", "./file", "a/../file", "a//file", "a/", "C:/file", "C:file", `\\server\share`, `\root`, `\\?\C:\file`, `\\.\NUL`, "CON", "NUL.txt", "COM1/x", "LPT³.txt", "dir/file:stream", "dir/name.", "dir/name ", "dir/na\x00me", "dir/na\nme", "dir/file?"}
	for _, value := range unsafe {
		if err := ValidateRelativePath(value); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("accepted path %q: %v", value, err)
		}
		if _, err := Compile(value, Options{}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("literal silently sanitized %q: %v", value, err)
		}
	}
	for _, pattern := range []string{"../{PatientID}", "/{PatientID}", "C:{PatientID}", `{PatientID}\file`, "NUL/{PatientID}", "{PatientID}/.", "{PatientID}/..", "{PatientID}/", "{PatientID}.", "{PatientID} "} {
		if _, err := Compile(pattern, Options{}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("unsafe literals around field %q: %v", pattern, err)
		}
	}
	if got, err := format(t, "C{PatientID}N", Options{}, newObject(textElement(patientID, core.VRLO, "O"))); !errors.Is(err, ErrUnsafePath) || got != "" {
		t.Fatalf("mixedcomponent device escaped recheck: %q %v", got, err)
	}
}

func TestSyntaxAndUnsupportedModifiers(t *testing.T) {
	for _, pattern := range []string{"{", "}", "{PatientID", "{PatientID}}", "{}", "{NotAnAttribute}", "{0010002Z}", "{PatientID[-1]}", "{PatientID[]}", "{PatientID[2147483648]}", "{PatientID[0][1]}", "{00100020/00100010}", "{PatientID,{md5}}"} {
		if _, err := Compile(pattern, Options{}); !errors.Is(err, ErrSyntax) {
			t.Fatalf("syntax%q: %v", pattern, err)
		}
	}
	for _, pattern := range []string{"{PatientID,upper}", "{PatientID,number}", "{PatientID,offset,1}", "{PatientID,md5,extra}", "{StudyDate,date,P1D}", "{StudyDate,date,+1m}", "{StudyDate,date,1d}", "{StudyDate,date,+3652426d}", "{StudyDate,date,+-1d}"} {
		if _, err := Compile(pattern, Options{}); !errors.Is(err, ErrModifier) {
			t.Fatalf("modifier%q: %v", pattern, err)
		}
	}
}

func TestDateOffsetsAndPrecisionBounds(t *testing.T) {
	for _, tc := range []struct{ date, offset, want string }{
		{"20240228", "+1d", "20240229"}, {"20240229", "+1d", "20240301"},
		{"20240101", "-1d", "20231231"}, {"09991231", "+1d", "10000101"}, {"00000101", "+0d", "00000101"},
	} {
		got, err := format(t, "{StudyDate,date,"+tc.offset+"}", Options{}, newObject(textElement(studyDate, core.VRDA, tc.date)))
		if err != nil || got != tc.want {
			t.Fatalf("date%s%s => %q %v", tc.date, tc.offset, got, err)
		}
	}
	for _, tc := range []struct {
		vr            core.VR
		value, offset string
	}{
		{core.VRDA, "99991231", "+1d"}, {core.VRDA, "00000101", "-1d"},
		{core.VRDA, "2024", "+0d"}, {core.VRDA, "20240230", "+1d"},
		{core.VRDA, "2024.02.28", "+0d"}, {core.VRDT, "20240101120000+0530", "+1d"},
	} {
		got, err := format(t, "{00080020,date,"+tc.offset+"}", Options{}, newObject(textElement(studyDate, tc.vr, tc.value)))
		if !errors.Is(err, ErrValue) || got != "" {
			t.Fatalf("invented date precision/bounds %q %v", got, err)
		}
	}
}

func TestBudgetsBeforeDecodingAndOutputExpansion(t *testing.T) {
	for _, opts := range []Options{{MaxTemplateBytes: -1}, {MaxOutputBytes: -1}, {MaxAttributeBytes: -1}, {MaxExpansions: -1}, {MaxComponentBytes: -1}, {MaxComponents: -1}} {
		if _, err := Compile("file", opts); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("negative budget: %v", err)
		}
	}
	for _, tc := range []struct {
		pattern string
		opts    Options
	}{
		{strings.Repeat("a", 4097), Options{}}, {"{PatientID}{PatientID}{PatientID}", Options{MaxExpansions: 2}},
		{strings.Repeat("a/", 64) + "a", Options{}}, {strings.Repeat("a", 256), Options{}},
		{"file", Options{Replacement: strings.Repeat("a", 4097)}},
	} {
		if _, err := Compile(tc.pattern, tc.opts); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("compilebudget: %v", err)
		}
	}
	for _, tc := range []struct {
		pattern string
		opts    Options
		src     *object.Object
	}{
		{"{PatientID}", Options{}, newObject(textElement(patientID, core.VRLO, strings.Repeat("a", 4097)))},
		{"{PatientID}", Options{}, newObject(core.Element{Header: core.ElementHeader{Tag: patientID, VR: core.VRLO, Length: 1, LengthSet: true}, Value: core.RawValue(strings.Repeat("a", 4097))})},
		{"{PatientID}", Options{}, newObject(textElement(patientID, core.VRLO, make([]string, 4098)...))},
		{"{PatientName}", Options{}, newObject(core.NewRawElement(patientName, core.VRPN, []byte("Name")), core.NewRawElement(charsetTag, core.VRCS, []byte(strings.Repeat("a", 4097))))},
		{"{PatientID}", Options{}, newObject(textElement(patientID, core.VRLO, strings.Repeat("a", 256)))},
		{"{PatientID}{PatientID}", Options{MaxOutputBytes: 5}, newObject(textElement(patientID, core.VRLO, "abc"))},
		{"{PatientID,sha256}", Options{MaxOutputBytes: 63}, newObject(textElement(patientID, core.VRLO, "abc"))},
	} {
		got, err := format(t, tc.pattern, tc.opts, tc.src)
		if !errors.Is(err, ErrLimitExceeded) || got != "" {
			t.Fatalf("formatbudget: %q %v", got, err)
		}
	}
}

func TestUnsupportedValuesNeverBecomeMissingFallback(t *testing.T) {
	for _, value := range []core.Value{nil, core.BulkDataValue{URI: "file:///private/unread.dcm"}, core.DiscardedValue{}, core.SequenceValue{}, core.FragmentSequence{Fragments: [][]byte{{1, 2}}}} {
		src := newObject(core.Element{Header: core.ElementHeader{Tag: patientID, VR: core.VRLO, Length: 100, LengthSet: true}, Value: value})
		got, err := format(t, "prefix/{PatientID}.dcm", Options{Missing: MissingSubstitute}, src)
		if !errors.Is(err, ErrValue) || got != "" {
			t.Fatalf("unavailable value produced success: %q %v", got, err)
		}
	}
	for _, modifier := range []string{"", ",sha256", ",md5"} {
		src := newObject(textElement(patientID, core.VRLO, string([]byte{0xff})))
		got, err := format(t, "{PatientID"+modifier+"}", Options{}, src)
		if !errors.Is(err, ErrValue) || got != "" {
			t.Fatalf("invalid text hidden by modifier%s: %q %v", modifier, got, err)
		}
	}
}

func TestNumericHelpersByteOrderAndUnsignedRange(t *testing.T) {
	tag := core.NewTag(0x0066, 0x1000)
	for _, tc := range []struct {
		vr    core.VR
		value core.Value
		want  string
	}{
		{core.VRUS, core.Uint16Value{65535}, "65535"}, {core.VRSS, core.Int16Value{-32768}, "-32768"},
		{core.VRUL, core.Uint32Value{4294967295}, "4294967295"}, {core.VRSL, core.Int32Value{-2147483648}, "-2147483648"},
		{core.VRUV, core.Uint64Value{math.MaxUint64}, "18446744073709551615"}, {core.VRSV, core.Int64Value{math.MinInt64}, "-9223372036854775808"},
		{core.VRFL, core.Float32Value{1.25}, "1.25"}, {core.VRFD, core.Float64Value{1.25}, "1.25"}, {core.VRAT, core.TagValue{patientID}, "00100020"},
	} {
		src := newObject(core.Element{Header: core.ElementHeader{Tag: tag, VR: tc.vr}, Value: tc.value})
		got, err := format(t, "{00661000}", Options{}, src)
		if err != nil || got != tc.want {
			t.Fatalf("typed%s: %q %v want %q", tc.vr, got, err, tc.want)
		}
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, tc := range []struct {
			vr    core.VR
			value uint64
			width int
			want  string
		}{
			{core.VRUS, 65535, 2, "65535"}, {core.VRSS, 32768, 2, "-32768"},
			{core.VRUL, 4294967295, 4, "4294967295"}, {core.VRSL, 2147483648, 4, "-2147483648"},
			{core.VRUV, math.MaxUint64, 8, "18446744073709551615"}, {core.VRSV, uint64(1) << 63, 8, "-9223372036854775808"},
			{core.VRFL, uint64(math.Float32bits(1.25)), 4, "1.25"}, {core.VRFD, math.Float64bits(1.25), 8, "1.25"},
		} {
			raw := make([]byte, tc.width)
			switch tc.width {
			case 2:
				order.PutUint16(raw, uint16(tc.value))
			case 4:
				order.PutUint32(raw, uint32(tc.value))
			case 8:
				order.PutUint64(raw, tc.value)
			}
			src := newObject(core.NewRawElement(tag, tc.vr, raw))
			src.SetValueByteOrder(order)
			got, err := format(t, "{00661000}", Options{}, src)
			if err != nil || got != tc.want {
				t.Fatalf("raw%s%s: %q %v", order, tc.vr, got, err)
			}
		}
	}
	for _, src := range []*object.Object{newObject(core.NewRawElement(tag, core.VRUS, []byte{1})), newObject(core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUS}, Value: core.Int16Value{-1}})} {
		if got, err := format(t, "{00661000}", Options{}, src); !errors.Is(err, ErrValue) || got != "" {
			t.Fatalf("malformednumeric %q %v", got, err)
		}
	}
}

func TestCharsetInheritanceFallbackAndConcurrentTemplateUse(t *testing.T) {
	name := core.NewRawElement(patientName, core.VRPN, []byte("M\xfcller/.."))
	charset := textElement(charsetTag, core.VRCS, "ISO_IR 100")
	src := newObject(charset, name)
	before := append([]byte(nil), name.Value.(core.RawValue)...)
	got, err := format(t, "people/{PatientName}.dcm", Options{}, src)
	if err != nil || got != "people/Müller_.dcm" || !reflect.DeepEqual(name.Value.(core.RawValue), core.RawValue(before)) {
		t.Fatalf("charset/valueownership: %q %v", got, err)
	}
	sequenceTag := core.NewTag(0x0040, 0xA730)
	parent := newObject(charset, core.Element{Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{name}}}}})
	items, ok := parent.GetSequence(sequenceTag)
	if !ok {
		t.Fatal("sequence setup")
	}
	if got, err := format(t, "{PatientName}", Options{}, items[0]); err != nil || got != "Müller_" {
		t.Fatalf("inherited charset: %q %v", got, err)
	}
	fallback := object.FromElementsWithTextOptions([]core.Element{name}, std.Dictionary, object.TextOptions{CharsetFallback: object.FallbackUndeclared, FallbackCharacterSet: mustCharset(t, "ISO_IR 100")})
	if got, err := format(t, "{PatientName}", Options{}, fallback); err != nil || got != "Müller_" {
		t.Fatalf("Object fallback: %q %v", got, err)
	}
	tpl, err := Compile("{PatientName}.dcm", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			own := newObject(charset, name)
			for n := 0; n < 20; n++ {
				if got, err := tpl.Format(own); err != nil || got != "Müller_.dcm" {
					t.Errorf("concurrentTemplate: %q %v", got, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

type mutableDictionary struct{ tag core.Tag }

func (d *mutableDictionary) ByTag(tag core.Tag) (dictionary.Entry, bool) {
	return dictionary.Entry{Tag: d.tag, VR: core.VRLO, Keyword: "SiteKey"}, tag == d.tag
}
func (d *mutableDictionary) ByKeyword(keyword string) (dictionary.Entry, bool) {
	return dictionary.Entry{Tag: d.tag, VR: core.VRLO, Keyword: "SiteKey"}, strings.EqualFold(keyword, "SiteKey")
}
func TestCompiledKeywordDoesNotRetainMutableDictionary(t *testing.T) {
	dict := &mutableDictionary{patientID}
	tpl, err := Compile("{SiteKey}", Options{Dictionary: dict})
	if err != nil {
		t.Fatal(err)
	}
	dict.tag = patientName
	got, err := tpl.Format(newObject(textElement(patientID, core.VRLO, "old"), textElement(patientName, core.VRPN, "new")))
	if err != nil || got != "old" {
		t.Fatalf("compiledselector changed: %q %v", got, err)
	}
}
