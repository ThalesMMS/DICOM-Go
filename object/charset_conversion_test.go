package object_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Synthetic names only.
const (
	latinName    = "José^Ñúñez"
	latinNameISO = "Jos\xe9^\xd1\xfa\xf1ez"
	latinPlace   = "Clínica Été"
	latinPlaceIS = "Cl\xednica \xc9t\xe9"
	japaneseName = "Yamada^Tarou=山田^太郎=やまだ^たろう"
	japaneseRaw  = "Yamada^Tarou=\x1b$B;3ED\x1b(B^\x1b$BB@O:\x1b(B=\x1b$B$d$^$@\x1b(B^\x1b$B$?$m$&\x1b(B"
	polishName   = "Łukasz^Wójcik"
)

var (
	tagCharset       = core.NewTag(0x0008, 0x0005)
	tagPatientName   = core.NewTag(0x0010, 0x0010)
	tagInstitution   = core.NewTag(0x0008, 0x0080)
	tagModality      = core.NewTag(0x0008, 0x0060)
	tagRefSeries     = core.NewTag(0x0008, 0x1115)
	tagPrivateCreatr = core.NewTag(0x0029, 0x0010)
	tagRows          = core.NewTag(0x0028, 0x0010)
	tagStudyDesc     = core.NewTag(0x0008, 0x1030)
)

func text(tag core.Tag, vr core.VR, value string) core.Element {
	return core.NewRawElement(tag, vr, vr.PadToEvenLength([]byte(value)))
}

func item(elements ...core.Element) core.DataSet { return core.DataSet{Elements: elements} }

func sequence(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true}, Value: core.SequenceValue{Items: items}}
}

func charsetObject(options object.TextOptions, elements ...core.Element) *object.Object {
	return object.FromElementsWithTextOptions(elements, std.Dictionary, options)
}

func TestTextFallbackOnlyInAuthorizedCases(t *testing.T) {
	latin1 := dicomenc.ISOIR100
	undeclared := []core.Element{text(tagPatientName, core.VRPN, latinNameISO)}
	contradictory := []core.Element{text(tagCharset, core.VRCS, "ISO_IR 192"), text(tagPatientName, core.VRPN, latinNameISO)}
	emptyDeclaration := []core.Element{text(tagCharset, core.VRCS, ""), text(tagPatientName, core.VRPN, latinNameISO)}
	unknown := []core.Element{text(tagCharset, core.VRCS, "ISO_IR 999"), text(tagPatientName, core.VRPN, latinNameISO)}
	for _, tc := range []struct {
		name     string
		elements []core.Element
		options  object.TextOptions
		want     bool
	}{
		{"undeclared, default", undeclared, object.TextOptions{}, false},
		{"undeclared, fallback without authorization", undeclared, object.TextOptions{FallbackCharacterSet: latin1}, false},
		{"undeclared, undeclared authorized", undeclared, object.TextOptions{FallbackCharacterSet: latin1, CharsetFallback: object.FallbackUndeclared}, true},
		{"undeclared, only undecodable authorized", undeclared, object.TextOptions{FallbackCharacterSet: latin1, CharsetFallback: object.FallbackUndecodable}, false},
		{"empty declaration is undeclared", emptyDeclaration, object.TextOptions{FallbackCharacterSet: latin1, CharsetFallback: object.FallbackUndeclared}, true},
		{"ISO_IR 192 over Latin-1, default", contradictory, object.TextOptions{}, false},
		{"ISO_IR 192 over Latin-1, only undeclared authorized", contradictory, object.TextOptions{FallbackCharacterSet: latin1, CharsetFallback: object.FallbackUndeclared}, false},
		{"ISO_IR 192 over Latin-1, undecodable authorized", contradictory, object.TextOptions{FallbackCharacterSet: latin1, CharsetFallback: object.FallbackUndecodable}, true},
		{"unknown term, default", unknown, object.TextOptions{}, false},
		{"unknown term, cases do not cover it", unknown, object.TextOptions{FallbackCharacterSet: latin1, CharsetFallback: object.FallbackUndeclared | object.FallbackUndecodable}, false},
		{"unknown term, unsupported authorized", unknown, object.TextOptions{FallbackCharacterSet: latin1, AllowUnsupportedCharsetFallback: true}, true},
	} {
		obj := charsetObject(tc.options, tc.elements...)
		got, err := obj.LookupString(tagPatientName)
		if tc.want != (err == nil) || tc.want && got != latinName {
			t.Errorf("%s: %q %v", tc.name, got, err)
		}
	}
	// Valid text in the declared repertoire never takes the fallback, and a
	// fallback that cannot decode the bytes is an error, not replacement text.
	utf8 := charsetObject(object.TextOptions{FallbackCharacterSet: latin1, CharsetFallback: object.FallbackUndecodable}, text(tagCharset, core.VRCS, "ISO_IR 192"), text(tagPatientName, core.VRPN, polishName))
	if got, err := utf8.LookupString(tagPatientName); err != nil || got != polishName {
		t.Fatalf("valid UTF-8: %q %v", got, err)
	}
	strictFallback := charsetObject(object.TextOptions{FallbackCharacterSet: dicomenc.DefaultCharacterSet, CharsetFallback: object.FallbackUndeclared}, undeclared...)
	if _, err := strictFallback.LookupString(tagPatientName); err == nil {
		t.Fatal("fallback that cannot decode the bytes produced text")
	}
	// Items inherit the declaration state: an undeclared parent keeps the
	// item undeclared, an override makes it declared.
	nested := charsetObject(object.TextOptions{FallbackCharacterSet: latin1, CharsetFallback: object.FallbackUndeclared},
		sequence(tagRefSeries, item(text(tagPatientName, core.VRPN, latinNameISO)), item(text(tagCharset, core.VRCS, "ISO_IR 192"), text(tagPatientName, core.VRPN, latinNameISO))))
	items, _ := nested.GetSequence(tagRefSeries)
	if got, err := items[0].LookupString(tagPatientName); err != nil || got != latinName {
		t.Fatalf("inherited undeclared item: %q %v", got, err)
	}
	if _, err := items[1].LookupString(tagPatientName); err == nil {
		t.Fatal("declared item used the undeclared fallback")
	}
}

// preservingFixture mixes scopes: an ISO_IR 100 data set, an inheriting item,
// an ISO 2022 override, a UTF-8 override, text that must not change (CS,
// Private Creator) and a binary value.
func preservingFixture() []core.Element {
	return []core.Element{
		text(tagCharset, core.VRCS, "ISO_IR 100"),
		text(tagModality, core.VRCS, "OT"),
		text(tagInstitution, core.VRLO, latinPlaceIS),
		sequence(tagRefSeries,
			item(text(tagPatientName, core.VRPN, latinNameISO)),
			item(core.NewRawElement(tagCharset, core.VRCS, []byte(`\ISO 2022 IR 87`)), text(tagPatientName, core.VRPN, japaneseRaw)),
			item(text(tagCharset, core.VRCS, "ISO_IR 192"), text(tagPatientName, core.VRPN, polishName)),
		),
		text(tagPatientName, core.VRPN, latinNameISO),
		text(tagPrivateCreatr, core.VRLO, "SYNTHETIC CS"),
		core.NewRawElement(tagRows, core.VRUS, []byte{0x00, 0x02}),
	}
}

func TestConvertTextToUTF8ConvertsEveryScope(t *testing.T) {
	source := charsetObject(object.TextOptions{}, preservingFixture()...)
	before := mustWrite(t, source, transfer.ExplicitVRLittleEndian)
	converted, report, err := source.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.ConvertedValues != 5 || report.FallbackValues != 0 || report.KeptScopes != 0 {
		t.Fatalf("report %+v", report)
	}
	// The receiver still writes its original bytes.
	if !bytes.Equal(mustWrite(t, source, transfer.ExplicitVRLittleEndian), before) {
		t.Fatal("conversion modified its receiver")
	}
	raw := func(obj *object.Object, tag core.Tag) string {
		value, _ := obj.GetRaw(tag)
		return string(value)
	}
	if got := strings.TrimRight(raw(converted, tagCharset), " "); got != "ISO_IR 192" {
		t.Fatalf("top declaration %q", got)
	}
	if got := strings.TrimRight(raw(converted, tagPatientName), " "); got != latinName {
		t.Fatalf("PN bytes %q", got)
	}
	if got := strings.TrimRight(raw(converted, tagInstitution), " "); got != latinPlace {
		t.Fatalf("LO bytes %q", got)
	}
	for _, tag := range []core.Tag{tagModality, tagPrivateCreatr, tagRows} {
		if raw(converted, tag) != raw(source, tag) {
			t.Fatalf("%s changed", tag)
		}
	}
	items, _ := converted.GetSequence(tagRefSeries)
	if _, own := items[0].Get(tagCharset); own {
		t.Fatal("inheriting item got its own declaration")
	}
	for i, want := range []string{latinName, japaneseName, polishName} {
		if got := strings.TrimRight(raw(items[i], tagPatientName), " "); got != want {
			t.Fatalf("item %d bytes %q", i, got)
		}
		if i > 0 && strings.TrimRight(raw(items[i], tagCharset), " ") != "ISO_IR 192" {
			t.Fatalf("item %d override not converted", i)
		}
	}
	// Reopened independently of the conversion, strictly, the characters
	// match; JSON carries the same text.
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		reread, err := object.ReadDataSet(bytes.NewReader(mustWrite(t, converted, syntax)), syntax)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := reread.LookupString(tagPatientName); err != nil || got != latinName {
			t.Fatalf("%s: reread %q %v", syntax.Name, got, err)
		}
		rereadItems, _ := reread.GetSequence(tagRefSeries)
		for i, want := range []string{latinName, japaneseName, polishName} {
			if got, err := rereadItems[i].LookupString(tagPatientName); err != nil || got != want {
				t.Fatalf("%s: item %d %q %v", syntax.Name, i, got, err)
			}
		}
	}
	document, err := dicomjson.Marshal(converted, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"José", "Ñúñez", "山田", "Łukasz", "Clínica", "ISO_IR 192"} {
		if !bytes.Contains(document, []byte(want)) {
			t.Fatalf("JSON lacks %q", want)
		}
	}
}

func TestConvertTextToUTF8UsesOnlyAuthorizedFallbacks(t *testing.T) {
	// Latin-1 bytes declared ISO_IR 192 (the upstream mislabel) convert only
	// with FallbackUndecodable, and are reported.
	elements := []core.Element{text(tagCharset, core.VRCS, "ISO_IR 192"), text(tagPatientName, core.VRPN, latinNameISO), text(tagStudyDesc, core.VRLO, "ASCII")}
	if _, _, err := charsetObject(object.TextOptions{}, elements...).ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{}); !errors.Is(err, object.ErrCharsetConversion) {
		t.Fatalf("unauthorized fallback: %v", err)
	}
	options := object.TextOptions{FallbackCharacterSet: dicomenc.ISOIR100, CharsetFallback: object.FallbackUndecodable}
	converted, report, err := charsetObject(options, elements...).ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := converted.LookupString(tagPatientName); err != nil || got != latinName {
		t.Fatalf("converted %q %v", got, err)
	}
	if report.FallbackValues != 1 || len(report.Diagnostics) != 1 || report.Diagnostics[0] != (object.CharsetDiagnostic{Path: "(0010,0010)", Reason: object.CharsetFallbackUndecodable, Declared: "ISO_IR 192"}) {
		t.Fatalf("report %+v", report)
	}
	// An unsupported term needs AllowUnsupportedCharsetFallback; its
	// diagnostic reduces the arbitrary term to a bounded safe form.
	odd := "ISO_IR 999\x01<b>" + strings.Repeat("X", 80)
	unknown := []core.Element{text(tagCharset, core.VRCS, odd), text(tagPatientName, core.VRPN, latinNameISO)}
	_, _, err = charsetObject(object.TextOptions{}, unknown...).ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{})
	var conversionErr *object.CharsetConversionError
	if !errors.As(err, &conversionErr) || conversionErr.Reason != object.CharsetUnsupported || strings.ContainsAny(conversionErr.Declared, "\x01<>") || len(conversionErr.Declared) > 64 {
		t.Fatalf("unsupported: %v", err)
	}
	converted, report, err = charsetObject(object.TextOptions{FallbackCharacterSet: dicomenc.ISOIR100, AllowUnsupportedCharsetFallback: true}, unknown...).ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{})
	if err != nil || report.Diagnostics[0].Reason != object.CharsetFallbackUnsupported || !strings.HasPrefix(report.Diagnostics[0].Declared, "ISO_IR 999??b?XXX") {
		t.Fatalf("unsupported fallback: %+v %v", report, err)
	}
	if got, _ := converted.LookupString(tagPatientName); got != latinName {
		t.Fatalf("unsupported fallback text %q", got)
	}
}

func TestConvertTextToUTF8FailsAtomicallyOrKeepsScope(t *testing.T) {
	// ASCII data set; one item inherits the default repertoire but holds
	// Latin-1 bytes, which nothing is authorized to decode.
	elements := []core.Element{
		text(tagStudyDesc, core.VRLO, "ASCII STUDY"),
		sequence(tagRefSeries, item(text(tagPatientName, core.VRPN, latinNameISO)), item(text(tagPatientName, core.VRPN, "PLAIN^NAME"))),
	}
	source := charsetObject(object.TextOptions{}, elements...)
	_, _, err := source.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{})
	var conversionErr *object.CharsetConversionError
	if !errors.As(err, &conversionErr) || conversionErr.Path != "(0008,1115)[0].(0010,0010)" || conversionErr.Reason != object.CharsetUndecodable {
		t.Fatalf("atomic: %v", err)
	}
	if strings.Contains(err.Error(), "Jos") || strings.Contains(err.Error(), "\xe9") {
		t.Fatal("error exposes text")
	}
	converted, report, err := source.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{Policy: object.CharsetConversionKeepScope})
	if err != nil {
		t.Fatal(err)
	}
	if report.KeptScopes != 1 || report.ConvertedValues != 2 {
		t.Fatalf("report %+v", report)
	}
	items, _ := converted.GetSequence(tagRefSeries)
	// The kept item pins its original (default) repertoire under the UTF-8
	// parent; its bytes are unchanged and read exactly as before.
	declaration, pinned := items[0].Get(tagCharset)
	if !pinned || len(bytes.TrimSpace(declaration.Value.(core.RawValue))) != 0 {
		t.Fatalf("kept item declaration %+v", declaration)
	}
	if raw, _ := items[0].GetRaw(tagPatientName); !bytes.Equal(bytes.TrimRight(raw, " "), []byte(latinNameISO)) {
		t.Fatal("kept item bytes changed")
	}
	if _, err := items[0].LookupString(tagPatientName); err == nil {
		t.Fatal("kept item now decodes under UTF-8")
	}
	if _, own := items[1].Get(tagCharset); own {
		t.Fatal("converted sibling got a declaration")
	}
	reasons := map[object.CharsetReason]bool{}
	for _, d := range report.Diagnostics {
		reasons[d.Reason] = true
	}
	if !reasons[object.CharsetUndecodable] || !reasons[object.CharsetScopePinned] {
		t.Fatalf("diagnostics %+v", report.Diagnostics)
	}
	// A kept top-level scope leaves everything as it was.
	top := charsetObject(object.TextOptions{}, text(tagPatientName, core.VRPN, latinNameISO), sequence(tagRefSeries, item(text(tagPatientName, core.VRPN, "PLAIN"))))
	kept, report, err := top.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{Policy: object.CharsetConversionKeepScope})
	if err != nil || report.KeptScopes != 1 || kept.Has(tagCharset) {
		t.Fatalf("kept top: %+v %v", report, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := source.ConvertTextToUTF8(canceled, object.CharsetConversionOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestConvertTextToUTF8KeepsInvalidDeclaredScopes(t *testing.T) {
	for _, declaration := range []string{"ISO_IR 192", "ISO_IR 999"} {
		for _, placement := range []string{"root", "inherited item", "local item"} {
			t.Run(declaration+"/"+placement, func(t *testing.T) {
				bad := text(tagPatientName, core.VRPN, "INVALID\xff")
				elements := []core.Element{text(tagCharset, core.VRCS, declaration), bad}
				if placement != "root" {
					child := []core.Element{bad}
					parentCharset := declaration
					if placement == "local item" {
						child = append(child, text(tagCharset, core.VRCS, declaration))
						parentCharset = "ISO_IR 192"
					}
					elements = []core.Element{
						text(tagCharset, core.VRCS, parentCharset),
						sequence(tagRefSeries, item(child...), item(text(tagPatientName, core.VRPN, "PLAIN"))),
					}
				}
				source := charsetObject(object.TextOptions{}, elements...)
				if _, _, err := source.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{}); !errors.Is(err, object.ErrCharsetConversion) {
					t.Fatalf("atomic conversion: %v", err)
				}
				kept, report, err := source.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{Policy: object.CharsetConversionKeepScope})
				if err != nil || report.KeptScopes != 1 {
					t.Fatalf("keep conversion: %+v, %v", report, err)
				}
				if placement != "root" {
					items, _ := kept.GetSequence(tagRefSeries)
					kept = items[0]
				}
				if raw, _ := kept.GetRaw(tagPatientName); !bytes.Equal(raw, bad.Value.(core.RawValue)) {
					t.Fatal("kept scope changed its original text bytes")
				}
			})
		}
	}
}

// A metadata-only text value (its bytes are not in memory) cannot be
// converted, so it is never covered by a UTF-8 declaration. Readers do not
// defer text VRs; the element here stands for any caller-built one.
func TestConvertTextToUTF8RefusesDeferredText(t *testing.T) {
	deferred := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0xA160), VR: core.VRUT, Length: 4096, LengthSet: true}}
	source := charsetObject(object.TextOptions{}, text(tagPatientName, core.VRPN, "PLAIN"), deferred)
	_, _, err := source.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{})
	var conversionErr *object.CharsetConversionError
	if !errors.As(err, &conversionErr) || conversionErr.Reason != object.CharsetDeferredText {
		t.Fatalf("deferred text: %v", err)
	}
	kept, report, err := source.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{Policy: object.CharsetConversionKeepScope})
	if err != nil || report.KeptScopes != 1 || kept.Has(tagCharset) {
		t.Fatalf("kept: %+v %v", report, err)
	}
}
