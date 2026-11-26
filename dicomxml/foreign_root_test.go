package dicomxml

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// The root element below is the one Orthanc's DICOMweb plugin 1.23 writes in
// QIDO-RS and WADO-RS metadata: no xml:space and an XML Schema instance hint.
const foreignRootDocument = `<?xml version="1.0" encoding="utf-8"?>
<NativeDicomModel xmlns="` + Namespace + `" xsi:schemaLocation="` + Namespace + `" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <DicomAttribute tag="00080050" vr="SH" keyword="AccessionNumber">
    <Value number="1">ACC0002</Value>
  </DicomAttribute>
</NativeDicomModel>`

func TestForeignRootAttributesNeedTheOption(t *testing.T) {
	if _, err := Unmarshal([]byte(foreignRootDocument), std.Dictionary); !errors.Is(err, ErrInvalidXML) {
		t.Fatalf("the strict default accepted a root without xml:space: %v", err)
	}
	options := DefaultUnmarshalOptions()
	options.AllowForeignRootAttributes = true
	decoded, err := UnmarshalWithOptions([]byte(foreignRootDocument), std.Dictionary, options)
	if err != nil {
		t.Fatalf("decode with AllowForeignRootAttributes: %v", err)
	}
	if got, _ := decoded.GetString(core.NewTag(0x0008, 0x0050)); got != "ACC0002" {
		t.Fatalf("Accession Number = %q", got)
	}
	// The option does not admit attributes that are neither namespace
	// declarations nor XML Schema instance hints.
	unknown := `<NativeDicomModel xmlns="` + Namespace + `" version="2"></NativeDicomModel>`
	if _, err := UnmarshalWithOptions([]byte(unknown), std.Dictionary, options); !errors.Is(err, ErrInvalidXML) {
		t.Fatalf("an unknown root attribute was accepted: %v", err)
	}
	wrongSpace := `<NativeDicomModel xmlns="` + Namespace + `" xml:space="default"></NativeDicomModel>`
	if _, err := UnmarshalWithOptions([]byte(wrongSpace), std.Dictionary, options); !errors.Is(err, ErrInvalidXML) {
		t.Fatalf(`xml:space="default" was accepted: %v`, err)
	}
}
