package dicomdir_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomdir"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// writeSparseImage writes an image without Patient ID, Study ID, Study
// Date and Time and Series Number, as de-identified files often are, but
// with a Series Date and Time.
func writeSparseImage(t *testing.T, root string, sopUID string, components ...string) {
	t.Helper()
	writeSparseImageWithKeys(t, root, sopUID, nil, components...)
}

func writeSparseImageWithKeys(t *testing.T, root, sopUID string, keys []core.Element, components ...string) {
	t.Helper()
	path := filepath.Join(append([]string{root}, components...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	elements := []core.Element{
		filesetStringElement(tags.SOPClassUID, core.VRUI, filesetImageSOPClass),
		filesetStringElement(tags.SOPInstanceUID, core.VRUI, sopUID),
		filesetStringElement(core.NewTag(0x0008, 0x0021), core.VRDA, "20240301"),
		filesetStringElement(core.NewTag(0x0008, 0x0031), core.VRTM, "093000"),
		filesetStringElement(tags.Modality, core.VRCS, "CT"),
		filesetStringElement(tags.PatientName, core.VRPN, ""),
		filesetStringElement(tags.StudyInstanceUID, core.VRUI, "1.2.826.0.1.3680043.10.1055.20"),
		filesetStringElement(tags.StudyID, core.VRSH, ""),
		filesetStringElement(tags.SeriesInstanceUID, core.VRUI, "1.2.826.0.1.3680043.10.1055.21"),
		filesetStringElement(tags.InstanceNumber, core.VRIS, "7"),
		core.NewRawElement(tags.PixelData, core.VROB, []byte{1, 2}),
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	dataset := object.FromElements(elements, nil)
	for _, key := range keys {
		dataset.Put(key)
	}
	if err := object.WriteFile(file, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordsCopiesInventedKeys(t *testing.T) {
	root := t.TempDir()
	writeSparseImage(t, root, "1.2.826.0.1.3680043.10.1055.22", "IM1")
	set, err := dicomdir.NewFileSet(root, dicomdir.Options{InventMissingKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Add(context.Background(), dicomdir.FileID{"IM1"}); err != nil {
		t.Fatal(err)
	}
	record := set.Records()[0]
	if len(record.InventedKeys) == 0 {
		t.Fatal("sparse image has no invented keys")
	}
	want := slices.Clone(record.InventedKeys)
	record.InventedKeys[0] = "changed by caller"
	if got := set.Records()[0].InventedKeys; !slices.Equal(got, want) {
		t.Fatalf("mutating returned keys changed the file-set: got %v, want %v", got, want)
	}
}

func TestInventMissingStudyDateAndTimeAreConsistent(t *testing.T) {
	for _, tc := range []struct {
		name, date, time, studyDate, studyTime, wantDate, wantTime string
	}{
		{"matching", "20240301", "093000", "", "", "20240301", "093000"},
		{"different dates", "20240302", "093000", "", "", "19000101", "093000"},
		{"different times", "20240301", "103000", "", "", "20240301", "000000"},
		{"different dates and times", "20240302", "103000", "", "", "19000101", "000000"},
		{"source study values", "20240302", "103000", "20240228", "080000", "20240228", "080000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				root := t.TempDir()
				for i := range 3 {
					keys := []core.Element{filesetStringElement(tags.SeriesInstanceUID, core.VRUI, "1.2.826.0.1.3680043.10.1055.21."+strconv.Itoa(i+1))}
					if i == 1 {
						keys = append(keys, filesetStringElement(tags.SeriesDate, core.VRDA, tc.date), filesetStringElement(tags.SeriesTime, core.VRTM, tc.time))
						if tc.studyDate != "" {
							keys = append(keys, filesetStringElement(tags.StudyDate, core.VRDA, tc.studyDate), filesetStringElement(tags.StudyTime, core.VRTM, tc.studyTime))
						}
					}
					writeSparseImageWithKeys(t, root, "1.2.826.0.1.3680043.10.1055."+strconv.Itoa(22+i), keys, "IM"+strconv.Itoa(i))
				}
				set, err := dicomdir.NewFileSet(root, dicomdir.Options{InventMissingKeys: true})
				if err != nil {
					t.Fatal(err)
				}
				for i := range 3 {
					if reverse {
						i = 2 - i
					}
					if err := set.Add(context.Background(), dicomdir.FileID{"IM" + strconv.Itoa(i)}); err != nil {
						t.Fatal(err)
					}
				}
				if report, err := set.Validate(context.Background()); err != nil || !report.Valid {
					t.Fatalf("reverse=%v: validation: %+v, %v", reverse, report, err)
				}
				if _, err := dicomdir.CommitDICOMDIR(context.Background(), set, dicomdir.WriteOptions{}); err != nil {
					t.Fatalf("reverse=%v: commit: %v", reverse, err)
				}
				parsed, err := dicomdir.ParseFileSet(filepath.Join(root, "DICOMDIR"))
				if err != nil {
					t.Fatal(err)
				}
				for _, records := range [][]dicomdir.FileRecord{set.Records(), parsed.Records()} {
					for _, record := range records {
						if record.StudyDate != tc.wantDate || record.StudyTime != tc.wantTime {
							t.Errorf("reverse=%v: study date/time=%q/%q, want %q/%q", reverse, record.StudyDate, record.StudyTime, tc.wantDate, tc.wantTime)
						}
					}
				}
			}
		})
	}
}

// TestInventMissingKeysFillsType1KeysDeterministically: without the option
// the sparse file is refused; with it the record gets stable invented keys,
// the Study Date and Time come from the Series ones, and the DICOMDIR
// commits and parses back with them.
func TestInventMissingKeysFillsType1KeysDeterministically(t *testing.T) {
	root := t.TempDir()
	writeSparseImage(t, root, "1.2.826.0.1.3680043.10.1055.22", "P1", "S1", "IM1")
	strict, err := dicomdir.NewFileSet(root, dicomdir.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := strict.Add(context.Background(), dicomdir.FileID{"P1", "S1", "IM1"}); !errors.Is(err, dicomdir.ErrMissingRequiredKey) {
		t.Fatalf("strict Add error %v, want ErrMissingRequiredKey", err)
	}

	var first dicomdir.FileRecord
	for run := 0; run < 2; run++ {
		set, err := dicomdir.NewFileSet(root, dicomdir.Options{InventMissingKeys: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := set.Add(context.Background(), dicomdir.FileID{"P1", "S1", "IM1"}); err != nil {
			t.Fatal(err)
		}
		record := set.Records()[0]
		if run == 0 {
			first = record
			continue
		}
		if record.PatientID != first.PatientID || record.StudyID != first.StudyID || record.SeriesNumber != first.SeriesNumber {
			t.Fatalf("invented keys differ between runs: %+v vs %+v", record, first)
		}
		if _, err := dicomdir.CommitDICOMDIR(context.Background(), set, dicomdir.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"patient_id", "study_date", "study_time", "study_id", "series_number"}
	if !slices.Equal(first.InventedKeys, want) {
		t.Fatalf("invented %v, want %v", first.InventedKeys, want)
	}
	if first.StudyDate != "20240301" || first.StudyTime != "093000" || first.InstanceNumber != "7" {
		t.Fatalf("study date/time %q %q, instance %q", first.StudyDate, first.StudyTime, first.InstanceNumber)
	}
	if len(first.PatientID) != 15 || len(first.StudyID) != 13 || first.SeriesNumber == "" {
		t.Fatalf("invented patient %q study %q series %q", first.PatientID, first.StudyID, first.SeriesNumber)
	}
	parsed, err := dicomdir.ParseFileSet(filepath.Join(root, "DICOMDIR"))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Records()[0]; got.StudyID != first.StudyID || got.PatientID != first.PatientID {
		t.Fatalf("parsed record %+v", got)
	}
}
