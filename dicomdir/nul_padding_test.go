package dicomdir_test

import (
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/dicomdir"
)

// TestParseFileSetAcceptsNULPaddedRecordTypes pins #2032: a Directory Record
// Type padded with NUL instead of a space ("PATIENT\x00") is read as the
// unpadded record type, so the DICOMDIR loads with the same records.
func TestParseFileSetAcceptsNULPaddedRecordTypes(t *testing.T) {
	root, fileSet, _ := filesetSyntheticFileSet(t)
	data := filesetWriteBytes(t, fileSet)
	items, ok := filesetReadPart10(t, data).Dataset.GetSequence(filesetTagDirectorySequence)
	if !ok {
		t.Fatal("fixture has no directory records")
	}
	padded := map[string]int{}
	for _, item := range items {
		recordType, _ := item.GetString(filesetTagRecordType)
		offset, set := item.ItemOffset()
		if !set {
			t.Fatalf("%s record has no ItemOffset", recordType)
		}
		_, value := filesetElementValueRange(t, data, int(offset), filesetTagRecordType)
		if len(value) > 0 && value[len(value)-1] == ' ' {
			value[len(value)-1] = 0
			padded[recordType]++
		}
	}
	for _, recordType := range []string{"PATIENT", "STUDY", "IMAGE"} {
		if padded[recordType] == 0 {
			t.Fatalf("fixture has no space-padded %s record to rewrite (rewrote %v)", recordType, padded)
		}
	}

	path := filesetWriteDICOMDIRFixture(t, root, data)
	parsed, err := dicomdir.ParseFileSet(path)
	if err != nil {
		t.Fatalf("ParseFileSet(NUL-padded record types) error = %v", err)
	}
	want, got := fileSet.Records(), parsed.Records()
	if len(got) != len(want) {
		t.Fatalf("records = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].RecordType != want[i].RecordType || !reflect.DeepEqual(got[i].FileID, want[i].FileID) {
			t.Fatalf("record %d = %s %v, want %s %v", i, got[i].RecordType, got[i].FileID, want[i].RecordType, want[i].FileID)
		}
	}

	references := dicomdir.References(filesetReadPart10(t, data).Dataset)
	if len(references) != len(want) {
		t.Fatalf("References() = %d, want %d", len(references), len(want))
	}
	for _, reference := range references {
		if reference.Type != "IMAGE" {
			t.Fatalf("reference type = %q, want IMAGE", reference.Type)
		}
	}
}
