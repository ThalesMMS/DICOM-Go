package object

import (
	"bytes"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestFileMetadataReturnsCommonPart10Fields(t *testing.T) {
	data, err := dicomtest.Part10File(
		transfer.ExplicitVRLittleEndian,
		append(dicomtest.MinimalDataset(),
			dicomtest.NewStringElement(core.NewTag(0x0008, 0x0060), core.VRCS, "CT"),
			dicomtest.NewStringElement(core.NewTag(0x0008, 0x0050), core.VRSH, "ACC-1"),
			dicomtest.NewStringElement(core.NewTag(0x0020, 0x0013), core.VRIS, "7"),
			dicomtest.NewStringElement(core.NewTag(0x0008, 0x1030), core.VRLO, "Study"),
			dicomtest.NewStringElement(core.NewTag(0x0018, 0x1030), core.VRLO, "  Chest"),
			dicomtest.NewStringElement(tagIssuerOfPatientID, core.VRLO, "SITE"),
			issuerQualifiers("2.25.1661", "ISO"),
		)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	file, err := ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	defer file.Close()
	got := file.Metadata()
	if got.TransferSyntaxUID != transfer.ExplicitVRLittleEndian.UID {
		t.Fatalf("TransferSyntaxUID = %q, want %q", got.TransferSyntaxUID, transfer.ExplicitVRLittleEndian.UID)
	}
	if got.TransferSyntaxName != transfer.ExplicitVRLittleEndian.Name {
		t.Fatalf("TransferSyntaxName = %q, want %q", got.TransferSyntaxName, transfer.ExplicitVRLittleEndian.Name)
	}
	if got.PatientName != "TEST^PATIENT" || got.PatientID != "TESTID001" {
		t.Fatalf("patient metadata = name %q id %q", got.PatientName, got.PatientID)
	}
	if got.SOPClassUID != dicomtest.TestSOPClassUID || got.SOPInstanceUID != dicomtest.TestSOPInstanceUID {
		t.Fatalf("SOP metadata = class %q instance %q", got.SOPClassUID, got.SOPInstanceUID)
	}
	if got.ProtocolName != "  Chest" || got.IssuerOfPatientID != (Issuer{"SITE", "2.25.1661", "ISO"}) {
		t.Fatalf("protocol and issuer = %q / %+v", got.ProtocolName, got.IssuerOfPatientID)
	}
	if got.Modality != "CT" || got.AccessionNumber != "ACC-1" || got.InstanceNumber != "7" || got.StudyDescription != "Study" {
		t.Fatalf("selected metadata = %#v", got)
	}
}

func TestFileMetadataNilFileReturnsZeroValue(t *testing.T) {
	var file *File
	if got := file.Metadata(); got != (FileMetadata{}) {
		t.Fatalf("nil Metadata() = %#v, want zero value", got)
	}
}

func TestFileMetadataPreservesDecodedUIDTextAndPrefersResolvedSyntax(t *testing.T) {
	file := &File{
		Meta: FromElements([]core.Element{
			dicomtest.NewStringElement(tagTransferSyntaxUID, core.VRUI, transfer.ImplicitVRLittleEndian.UID),
		}, std.Dictionary),
		Dataset: FromElements([]core.Element{
			core.NewRawElement(core.NewTag(0x0020, 0x000D), core.VRUI, []byte("1.2.93\x00")),
			dicomtest.NewStringElement(core.NewTag(0x0020, 0x000E), core.VRLO, "1.2.93.1 "),
			dicomtest.NewStringElement(tagIssuerOfPatientID, core.VRLO, "NO-PATIENT-ID"),
		}, std.Dictionary),
		TransferSyntax: transfer.ExplicitVRLittleEndian,
	}
	got := file.Metadata()
	if got.TransferSyntaxUID != transfer.ExplicitVRLittleEndian.UID || got.TransferSyntaxName != transfer.ExplicitVRLittleEndian.Name {
		t.Fatalf("resolved syntax = %q / %q", got.TransferSyntaxUID, got.TransferSyntaxName)
	}
	if got.StudyInstanceUID != "1.2.93" || got.SeriesInstanceUID != "1.2.93.1" {
		t.Fatalf("projected UIDs = %q / %q", got.StudyInstanceUID, got.SeriesInstanceUID)
	}
	if got.PatientID != "" || !got.IssuerOfPatientID.IsZero() {
		t.Fatalf("patient metadata without ID = %q / %+v", got.PatientID, got.IssuerOfPatientID)
	}

	file.TransferSyntax = transfer.Syntax{}
	got = file.Metadata()
	if got.TransferSyntaxUID != transfer.ImplicitVRLittleEndian.UID || got.TransferSyntaxName != transfer.ImplicitVRLittleEndian.Name {
		t.Fatalf("file meta syntax fallback = %q / %q", got.TransferSyntaxUID, got.TransferSyntaxName)
	}
}
