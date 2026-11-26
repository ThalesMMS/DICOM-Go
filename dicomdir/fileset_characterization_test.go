package dicomdir_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/dicomdir"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
)

func TestFileSetMixedCharacterSetsAndSeriesNumbers(t *testing.T) {
	tests := []struct {
		name, charset, patient, description, seriesNumber string
	}{
		{"default repertoire", "", "Local^Subject", "Chest PA", "-2147483648"},
		{"Latin-1", "ISO_IR 100", "Müller^Käthe", "Rückenübersicht", "0"},
		{"Cyrillic", "ISO_IR 144", "Смирнов^Илья", "Грудная клетка", "-7"},
		{"Japanese code extensions", `ISO 2022 IR 6\ISO 2022 IR 87`, "Test^A=佐藤^花子=さとう^はなこ", "腹部画像", "12"},
		{"UTF-8", "ISO_IR 192", "顧^美=Hồ^An", "腹部及び骨盤", "2147483647"},
	}
	ctx := context.Background()
	root := t.TempDir()
	set, err := dicomdir.NewFileSet(root, dicomdir.Options{FileSetUID: filesetFixedUID})
	if err != nil {
		t.Fatal(err)
	}
	specs := make([]filesetInstanceSpec, len(tests))
	for i, tc := range tests {
		baseUID := fmt.Sprintf("2.25.777.604.%d", i+1)
		spec := filesetInstanceSpec{
			fileID: dicomdir.FileID{"IMAGES", fmt.Sprintf("IMG%05d", i+1)}, specificCharacterSet: tc.charset,
			patientID: fmt.Sprintf("SYNTH-%d", i+1), patientName: tc.patient,
			studyUID: baseUID, studyID: "SYNTH", studyDate: "20261201", studyTime: "101112", studyDescription: tc.description,
			seriesUID: baseUID + ".1", seriesNo: tc.seriesNumber, modality: "CT", sopUID: baseUID + ".1.1", instanceNo: "1",
		}
		filesetWriteInstance(t, root, spec)
		if err := set.Add(ctx, spec.fileID); err != nil {
			t.Fatalf("Add %s: %v", tc.name, err)
		}
		specs[i] = spec
	}
	if _, err := dicomdir.CommitDICOMDIR(ctx, set, dicomdir.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "DICOMDIR")
	parsed, err := dicomdir.ParseFileSet(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(parsed.Records()); got != len(tests) {
		t.Fatalf("parsed %d records, want %d", got, len(tests))
	}
	items, ok := filesetReadPart10(t, filesetReadBytes(t, directory)).Dataset.GetSequence(filesetTagDirectorySequence)
	if !ok {
		t.Fatal("published DICOMDIR has no record sequence")
	}
	patientNames, studyDescriptions, seriesNumbers := map[string]string{}, map[string]string{}, map[string]string{}
	for _, item := range items {
		kind, _ := item.GetString(filesetTagRecordType)
		switch kind {
		case "PATIENT":
			id, _ := item.GetString(tags.PatientID)
			patientNames[id], err = item.LookupString(tags.PatientName)
		case "STUDY":
			id, _ := item.GetUID(tags.StudyInstanceUID)
			studyDescriptions[id], err = item.LookupString(tags.StudyDescription)
		case "SERIES":
			id, _ := item.GetUID(tags.SeriesInstanceUID)
			seriesNumbers[id], err = item.LookupString(tags.SeriesNumber)
		}
		if err != nil {
			t.Fatalf("decode directory record %q: %v", kind, err)
		}
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := specs[i]
			if got := patientNames[spec.patientID]; got != tc.patient {
				t.Errorf("directory Patient Name = %q, want %q", got, tc.patient)
			}
			if got := studyDescriptions[spec.studyUID]; got != tc.description {
				t.Errorf("directory Study Description = %q, want %q", got, tc.description)
			}
			if got := seriesNumbers[spec.seriesUID]; got != tc.seriesNumber {
				t.Errorf("directory Series Number = %q, want %q", got, tc.seriesNumber)
			}
			matches := parsed.Query(dicomdir.Query{SeriesInstanceUID: spec.seriesUID})
			if len(matches) != 1 {
				t.Fatalf("series query returned %d records, want 1", len(matches))
			}
			got := matches[0]
			if got.PatientName != tc.patient || got.StudyDescription != tc.description || got.SeriesNumber != tc.seriesNumber || got.SOPInstanceUID != spec.sopUID {
				t.Errorf("reopened selection = %q, %q, %q, %q; want %q, %q, %q, %q",
					got.PatientName, got.StudyDescription, got.SeriesNumber, got.SOPInstanceUID, tc.patient, tc.description, tc.seriesNumber, spec.sopUID)
			}
		})
	}
}

func TestCommitDICOMDIRBudgetRefusalPreservesPublishedIndexAndAllowsRetry(t *testing.T) {
	ctx := context.Background()
	root, set, specs := filesetSyntheticFileSet(t)
	if _, err := dicomdir.CommitDICOMDIR(ctx, set, dicomdir.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "DICOMDIR")
	previous := filesetReadBytes(t, directory)
	last := specs[len(specs)-1]
	last.fileID = dicomdir.FileID{"RETRY001"}
	last.sopUID += ".99"
	last.instanceNo = "99"
	filesetWriteInstance(t, root, last)
	if err := set.Add(ctx, last.fileID); err != nil {
		t.Fatal(err)
	}
	updated := filesetWriteBytes(t, set)
	if len(updated) <= len(previous) {
		t.Fatal("adding an instance did not grow the directory")
	}

	t.Run("stream at exact size", func(t *testing.T) {
		var streamed bytes.Buffer
		if _, err := dicomdir.WriteDICOMDIR(ctx, &streamed, set, dicomdir.WriteOptions{MaxBytes: int64(len(updated))}); err != nil {
			t.Fatalf("WriteDICOMDIR with exact budget: %v", err)
		}
		if !bytes.Equal(streamed.Bytes(), updated) {
			t.Fatal("exact byte budget changed the streamed directory")
		}
	})

	t.Run("one byte short", func(t *testing.T) {
		_, err := dicomdir.CommitDICOMDIR(ctx, set, dicomdir.WriteOptions{MaxBytes: int64(len(updated) - 1)})
		if !errors.Is(err, dicomdir.ErrResourceLimit) {
			t.Fatalf("CommitDICOMDIR = %v, want resource limit", err)
		}
		if !bytes.Equal(filesetReadBytes(t, directory), previous) {
			t.Fatal("failed commit changed the published directory")
		}
	})
	t.Run("retry at exact size after resource refusal", func(t *testing.T) {
		if _, err := dicomdir.CommitDICOMDIR(ctx, set, dicomdir.WriteOptions{MaxBytes: int64(len(updated))}); err != nil {
			t.Fatalf("CommitDICOMDIR retry: %v", err)
		}
		if !bytes.Equal(filesetReadBytes(t, directory), updated) {
			t.Fatal("retry published bytes different from the staged index")
		}
		parsed, err := dicomdir.ParseFileSet(directory)
		if err != nil {
			t.Fatal(err)
		}
		if got := parsed.Query(dicomdir.Query{SOPInstanceUID: last.sopUID}); len(got) != 1 || !filesetEqualStrings(got[0].FileID, last.fileID) {
			t.Fatalf("retried index did not resolve the added instance: %+v", got)
		}
	})
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			t.Errorf("commit left staging entry %s", entry.Name())
		}
	}
}
