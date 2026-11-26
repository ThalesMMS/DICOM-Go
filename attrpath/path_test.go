package attrpath

import (
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/curated"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagContentSequence  = core.NewTag(0x0040, 0xA730)
	tagCodeValue        = core.NewTag(0x0008, 0x0100)
	tagPatientID        = core.NewTag(0x0010, 0x0020)
	tagPatientName      = core.NewTag(0x0010, 0x0010)
	tagSeriesUID        = core.NewTag(0x0020, 0x000E)
	tagReferencedSeries = core.NewTag(0x0008, 0x1115)
)

func items(t *testing.T, dataset *object.Object, tag core.Tag) []*object.Object {
	t.Helper()
	sequence, ok := dataset.GetSequence(tag)
	if !ok {
		t.Fatalf("%s is not a sequence", tag)
	}
	return sequence
}

// The cases follow the path vectors of DCMTK's dcmdata tpath test: the same
// paths, the same outcome, checked through the resulting structure.
func TestDCMTKPathVectors(t *testing.T) {
	dataset := object.New(std.Dictionary)
	for _, text := range []string{
		"PatientID",
		"ContentSequence",
		"(0040,A730)",
		"(0040,A730)[5]",
		"(0040,A730)[5].ContentSequence[3].ConceptNameCodeSequence[0].CodeValue",
		"ContentSequence[5].ContentSequence[3].ConceptNameCodeSequence[0].(0008,0104)",
	} {
		p, err := Parse(text, nil)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if _, err := Create(dataset, p, Options{}); err != nil {
			t.Fatalf("create %s: %v", text, err)
		}
	}
	content := items(t, dataset, tagContentSequence)
	if len(content) != 6 {
		t.Fatalf("ContentSequence has %d items, want 6", len(content))
	}
	nested := items(t, content[5], tagContentSequence)
	concept := items(t, nested[3], core.NewTag(0x0040, 0xA043))
	if len(nested) != 4 || len(concept) != 1 || !concept[0].Has(tagCodeValue) || !concept[0].Has(core.NewTag(0x0008, 0x0104)) {
		t.Fatalf("nested structure: %d items, %d concept items", len(nested), len(concept))
	}

	for _, text := range []string{"ContentSequences", "(1234,ContentSequences", "(00X4,A730)", "(0040,A730)[-5]", "(0040,A730)[a].ContentSequence[3].ConceptNameCodeSequence[0].CodeValue"} {
		if _, err := Parse(text, nil); !errors.Is(err, ErrSyntax) {
			t.Errorf("%s: %v, want ErrSyntax", text, err)
		}
	}

	for _, text := range []string{
		"PatientID",
		"ContentSequence",
		"(0040,A730)",
		"(0040,A730)[5]",
		"(0040,A730)[5].ContentSequence[3].ConceptNameCodeSequence[0].CodeValue",
		"ContentSequence[5].ContentSequence[3].ConceptNameCodeSequence[0].(0008,0104)",
	} {
		matches, err := Find(dataset, MustParse(text))
		if err != nil || len(matches) != 1 {
			t.Errorf("find %s: %d matches, %v", text, len(matches), err)
		}
	}
	for _, text := range []string{"PatientName", "(0040,A730)[6]", "ConceptNameCodeSequence"} {
		if _, err := Find(dataset, MustParse(text)); !errors.Is(err, ErrNotFound) {
			t.Errorf("find %s: %v, want ErrNotFound", text, err)
		}
	}

	for _, text := range []string{"ContentSequence[*].ContentSequence[3].PatientName", "ContentSequence[*].ContentSequence[0].PatientID"} {
		n, err := Create(dataset, MustParse(text), Options{})
		if err != nil || n != 6 {
			t.Errorf("wildcard %s: %d, %v", text, n, err)
		}
	}
	for _, text := range []string{"SourceImageSequence[*]", "ContentSequence[*].SourceImageSequence[*]"} {
		if _, err := Create(dataset, MustParse(text), Options{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("wildcard %s: %v, want ErrNotFound", text, err)
		}
	}
	content = items(t, dataset, tagContentSequence)
	for i, item := range content {
		nested := items(t, item, tagContentSequence)
		if len(nested) < 4 || !nested[3].Has(tagPatientName) || !nested[0].Has(tagPatientID) {
			t.Fatalf("item %d: wildcard insertion missing", i)
		}
	}
}

func TestParseRoundTripAndSyntax(t *testing.T) {
	for text, want := range map[string]string{
		"ReferencedSeriesSequence[0].SeriesInstanceUID": "ReferencedSeriesSequence[0].SeriesInstanceUID",
		"(0008,1115)[*].(0020,000e)":                    "(0008,1115)[*].(0020,000E)",
		`(0029,"SIEMENS CSA HEADER",08)`:                `(0029,"SIEMENS CSA HEADER",08)`,
		"patientid":                                     "PatientID",
	} {
		p, err := Parse(text, nil)
		if err != nil || p.String() != want {
			t.Errorf("%s: %q, %v", text, p.String(), err)
		}
		if again, err := Parse(p.String(), nil); err != nil || again.String() != want {
			t.Errorf("%s does not round-trip", text)
		}
	}
	for _, text := range []string{
		"", ".", "PatientID.", "ContentSequence.PatientID", "(0008,1115)[01]", "(0008,1115)[", "(0008,1115)[1",
		`(0028,"CREATOR",10)`, `(0009,"",10)`, `(0009,"CREATOR,10)`, "(FFFE,E000)", "(0008,0018", "Patient ID",
		"(0008,1115)[99999999999]",
	} {
		if _, err := Parse(text, nil); err == nil {
			t.Errorf("%q parsed", text)
		}
	}
	if _, err := Parse(strings.Repeat("ContentSequence[0].", MaxSegments)+"PatientID", nil); !errors.Is(err, ErrLimit) {
		t.Errorf("deep path: %v", err)
	}
}

func TestInsertQueryValues(t *testing.T) {
	dataset := object.New(std.Dictionary)
	path := MustParse("StudyDate")
	const dateRange = "20260101-20261231"
	if _, err := Insert(dataset, path, dateRange, Options{}); !errors.Is(err, ErrValue) {
		t.Fatalf("strict date range: %v", err)
	}
	options := Options{SkipValueValidation: true}
	if _, err := Insert(dataset, path, dateRange, options); err != nil {
		t.Fatal(err)
	}
	if got, _ := dataset.GetString(core.NewTag(0x0008, 0x0020)); got != dateRange {
		t.Fatalf("date range = %q", got)
	}
	if _, err := Insert(dataset, MustParse("Rows"), "512", options); err != nil {
		t.Fatal(err)
	}
	if element, _ := dataset.Get(core.NewTag(0x0028, 0x0010)); element.VR() != core.VRUS || element.Value.(core.Uint16Value)[0] != 512 {
		t.Fatalf("Rows = %+v", element)
	}
	if _, err := Insert(dataset, MustParse("Rows"), "70000", options); !errors.Is(err, ErrValue) {
		t.Fatalf("invalid US accepted: %v", err)
	}
}

func TestInsertModifyRemove(t *testing.T) {
	dataset := object.New(std.Dictionary)
	uid := MustParse("ReferencedSeriesSequence[1].SeriesInstanceUID")
	if n, err := Insert(dataset, uid, "1.2.3.4", Options{}); err != nil || n != 1 {
		t.Fatalf("insert: %d, %v", n, err)
	}
	series := items(t, dataset, tagReferencedSeries)
	if len(series) != 2 || series[0].Has(tagSeriesUID) {
		t.Fatalf("items %d", len(series))
	}
	if got, _ := series[1].GetString(tagSeriesUID); got != "1.2.3.4" {
		t.Fatalf("uid %q", got)
	}
	if _, err := Insert(dataset, MustParse("ReferencedSeriesSequence[0].SeriesInstanceUID"), "1.2.5", Options{}); err != nil {
		t.Fatal(err)
	}
	// Modify touches only existing attributes, across a wildcard.
	if n, err := Modify(dataset, MustParse("(0008,1115)[*].(0020,000E)"), "1.9", Options{}); err != nil || n != 2 {
		t.Fatalf("modify: %d, %v", n, err)
	}
	if _, err := Modify(dataset, MustParse("PatientID"), "X", Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("modify missing: %v", err)
	}
	// Values are converted and validated per VR.
	if _, err := Insert(dataset, MustParse("SliceThickness"), "abc", Options{}); !errors.Is(err, ErrValue) {
		t.Fatalf("invalid DS: %v", err)
	}
	if _, err := Insert(dataset, MustParse("Rows"), "70000", Options{}); !errors.Is(err, ErrValue) {
		t.Fatalf("invalid US: %v", err)
	}
	if _, err := Insert(dataset, MustParse("Rows"), "512", Options{}); err != nil {
		t.Fatal(err)
	}
	if element, _ := dataset.Get(core.NewTag(0x0028, 0x0010)); element.Header.VR != core.VRUS || element.Value.(core.Uint16Value)[0] != 512 {
		t.Fatalf("Rows = %+v", element)
	}
	if _, err := Insert(dataset, MustParse("ImageType"), `ORIGINAL\PRIMARY`, Options{}); err != nil {
		t.Fatal(err)
	}
	if values, _ := dataset.GetStrings(core.NewTag(0x0008, 0x0008)); len(values) != 2 {
		t.Fatalf("ImageType %v", values)
	}
	if _, err := Insert(dataset, MustParse("ReferencedSeriesSequence[0]"), "x", Options{}); !errors.Is(err, ErrValue) {
		t.Fatalf("value on item: %v", err)
	}
	if _, err := Insert(dataset, MustParse("(0011,1010)"), "x", Options{}); !errors.Is(err, ErrUnknownVR) {
		t.Fatalf("unknown VR: %v", err)
	}
	if _, err := Insert(dataset, MustParse("(0011,1010)"), "x", Options{VR: core.VRLO}); err != nil {
		t.Fatal(err)
	}
	// Removal of an item and of attributes.
	if n, err := Remove(dataset, MustParse("ReferencedSeriesSequence[0]")); err != nil || n != 1 {
		t.Fatalf("remove item: %d, %v", n, err)
	}
	if series := items(t, dataset, tagReferencedSeries); len(series) != 1 {
		t.Fatalf("%d items after removal", len(series))
	}
	if n, err := Remove(dataset, MustParse("ReferencedSeriesSequence[*].SeriesInstanceUID")); err != nil || n != 1 {
		t.Fatalf("remove attribute: %d, %v", n, err)
	}
	if _, err := Remove(dataset, MustParse("PatientName")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove missing: %v", err)
	}
	if _, err := Insert(dataset, MustParse("ContentSequence[5000]"), "", Options{}); !errors.Is(err, ErrLimit) {
		t.Fatalf("item limit: %v", err)
	}
}

func TestPrivateCreatorPaths(t *testing.T) {
	catalog, err := curated.New()
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Dictionary: dictionary.Chain{catalog, std.Dictionary}}
	dataset := object.New(std.Dictionary)
	p := MustParse(`(0029,"SIEMENS CSA HEADER",08)`)
	if _, err := Find(dataset, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find before insert: %v", err)
	}
	if _, err := Insert(dataset, p, "IMAGE NUM 4", opts); err != nil {
		t.Fatal(err)
	}
	creator, _ := dataset.GetString(core.NewTag(0x0029, 0x0010))
	element, ok := dataset.Get(core.NewTag(0x0029, 0x1008))
	if creator != "SIEMENS CSA HEADER" || !ok || element.Header.VR != core.VRCS {
		t.Fatalf("creator %q, element %+v", creator, element)
	}
	// A second creator in the same group takes the next block.
	if _, err := Insert(dataset, MustParse(`(0029,"OTHER CREATOR",01)`), "x", Options{VR: core.VRLO}); err != nil {
		t.Fatal(err)
	}
	if !dataset.Has(core.NewTag(0x0029, 0x1101)) {
		t.Fatal("second creator block not used")
	}
	matches, err := Find(dataset, p)
	if err != nil || len(matches) != 1 || matches[0].Element.Header.Tag != core.NewTag(0x0029, 0x1008) {
		t.Fatalf("find: %+v, %v", matches, err)
	}
}

func TestElementFromString(t *testing.T) {
	cases := []struct {
		vr   core.VR
		text string
		ok   bool
	}{
		{core.VRFD, `1.5\-2`, true}, {core.VRFL, "x", false}, {core.VRSS, "-32768", true}, {core.VRSS, "40000", false},
		{core.VRUL, "4294967295", true}, {core.VRSL, "-1", true}, {core.VRAT, `(0010,0010)\00200013`, true}, {core.VRAT, "0010", false},
		{core.VRUV, "18446744073709551615", true}, {core.VRSV, "-9", true}, {core.VROB, "", true}, {core.VROB, "01", false},
		{core.VRSQ, "", true}, {core.VRSQ, "x", false}, {core.VRLT, `a\b`, true},
	}
	for _, c := range cases {
		element, err := ElementFromString(core.NewTag(0x0011, 0x1010), c.vr, c.text, nil)
		if (err == nil) != c.ok {
			t.Errorf("%s %q: %v", c.vr, c.text, err)
		}
		if c.vr == core.VRLT && err == nil && len(element.Value.(core.StringValue)) != 1 {
			t.Errorf("LT was split")
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{"PatientID", "(0040,A730)[5].ContentSequence[3].ConceptNameCodeSequence[0].CodeValue", `(0029,"SIEMENS CSA HEADER",08)`, "(0008,1115)[*].(0020,000E)"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		p, err := Parse(text, nil)
		if err != nil {
			return
		}
		again, err := Parse(p.String(), nil)
		if err != nil || again.String() != p.String() {
			t.Fatalf("%q -> %q does not round-trip: %v", text, p.String(), err)
		}
		// Creating any parsed path stays within the item bound.
		dataset := object.New(std.Dictionary)
		_, _ = Create(dataset, p, Options{MaxCreatedItems: 64, VR: core.VRLO})
	})
}

func TestScript(t *testing.T) {
	text := "# pseudonymise\r\n" +
		"insert ReferencedSeriesSequence[0].SeriesInstanceUID=1.2.3\n" +
		"modify-if-present PatientComments=none\n" +
		"insert PatientName=ANON^A=B\n" +
		"  create ContentSequence[1]\n" +
		"remove-if-present OtherPatientIDsSequence\n" +
		"remove ContentSequence[0]\n"
	script, err := ParseScript(text, nil)
	if err != nil || len(script) != 6 {
		t.Fatalf("parse: %d edits, %v", len(script), err)
	}
	again, err := ParseScript(script.String(), nil)
	if err != nil || again.String() != script.String() {
		t.Fatalf("script does not round-trip: %v\n%s", err, script)
	}
	dataset := object.New(std.Dictionary)
	n, err := script.Apply(dataset, Options{})
	if err != nil || n != 4 {
		t.Fatalf("apply: %d, %v", n, err)
	}
	if name, _ := dataset.GetString(tagPatientName); name != "ANON^A=B" {
		t.Fatalf("name %q", name)
	}
	if content := items(t, dataset, tagContentSequence); len(content) != 1 {
		t.Fatalf("%d content items", len(content))
	}
	for _, bad := range []string{"delete PatientID", "remove PatientID=1", "modify PatientID", "insert Nope=1"} {
		if _, err := ParseScript(bad, nil); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := (Script{{Op: OpModify, Path: MustParse("PatientID"), Value: "x"}}).Apply(dataset, Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("modify missing: %v", err)
	}
}
