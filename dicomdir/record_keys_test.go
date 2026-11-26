package dicomdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var allFileRecordTypes = []RecordType{RecordTypeImage, RecordTypeRTDose, RecordTypeRTStructureSet, RecordTypeRTPlan, RecordTypeRTTreatmentRecord, RecordTypePresentation, RecordTypeWaveform, RecordTypeSRDocument, RecordTypeKeyObjectDocument, RecordTypeSpectroscopy, RecordTypeRawData, RecordTypeRegistration, RecordTypeFiducial, RecordTypeHangingProtocol, RecordTypeEncapsulatedDocument, RecordTypeValueMap, RecordTypeStereometric, RecordTypePalette, RecordTypeImplant, RecordTypeImplantAssembly, RecordTypeImplantGroup, RecordTypePlan, RecordTypeMeasurement, RecordTypeSurface, RecordTypeSurfaceScan, RecordTypeTract, RecordTypeAssessment, RecordTypeRadiotherapy, RecordTypeAnnotation, RecordTypeInventory, RecordTypeWaveformPresentation}

func testDirectorySOP(t *testing.T, typ RecordType) string {
	t.Helper()
	for _, entry := range uid.EntriesByType(uid.SOPClass) {
		if got, ok := recordTypeForSOPClass(entry.UID); ok && got == typ {
			return entry.UID
		}
	}
	t.Fatalf("no SOP mapping for %s", typ)
	return ""
}
func testDirectorySQ(tag core.Tag, items ...core.DataSet) core.Element {
	return directoryValueElement(tag, core.VRSQ, core.SequenceValue{Items: items})
}
func testDirectoryCode() core.DataSet {
	return core.DataSet{Elements: []core.Element{textElement(keyTag(0x0008, 0x0100), core.VRSH, "121071"), textElement(keyTag(0x0008, 0x0102), core.VRSH, "DCM"), textElement(keyTag(0x0008, 0x0104), core.VRLO, "Finding")}}
}
func testDirectoryReference() core.DataSet {
	return core.DataSet{Elements: []core.Element{uiElementValue(keyTag(0x0008, 0x1150), "1.2.840.10008.5.1.4.1.1.2"), uiElementValue(keyTag(0x0008, 0x1155), "1.2.826.0.1.3680043.10.999.2")}}
}
func testDirectorySeries(wave bool) core.DataSet {
	tag := keyTag(0x0008, 0x1140)
	if wave {
		tag = keyTag(0x0008, 0x113A)
	}
	return core.DataSet{Elements: []core.Element{uiElementValue(tagSeriesInstanceUIDFS, "1.2.826.0.1.3680043.10.999.3"), testDirectorySQ(tag, testDirectoryReference())}}
}
func testDirectorySource(t *testing.T, typ RecordType, serial int) *object.Object {
	t.Helper()
	sop := testDirectorySOP(t, typ)
	elements := []core.Element{
		uiElementValue(tagSOPClassUIDFS, sop), uiElementValue(tagSOPInstanceUIDFS, fmt.Sprintf("1.2.826.0.1.3680043.10.999.10.%d", serial)),
		textElement(tagPatientNameFS, core.VRPN, "SYNTHETIC^DIRECTORY"), textElement(tagPatientIDFS, core.VRLO, "SYNTHETIC"),
		uiElementValue(tagStudyInstanceUIDFS, "1.2.826.0.1.3680043.10.999.11"), uiElementValue(tagSeriesInstanceUIDFS, "1.2.826.0.1.3680043.10.999.12"),
		textElement(tagStudyDateFS, core.VRDA, "20261009"), textElement(tagStudyTimeFS, core.VRTM, "120000"), textElement(tagStudyIDFS, core.VRSH, "DIR"),
		textElement(tagAccessionNumberFS, core.VRSH, ""), textElement(tagModalityFS, core.VRCS, "OT"), textElement(tagSeriesNumberFS, core.VRIS, "1"), textElement(tagInstanceNumberFS, core.VRIS, "1"),
	}
	src := object.FromElements(elements, std.Dictionary)
	for _, rule := range directoryKeyRules(typ) {
		if rule.kind == "1C" || rule.kind == "2C" {
			continue
		}
		if src.Has(rule.tag) {
			continue
		}
		entry, ok := std.Dictionary.ByTag(rule.tag)
		if !ok {
			t.Fatalf("unknown key %s", rule.tag)
		}
		var e core.Element
		switch entry.VR {
		case core.VRSQ:
			e = testDirectorySQ(rule.tag)
		case core.VRUS:
			e = usElement(rule.tag, 1)
		case core.VRUL:
			e = ulElement(rule.tag, 1)
		case core.VRUV:
			e = core.NewRawElement(rule.tag, core.VRUV, []byte{1, 0, 0, 0, 0, 0, 0, 0})
		default:
			value := "VALUE"
			switch entry.VR {
			case core.VRDA:
				value = "20261009"
			case core.VRTM:
				value = "120000"
			case core.VRDT:
				value = "20261009120000"
			case core.VRIS, core.VRDS:
				value = "1"
			}
			if rule.kind == "2" {
				value = ""
			}
			e = textElement(rule.tag, entry.VR, value)
		}
		src.Put(e)
	}
	switch typ {
	case RecordTypeSRDocument:
		src.Put(textElement(keyTag(0x0040, 0xA491), core.VRCS, "COMPLETE"))
		src.Put(textElement(keyTag(0x0040, 0xA493), core.VRCS, "UNVERIFIED"))
		src.Put(testDirectorySQ(keyTag(0x0040, 0xA043), testDirectoryCode()))
	case RecordTypeKeyObjectDocument:
		src.Put(testDirectorySQ(keyTag(0x0040, 0xA043), testDirectoryCode()))
	case RecordTypePresentation:
		if presentationSeriesRequired(sop) {
			src.Put(testDirectorySQ(keyTag(0x0008, 0x1115), testDirectorySeries(false)))
		}
	case RecordTypeWaveformPresentation:
		src.Put(testDirectorySQ(keyTag(0x0008, 0x1115), testDirectorySeries(true)))
	case RecordTypeSpectroscopy:
		src.Put(textValuesElement(keyTag(0x0008, 0x0008), core.VRCS, []string{"ORIGINAL", "PRIMARY", "OTHER", "MIXED"}))
	case RecordTypeHangingProtocol:
		src.Put(textElement(keyTag(0x0072, 0x0006), core.VRCS, "USER"))
		src.Put(testDirectorySQ(keyTag(0x0072, 0x000C), core.DataSet{Elements: []core.Element{textElement(tagModalityFS, core.VRCS, "CT")}}))
	case RecordTypeImplantAssembly:
		src.Put(testDirectorySQ(keyTag(0x0076, 0x0020), testDirectoryCode()))
	case RecordTypeInventory:
		src.Put(textElement(keyTag(0x0008, 0x0403), core.VRCS, "STUDY"))
		src.Put(textElement(keyTag(0x0008, 0x0426), core.VRCS, "COMPLETE"))
	case RecordTypeEncapsulatedDocument:
		src.Put(textElement(keyTag(0x0042, 0x0012), core.VRLO, "application/pdf"))
		if sop == "1.2.840.10008.5.1.4.1.1.104.2" {
			src.Put(textElement(keyTag(0x0040, 0xE001), core.VRST, "synthetic-cda-id"))
		}
	}
	return src
}
func testDirectoryWriteSource(t *testing.T, root string, id FileID, src *object.Object, syntax transfer.Syntax) {
	t.Helper()
	path := filepath.Join(append([]string{root}, []string(id)...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	handle, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Callers author the raw numeric values in the syntax's byte order.
	src.SetValueByteOrder(syntax.ByteOrder)
	err = object.WriteFile(handle, &object.File{Dataset: src, TransferSyntax: syntax})
	closeErr := handle.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}
func testDirectorySet(t *testing.T, root string, options Options) *FileSet {
	t.Helper()
	options.FileSetUID = "1.2.826.0.1.3680043.10.999.100"
	options.FileSetID = "SYNTHETIC"
	set, err := NewFileSet(root, options)
	if err != nil {
		t.Fatal(err)
	}
	return set
}
func TestDirectoryKeysAllStandardFileTypes(t *testing.T) {
	if len(allFileRecordTypes) != 31 {
		t.Fatal("matrix size")
	}
	for i, typ := range allFileRecordTypes {
		t.Run(typ.String(), func(t *testing.T) {
			root := t.TempDir()
			src := testDirectorySource(t, typ, i+1)
			testDirectoryWriteSource(t, root, FileID{"FILE0001"}, src, transfer.ExplicitVRLittleEndian)
			set := testDirectorySet(t, root, Options{})
			if err := set.Add(context.Background(), FileID{"FILE0001"}); err != nil {
				t.Fatal(err)
			}
			r := set.Records()[0]
			if r.RecordType != typ {
				t.Fatalf("type %s", r.RecordType)
			}
			if rootFileRecordType(typ) && (r.PatientID != "" || r.StudyInstanceUID != "" || r.SeriesInstanceUID != "") {
				t.Fatal("invented patient hierarchy for root object")
			}
			var out bytes.Buffer
			report, err := WriteDICOMDIR(context.Background(), &out, set, WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := 4
			if rootFileRecordType(typ) {
				want = 1
			}
			if report.Records != want {
				t.Fatalf("records %d want %d", report.Records, want)
			}
			if err := os.WriteFile(filepath.Join(root, "DICOMDIR"), out.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseFileSet(filepath.Join(root, "DICOMDIR"))
			if err != nil {
				t.Fatal(err)
			}
			if !directoryKeysEqual(r.keys, parsed.Records()[0].keys) {
				t.Fatal("keys changed after round-trip")
			}
		})
	}
}
func TestDirectoryKeyRequiredFieldsAndOwnership(t *testing.T) {
	for _, typ := range allFileRecordTypes {
		src := testDirectorySource(t, typ, 1)
		r := FileRecord{RecordType: typ, SOPClassUID: testDirectorySOP(t, typ), InstanceNumber: "1"}
		limits := Limits{}
		if err := normalizeFileSetLimits(&limits); err != nil {
			t.Fatal(err)
		}
		keys, err := extractDirectoryKeys(src, r, &directoryKeyBudget{ctx: context.Background(), limits: limits})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		for _, rule := range directoryKeyRules(typ) {
			if rule.kind != "1" {
				continue
			}
			changed := object.FromDataSet(cloneDirectoryKeys(keys), std.Dictionary)
			changed.Remove(rule.tag)
			if err := validateDirectoryKeys(typ, core.DataSet{Elements: changed.Elements()}, r.SOPClassUID); err == nil {
				t.Errorf("%s accepted missing %s", typ, rule.tag)
			}
		}
		snap := FileRecord{keys: keys}
		owned := snap.SelectionKeys()
		if len(owned.Elements) > 0 {
			owned.Elements[0] = core.Element{}
			if snap.keys.Elements[0].Tag() == (core.Tag{}) {
				t.Fatal("keys alias caller")
			}
		}
	}
}
func TestDirectoryKeyAdmissionBeforeMaterialization(t *testing.T) {
	root := t.TempDir()
	src := testDirectorySource(t, RecordTypeSRDocument, 1)
	items := make([]core.DataSet, 128)
	for i := range items {
		items[i] = core.DataSet{Elements: []core.Element{textElement(keyTag(0x0040, 0xA010), core.VRCS, "CONTAINS"), textElement(keyTag(0x0040, 0xA040), core.VRCS, "TEXT"), textElement(keyTag(0x0040, 0xA160), core.VRUT, "synthetic bounded content")}}
	}
	src.Put(testDirectorySQ(keyTag(0x0040, 0xA730), items...))
	testDirectoryWriteSource(t, root, FileID{"SR000001"}, src, transfer.ExplicitVRLittleEndian)
	set := testDirectorySet(t, root, Options{Limits: Limits{MaxRecordKeyElements: 64}})
	if err := set.Add(context.Background(), FileID{"SR000001"}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("large requested sequence: %v", err)
	}
	if len(set.Records()) != 0 {
		t.Fatal("failed Add published partial record")
	}
}

func TestSpectroscopyDirectoryEvidencePresenceAndHierarchy(t *testing.T) {
	for _, kind := range []string{"absent", "valid", "empty", "missing-study", "missing-SOP"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			src := testDirectorySource(t, RecordTypeSpectroscopy, 1)
			src.Put(textValuesElement(keyTag(0x0008, 0x0008), core.VRCS, []string{"ORIGINAL", "PRIMARY", "SPECTROSCOPY", "NONE"}))
			if kind != "absent" {
				series := core.DataSet{Elements: []core.Element{uiElementValue(tagSeriesInstanceUIDFS, "2.25.13002"), testDirectorySQ(keyTag(0x0008, 0x1199), testDirectoryReference())}}
				study := core.DataSet{Elements: []core.Element{uiElementValue(tagStudyInstanceUIDFS, "2.25.13001"), testDirectorySQ(keyTag(0x0008, 0x1115), series)}}
				if kind == "missing-study" {
					study.Elements = study.Elements[1:]
				}
				if kind == "missing-SOP" {
					series.Elements = series.Elements[:1]
					study.Elements[1] = testDirectorySQ(keyTag(0x0008, 0x1115), series)
				}
				src.Put(testDirectorySQ(keyTag(0x0008, 0x9092), study))
				if kind == "empty" {
					src.Put(testDirectorySQ(keyTag(0x0008, 0x9092)))
				}
			}
			testDirectoryWriteSource(t, root, FileID{"SPECTRO1"}, src, transfer.ExplicitVRLittleEndian)
			set := testDirectorySet(t, root, Options{})
			err := set.Add(context.Background(), FileID{"SPECTRO1"})
			valid := kind == "absent" || kind == "valid"
			if (err == nil) != valid {
				t.Fatalf("%s: %v", kind, err)
			}
			if valid {
				var out bytes.Buffer
				if _, err := WriteDICOMDIR(context.Background(), &out, set, WriteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDirectoryVerificationTimezoneAndPartialPrecision(t *testing.T) {
	for _, tc := range []struct {
		name, zone string
		values     []string
		want       string
		refuse     bool
	}{
		{name: "inherited offset", zone: "+0300", values: []string{"20261009120000", "20261009103000+0000"}, want: "20261009103000+0000"},
		{name: "retained truncated offset", zone: "-0300", values: []string{"20261009", "20261010"}, want: "20261010-0300"},
		{name: "ambiguous precision", zone: "+0000", values: []string{"20261009", "20261009120000"}, refuse: true},
		{name: "mixed unknown offset", values: []string{"20261009100000", "20261009103000+0000"}, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := object.New(nil)
			if tc.zone != "" {
				src.Put(textElement(keyTag(0x0008, 0x0201), core.VRSH, tc.zone))
			}
			var entries []core.DataSet
			for _, value := range tc.values {
				entries = append(entries, core.DataSet{Elements: []core.Element{textElement(keyTag(0x0040, 0xA030), core.VRDT, value)}})
			}
			src.Put(testDirectorySQ(keyTag(0x0040, 0xA073), entries...))
			limits := Limits{}
			_ = normalizeFileSetLimits(&limits)
			e, err := verificationDirectoryKey(src, &directoryKeyBudget{ctx: context.Background(), limits: limits})
			if tc.refuse {
				if err == nil {
					t.Fatal("ambiguous observer accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := object.FromElements([]core.Element{e}, nil).GetString(e.Tag())
			if got != tc.want {
				t.Fatalf("time %q want %q", got, tc.want)
			}
		})
	}
}
func TestDirectoryCodeRepresentationAndInventoryMatching(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tag   core.Tag
		vr    core.VR
		value string
		valid bool
	}{
		{"short code", keyTag(0x0008, 0x0100), core.VRSH, "ABC", true},
		{"long code", keyTag(0x0008, 0x0119), core.VRUC, "ABCDEFGHIJKLMNOPQ", true},
		{"short in long field", keyTag(0x0008, 0x0119), core.VRUC, "ABC", false},
		{"url in long field", keyTag(0x0008, 0x0119), core.VRUC, "https://example.org/synthetic-long-code", false},
		{"url", keyTag(0x0008, 0x0120), core.VRUR, "https://example.org/synthetic-long-code", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := object.FromDataSet(testDirectoryCode(), nil)
			item.Remove(keyTag(0x0008, 0x0100))
			item.Put(textElement(tc.tag, tc.vr, tc.value))
			err := validateCodeDirectoryItem(item)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		tag   core.Tag
		els   []core.Element
		valid bool
	}{
		{"one UID", keyTag(0x0008, 0x0411), []core.Element{uiElementValue(tagStudyInstanceUIDFS, "1.2.3")}, true},
		{"multiple UIDs in one attribute", keyTag(0x0008, 0x0411), []core.Element{textValuesElement(keyTag(0x0008, 0x0058), core.VRUI, []string{"1.2.3", "1.2.4"})}, false},
		{"two UIDs", keyTag(0x0008, 0x0411), []core.Element{uiElementValue(tagStudyInstanceUIDFS, "1.2.3"), uiElementValue(tagSeriesInstanceUIDFS, "1.2.4")}, false},
		{"universal", keyTag(0x0008, 0x0413), []core.Element{textElement(tagPatientNameFS, core.VRPN, "*")}, false},
		{"selective wildcard", keyTag(0x0008, 0x0413), []core.Element{textElement(tagPatientNameFS, core.VRPN, "A*")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := object.FromElements([]core.Element{testDirectorySQ(keyTag(0x0008, 0x0400), core.DataSet{Elements: []core.Element{testDirectorySQ(tc.tag, core.DataSet{Elements: tc.els})}})}, nil)
			err := validateInventoryScope(src)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
func TestDirectoryWaveformAnnotationCondition(t *testing.T) {
	src := testDirectorySource(t, RecordTypeWaveformPresentation, 1)
	r := FileRecord{RecordType: RecordTypeWaveformPresentation, SOPClassUID: testDirectorySOP(t, RecordTypeWaveformPresentation), InstanceNumber: "1"}
	src.Put(testDirectorySQ(keyTag(0x0040, 0xB030), core.DataSet{}))
	limits := Limits{}
	_ = normalizeFileSetLimits(&limits)
	if _, err := extractDirectoryKeys(src, r, &directoryKeyBudget{ctx: context.Background(), limits: limits}); !errors.Is(err, ErrMissingRequiredKey) {
		t.Fatalf("annotation condition %v", err)
	}
	reference := testDirectoryReference()
	reference.Elements[0] = uiElementValue(keyTag(0x0008, 0x1150), "1.2.840.10008.5.1.4.1.1.88.77")
	src.Put(testDirectorySQ(keyTag(0x0008, 0x1115), core.DataSet{Elements: []core.Element{uiElementValue(tagSeriesInstanceUIDFS, "1.2.3"), testDirectorySQ(keyTag(0x0008, 0x114A), reference)}}))
	if _, err := extractDirectoryKeys(src, r, &directoryKeyBudget{ctx: context.Background(), limits: limits}); err != nil {
		t.Fatal(err)
	}
}
func TestDirectoryKeysCharsetBigEndianNestedOwnership(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			root := t.TempDir()
			src := testDirectorySource(t, RecordTypeSpectroscopy, 1)
			src.Put(textElement(tagSpecificCharacterSetFS, core.VRCS, "ISO_IR 100"))
			src.Put(textElement(tagPatientNameFS, core.VRPN, "SYNTHETIC^CAFÉ"))
			src.Put(object.USElement(keyTag(0x0028, 0x0010), syntax.ByteOrder, 256))
			src.Put(object.USElement(keyTag(0x0028, 0x0011), syntax.ByteOrder, 513))
			src.Put(object.ULElement(keyTag(0x0028, 0x9001), syntax.ByteOrder, 1))
			src.Put(object.ULElement(keyTag(0x0028, 0x9002), syntax.ByteOrder, 1))
			code := testDirectoryCode()
			code.Elements[2] = textElement(keyTag(0x0008, 0x0104), core.VRLO, "Café")
			ref := testDirectoryReference()
			ref.Elements = append(ref.Elements, testDirectorySQ(keyTag(0x0040, 0xA170), code))
			src.Put(testDirectorySQ(keyTag(0x0008, 0x9092), core.DataSet{Elements: []core.Element{uiElementValue(tagStudyInstanceUIDFS, "1.2.3"), testDirectorySQ(keyTag(0x0008, 0x1115), core.DataSet{Elements: []core.Element{uiElementValue(tagSeriesInstanceUIDFS, "1.2.4"), testDirectorySQ(keyTag(0x0008, 0x1199), ref)}})}}))
			src.Put(textElement(keyTag(0x0011, 0x0010), core.VRLO, "SYNTHETIC_PRIVATE"))
			src.Put(core.NewRawElement(keyTag(0x0011, 0x1010), core.VROB, make([]byte, 8192)))
			id := FileID{"SPECT001"}
			testDirectoryWriteSource(t, root, id, src, syntax)
			set := testDirectorySet(t, root, Options{})
			if err := set.Add(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			record := set.Records()[0]
			if record.PatientName != "SYNTHETIC^CAFÉ" {
				t.Fatalf("charset %q", record.PatientName)
			}
			keys := object.FromDataSet(record.SelectionKeys(), nil)
			rows, rowsOK := uint16Value(keys, keyTag(0x0028, 0x0010))
			cols, colsOK := uint16Value(keys, keyTag(0x0028, 0x0011))
			if !rowsOK || rows != 256 || !colsOK || cols != 513 {
				t.Fatalf("numeric %v %v", rows, cols)
			}
			evidence, _ := keys.GetSequence(keyTag(0x0008, 0x9092))
			series, _ := evidence[0].GetSequence(keyTag(0x0008, 0x1115))
			refs, _ := series[0].GetSequence(keyTag(0x0008, 0x1199))
			codes, _ := refs[0].GetSequence(keyTag(0x0040, 0xA170))
			meaning, _ := codes[0].GetString(keyTag(0x0008, 0x0104))
			if meaning != "Café" {
				t.Fatalf("nested charset %q", meaning)
			}
			owned := record.SelectionKeys()
			var mutate func(core.DataSet)
			mutate = func(ds core.DataSet) {
				for _, e := range ds.Elements {
					switch v := e.Value.(type) {
					case core.StringValue:
						for i := range v {
							if v[i] == "Café" {
								v[i] = "CHANGED"
							}
						}
					case core.SequenceValue:
						for _, item := range v.Items {
							mutate(item)
						}
					}
				}
			}
			mutate(owned)
			if directoryKeysEqual(owned, set.Records()[0].keys) {
				t.Fatal("ownership mutation failed")
			}
			var output bytes.Buffer
			if _, err := WriteDICOMDIR(context.Background(), &output, set, WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "DICOMDIR"), output.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ParseFileSet(filepath.Join(root, "DICOMDIR")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestDirectoryKeyBudgetsAndRaisedLimits(t *testing.T) {
	root := t.TempDir()
	src := testDirectorySource(t, RecordTypeSRDocument, 1)
	item := core.DataSet{Elements: []core.Element{textElement(keyTag(0x0040, 0xA010), core.VRCS, "HAS CONCEPT MOD"), textElement(keyTag(0x0040, 0xA040), core.VRCS, "TEXT"), testDirectorySQ(keyTag(0x0040, 0xA043), testDirectoryCode()), textElement(keyTag(0x0040, 0xA160), core.VRUT, strings.Repeat("x", (4<<20)+17))}}
	src.Put(testDirectorySQ(keyTag(0x0040, 0xA730), item))
	id := FileID{"SR000001"}
	testDirectoryWriteSource(t, root, id, src, transfer.ExplicitVRLittleEndian)
	for _, limits := range []Limits{{MaxRecordKeyBytes: 8 << 20, MaxTotalRecordKeyBytes: 1024}, {}} {
		set := testDirectorySet(t, root, Options{Limits: limits})
		if err := set.Add(context.Background(), id); !errors.Is(err, ErrResourceLimit) || len(set.Records()) != 0 {
			t.Fatalf("admission %v", err)
		}
	}
	options := Options{Limits: Limits{MaxRecordKeyBytes: 8 << 20, MaxTotalRecordKeyBytes: 128 << 20}}
	set := testDirectorySet(t, root, options)
	if err := set.Add(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := WriteDICOMDIR(context.Background(), &output, set, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "DICOMDIR"), output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFileSetWithOptions(context.Background(), filepath.Join(root, "DICOMDIR"), options); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryRecordCountPreflightAndDuplicateSelection(t *testing.T) {
	root := t.TempDir()
	id := FileID{"IMAGE001"}
	src := testDirectorySource(t, RecordTypeImage, 1)
	testDirectoryWriteSource(t, root, id, src, transfer.ExplicitVRLittleEndian)
	set := testDirectorySet(t, root, Options{Limits: Limits{MaxDirectoryRecords: 3}})
	if err := set.Add(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := WriteDICOMDIR(context.Background(), &output, set, WriteOptions{}); !errors.Is(err, ErrResourceLimit) || output.Len() != 0 {
		t.Fatalf("record admission %v", err)
	}
	// A duplicate selected attribute is refused instead of silently last-winning.
	src = testDirectorySource(t, RecordTypeSRDocument, 2)
	id = FileID{"SR000002"}
	testDirectoryWriteSource(t, root, id, src, transfer.ExplicitVRLittleEndian)
	file, err := os.OpenFile(filepath.Join(root, id[0]), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte{0x40, 0x00, 0x93, 0xA4, 'C', 'S', 10, 0}
	raw = append(raw, []byte("UNVERIFIED")...)
	_, err = file.Write(raw)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	set = testDirectorySet(t, root, Options{})
	if err := set.Add(context.Background(), id); err == nil || len(set.Records()) != 0 {
		t.Fatalf("duplicate selected key %v", err)
	}
}
