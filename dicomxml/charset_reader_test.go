package dicomxml

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// TestUnmarshalDeclaredLatin1Document reads a document declared
// ISO-8859-1, as DCMTK dcm2xml --native-format writes.
func TestUnmarshalDeclaredLatin1Document(t *testing.T) {
	document := []byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n" +
		`<NativeDicomModel xml:space="preserve" xmlns="http://dicom.nema.org/PS3.19/models/NativeDICOM">` +
		`<DicomAttribute tag="00081030" vr="LO" keyword="StudyDescription"><Value number="1">Cl` + "\xed" + `nica</Value></DicomAttribute>` +
		`</NativeDicomModel>`)
	obj, err := Unmarshal(document, std.Dictionary)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := obj.GetString(core.NewTag(0x0008, 0x1030)); got != "Clínica" {
		t.Fatalf("StudyDescription %q", got)
	}
	unknown := []byte(`<?xml version="1.0" encoding="X-NOPE"?><NativeDicomModel xmlns="http://dicom.nema.org/PS3.19/models/NativeDICOM"/>`)
	if _, err := Unmarshal(unknown, std.Dictionary); err == nil {
		t.Fatal("unknown XML encoding accepted")
	}
}
