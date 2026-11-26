package seg

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// dcmtkRequiredAttributes are the Type 1 and 2 attributes DCMTK's
// DcmSegmentation::loadFile reports as absent when a Segmentation lacks the
// Patient, General Study, Frame of Reference, Segmentation Series, Enhanced
// General Equipment, General Image, Content Identification and Segmentation
// Image attributes (PS3.3 A.51).
var dcmtkRequiredAttributes = []struct {
	name  string
	tag   core.Tag
	type1 bool
}{
	{"PatientName", core.NewTag(0x0010, 0x0010), false},
	{"PatientID", core.NewTag(0x0010, 0x0020), false},
	{"PatientBirthDate", core.NewTag(0x0010, 0x0030), false},
	{"PatientSex", core.NewTag(0x0010, 0x0040), false},
	{"StudyDate", core.NewTag(0x0008, 0x0020), false},
	{"StudyTime", core.NewTag(0x0008, 0x0030), false},
	{"ReferringPhysicianName", core.NewTag(0x0008, 0x0090), false},
	{"StudyID", core.NewTag(0x0020, 0x0010), false},
	{"AccessionNumber", core.NewTag(0x0008, 0x0050), false},
	{"PositionReferenceIndicator", core.NewTag(0x0020, 0x1040), false},
	{"SeriesNumber", core.NewTag(0x0020, 0x0011), true},
	{"Manufacturer", core.NewTag(0x0008, 0x0070), true},
	{"ManufacturerModelName", core.NewTag(0x0008, 0x1090), true},
	{"DeviceSerialNumber", core.NewTag(0x0018, 0x1000), true},
	{"SoftwareVersions", core.NewTag(0x0018, 0x1020), true},
	{"ContentDate", core.NewTag(0x0008, 0x0023), true},
	{"ContentTime", core.NewTag(0x0008, 0x0033), true},
	{"InstanceNumber", core.NewTag(0x0020, 0x0013), true},
	{"ContentLabel", core.NewTag(0x0070, 0x0080), true},
	{"ContentDescription", core.NewTag(0x0070, 0x0081), false},
	{"ContentCreatorName", core.NewTag(0x0070, 0x0084), false},
	{"LossyImageCompression", core.NewTag(0x0028, 0x2110), true},
}

// encodedHeader writes the dataset of file (without Pixel Data) and reads
// it back, so the checks see what a reader of the Part 10 file sees.
func encodedHeader(t *testing.T, file *object.File) *object.Object {
	t.Helper()
	header := object.FromElements(file.Dataset.Elements(), nil)
	header.Remove(core.TagPixelData)
	var encoded bytes.Buffer
	if err := object.WriteDataSet(&encoded, header, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := object.ReadDataSet(&encoded, transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return back
}

func headerString(obj *object.Object, tag core.Tag) string {
	value, _ := obj.LookupString(tag)
	return strings.TrimRight(value, " \x00")
}

func TestWriteEmitsTypeOneAndTwoAttributesWithDefaults(t *testing.T) {
	file, err := Write(overlapDocument())
	if err != nil {
		t.Fatal(err)
	}
	header := encodedHeader(t, file)
	for _, a := range dcmtkRequiredAttributes {
		if !header.Has(a.tag) {
			t.Errorf("%s %s absent", a.name, a.tag)
			continue
		}
		if a.type1 && headerString(header, a.tag) == "" {
			t.Errorf("%s %s is Type 1 but empty", a.name, a.tag)
		}
	}
	for tag, want := range map[core.Tag]string{
		core.NewTag(0x0020, 0x0011): "1", core.NewTag(0x0020, 0x0013): "1",
		core.NewTag(0x0070, 0x0080): DefaultContentLabel, core.NewTag(0x0028, 0x2110): "00",
		core.NewTag(0x0008, 0x0070): "dicom-go",
		// No source: the Patient and Study Type 2 attributes are empty.
		core.NewTag(0x0010, 0x0010): "", core.NewTag(0x0010, 0x0020): "", core.NewTag(0x0008, 0x0050): "",
	} {
		if got := headerString(header, tag); got != want {
			t.Errorf("%s = %q, want %q", tag, got, want)
		}
	}
}

// segCommonSource is a synthetic source image (no PHI) of the patient and
// study a segmentation is filed under.
func segCommonSource() *object.Object {
	text := func(g, e uint16, vr core.VR, value string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(g, e), VR: vr}, Value: core.StringValue{value}}
	}
	return object.FromElements([]core.Element{
		text(0x0008, 0x0020, core.VRDA, "20260102"),
		text(0x0008, 0x0030, core.VRTM, "093000"),
		text(0x0008, 0x0050, core.VRSH, "ACC-SEG-1"),
		text(0x0008, 0x0070, core.VRLO, "SCANNER VENDOR"),
		text(0x0008, 0x0090, core.VRPN, "Referrer^Synthetic"),
		text(0x0010, 0x0010, core.VRPN, "Phantom^Segmentation"),
		text(0x0010, 0x0020, core.VRLO, "SYN-SEG-1"),
		text(0x0010, 0x0030, core.VRDA, "19700101"),
		text(0x0010, 0x0040, core.VRCS, "O"),
		text(0x0020, 0x000d, core.VRUI, "1.2.826.0.1.3680043.10.543.1000.2"),
		text(0x0020, 0x0010, core.VRSH, "STUDY-7"),
		text(0x0020, 0x0052, core.VRUI, "1.2.826.0.1.3680043.10.543.1000.4"),
		text(0x0020, 0x1040, core.VRLO, "XY"),
	}, nil)
}

func TestWriteEmitsCallerPatientStudyEquipmentAndIdentification(t *testing.T) {
	doc := overlapDocument()
	doc.Common = object.DerivedCommon{
		Source:    segCommonSource(),
		Equipment: &object.Equipment{Manufacturer: "Maker", ModelName: "Model", DeviceSerialNumber: "SN-1", SoftwareVersions: "2.0"},
	}
	doc.Identification = Identification{
		SeriesNumber: 301, InstanceNumber: 4, ContentLabel: "LIVER_SEG",
		ContentDescription: "Liver segments", ContentCreatorName: "Reader^Synthetic",
	}
	doc.ContentDate, doc.ContentTime = "20260103", "101500"
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	header := encodedHeader(t, file)
	want := map[string]string{
		"PatientName": "Phantom^Segmentation", "PatientID": "SYN-SEG-1", "PatientBirthDate": "19700101", "PatientSex": "O",
		"StudyDate": "20260102", "StudyTime": "093000", "ReferringPhysicianName": "Referrer^Synthetic", "StudyID": "STUDY-7",
		"AccessionNumber": "ACC-SEG-1", "PositionReferenceIndicator": "XY", "SeriesNumber": "301",
		// The equipment is the creating software's, never the scanner's.
		"Manufacturer": "Maker", "ManufacturerModelName": "Model", "DeviceSerialNumber": "SN-1", "SoftwareVersions": "2.0",
		"ContentDate": "20260103", "ContentTime": "101500", "InstanceNumber": "4", "ContentLabel": "LIVER_SEG",
		"ContentDescription": "Liver segments", "ContentCreatorName": "Reader^Synthetic", "LossyImageCompression": "00",
	}
	for _, a := range dcmtkRequiredAttributes {
		if got := headerString(header, a.tag); got != want[a.name] {
			t.Errorf("%s = %q, want %q", a.name, got, want[a.name])
		}
	}
	// The writer's own UIDs win over the source's.
	if got := headerString(header, core.NewTag(0x0020, 0x000e)); got != doc.SeriesInstanceUID {
		t.Errorf("Series Instance UID %q, want %q", got, doc.SeriesInstanceUID)
	}

	read, err := Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if read.Identification != doc.Identification || read.ContentDate != doc.ContentDate || read.ContentTime != doc.ContentTime {
		t.Fatalf("read identification %+v %s %s, want %+v", read.Identification, read.ContentDate, read.ContentTime, doc.Identification)
	}
}

func TestWriteRejectsInvalidContentLabel(t *testing.T) {
	doc := overlapDocument()
	doc.Identification.ContentLabel = "lower case"
	if _, err := Write(doc); err == nil {
		t.Fatal("Write accepted a Content Label that is not a CS value")
	}
}
