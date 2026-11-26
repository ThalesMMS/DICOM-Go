package object

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestEncodeFileMetaHeaderPrefixesARawDataSet: the header followed by a data
// set encoded in the syntax is a Part 10 file whose File Meta Information
// names the UIDs, the syntax, this implementation and the AE titles.
func TestEncodeFileMetaHeaderPrefixesARawDataSet(t *testing.T) {
	header, err := EncodeFileMetaHeader(dicomtest.TestSOPClassUID, dicomtest.TestSOPInstanceUID+"\x00", transfer.ExplicitVRLittleEndian,
		FileMetaAETitles{Source: "ARCHIVE", Sending: "MODALITY", Receiving: "ARCHIVE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(header) < 132 || !bytes.Equal(header[:128], make([]byte, 128)) || string(header[128:132]) != "DICM" {
		t.Fatal("header does not start with a zero preamble and DICM")
	}
	var dataSet bytes.Buffer
	dataSet.Write(dicomtest.ExplicitElement(dicomtest.NewUIElement(tagSOPClassUID, dicomtest.TestSOPClassUID)))
	dataSet.Write(dicomtest.ExplicitElement(dicomtest.NewUIElement(tagSOPInstanceUID, dicomtest.TestSOPInstanceUID)))
	file, err := ReadFile(bytes.NewReader(append(header, dataSet.Bytes()...)))
	if err != nil {
		t.Fatal(err)
	}
	for tag, want := range map[core.Tag]string{
		tagMediaStorageSOPClassUID:    dicomtest.TestSOPClassUID,
		tagMediaStorageSOPInstanceUID: dicomtest.TestSOPInstanceUID,
		tagTransferSyntaxUID:          transfer.ExplicitVRLittleEndian.UID,
		tagImplementationClassUID:     ImplementationClassUID,
		tagSourceAETitle:              "ARCHIVE",
		tagSendingAETitle:             "MODALITY",
		tagReceivingAETitle:           "ARCHIVE",
	} {
		if got := metaString(t, file, tag); got != want {
			t.Errorf("%v = %q, want %q", tag, got, want)
		}
	}
	if got, _ := file.Dataset.GetString(tagSOPInstanceUID); got != dicomtest.TestSOPInstanceUID {
		t.Fatalf("data set SOP Instance UID %q", got)
	}
}

func TestEncodeFileMetaHeaderRejectsInvalidIdentity(t *testing.T) {
	if _, err := EncodeFileMetaHeader("", dicomtest.TestSOPInstanceUID, transfer.ExplicitVRLittleEndian, FileMetaAETitles{}); !errors.Is(err, ErrMissingSOPClassUID) {
		t.Fatalf("empty SOP Class error %v", err)
	}
	if _, err := EncodeFileMetaHeader(dicomtest.TestSOPClassUID, "1..2", transfer.ExplicitVRLittleEndian, FileMetaAETitles{}); !errors.Is(err, ErrMissingSOPInstanceUID) {
		t.Fatalf("invalid SOP Instance error %v", err)
	}
	if _, err := EncodeFileMetaHeader(dicomtest.TestSOPClassUID, dicomtest.TestSOPInstanceUID, transfer.Syntax{}, FileMetaAETitles{}); err == nil {
		t.Fatal("empty transfer syntax accepted")
	}
}
