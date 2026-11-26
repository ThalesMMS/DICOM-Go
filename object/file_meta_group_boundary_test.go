package object_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestFileMetaRejectsOverstatedGroupLengthIncludingDatasetAttribute(t *testing.T) {
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian,
		core.NewRawElement(tags.PatientID, core.VRLO, []byte("META-1")))
	if err != nil {
		t.Fatal(err)
	}
	// (0002,0000) is a four-byte UL in the first File Meta element.
	const groupLengthValueOffset = 132 + 8
	groupLength := data[groupLengthValueOffset : groupLengthValueOffset+4]
	datasetOffset := groupLengthValueOffset + 4 + int(binary.LittleEndian.Uint32(groupLength))
	binary.LittleEndian.PutUint32(groupLength, uint32(len(data)-groupLengthValueOffset-4))

	const patientIDHeaderBytes = 8
	source := &metadataTestSource{Reader: bytes.NewReader(data), forbidden: []metadataReadSpan{
		{int64(datasetOffset + patientIDHeaderBytes), int64(len(data))},
	}}
	if _, err := object.ReadPart10Header(source, object.ReadFileOptions{}); !errors.Is(err, object.ErrInvalidFileMetaGroupLength) {
		t.Errorf("ReadPart10Header: %v, want invalid File Meta group length", err)
	}
	if _, err := object.ReadFile(bytes.NewReader(data)); !errors.Is(err, object.ErrInvalidFileMetaGroupLength) {
		t.Errorf("ReadFile: %v, want invalid File Meta group length", err)
	}

	opts := object.ReadFileOptions{AllowMissingMetaElementGroupLength: true}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	header, err := object.ReadPart10Header(source, opts)
	if err != nil || header.DataSetOffset != int64(datasetOffset) || header.Meta.Has(tags.PatientID) {
		t.Fatalf("group scan recovered header %+v, err %v", header, err)
	}
	file, err := object.ReadFileWithOptions(bytes.NewReader(data), opts)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := file.Dataset.GetString(tags.PatientID); id != "META-1" || file.Meta.Has(tags.PatientID) {
		t.Fatalf("group scan recovered Patient ID %q", id)
	}
}
