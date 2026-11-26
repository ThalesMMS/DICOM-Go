package dicomdir

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestFileSetAppendRefreshAndAtomicDirectoryBaseline(t *testing.T) {
	root := t.TempDir()
	id := FileID{"STRUCT01"}
	source := testDirectorySource(t, RecordTypeRTStructureSet, 1)
	testDirectoryWriteSource(t, root, id, source, transfer.ExplicitVRLittleEndian)
	set := testDirectorySet(t, root, Options{})
	if err := set.Add(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitDICOMDIR(context.Background(), set, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "DICOMDIR")
	original, _ := os.ReadFile(path)
	unchanged, report, err := PrepareFileSetUpdate(context.Background(), path, []FileID{id}, FileSetUpdateOptions{Mode: FileSetAppend})
	if err != nil {
		t.Fatal(err)
	}
	if report.UnchangedFiles != 1 || report.NewFiles != 0 || unchanged.uid != set.uid {
		t.Fatalf("report %+v", report)
	}
	source.Put(textElement(keyTag(0x3006, 0x0002), core.VRSH, "REFRESHED"))
	testDirectoryWriteSource(t, root, id, source, transfer.ExplicitVRBigEndian)
	if candidate, _, err := PrepareFileSetUpdate(context.Background(), path, []FileID{id}, FileSetUpdateOptions{Mode: FileSetAppend}); candidate != nil || !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed append %v %v", candidate, err)
	}
	candidate, report, err := PrepareFileSetUpdate(context.Background(), path, []FileID{id}, FileSetUpdateOptions{Mode: FileSetRefresh})
	if err != nil {
		t.Fatal(err)
	}
	if report.RefreshedFiles != 1 || candidate.Records()[0].TransferSyntaxUID != transfer.ExplicitVRBigEndian.UID {
		t.Fatalf("report %+v", report)
	}
	before, _ := os.ReadFile(path)
	if !bytes.Equal(before, original) {
		t.Fatal("preparation published")
	}
	if _, err := CommitDICOMDIR(context.Background(), candidate, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseFileSet(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := object.FromDataSet(parsed.Records()[0].SelectionKeys(), nil)
	label, _ := obj.GetString(keyTag(0x3006, 0x0002))
	if label != "REFRESHED" {
		t.Fatalf("label %q", label)
	}
	nextID := FileID{"STRUCT02"}
	next := testDirectorySource(t, RecordTypeRTStructureSet, 2)
	testDirectoryWriteSource(t, root, nextID, next, transfer.ExplicitVRLittleEndian)
	appended, report, err := PrepareFileSetUpdate(context.Background(), path, []FileID{nextID}, FileSetUpdateOptions{Mode: FileSetAppend})
	if err != nil {
		t.Fatal(err)
	}
	if report.NewFiles != 1 || report.UnchangedFiles != 1 || len(appended.Records()) != 2 {
		t.Fatalf("append report %+v", report)
	}
	// Same-length modification with restored mtime must be caught by the hash.
	info, _ := os.Stat(path)
	current, _ := os.ReadFile(path)
	modified := append([]byte(nil), current...)
	modified[0] ^= 1
	if err := os.WriteFile(path, modified, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitDICOMDIR(context.Background(), appended, WriteOptions{}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("baseline modification %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, modified) {
		t.Fatal("stale update replaced directory")
	}
}
func TestFileSetUpdateRefusesIdentityMissingUnselectedAndLimits(t *testing.T) {
	for _, mode := range []string{"identity", "missing", "cancel", "budget", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			id := FileID{"DOC00001"}
			source := testDirectorySource(t, RecordTypeEncapsulatedDocument, 1)
			testDirectoryWriteSource(t, root, id, source, transfer.ExplicitVRLittleEndian)
			set := testDirectorySet(t, root, Options{})
			if err := set.Add(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if _, err := CommitDICOMDIR(context.Background(), set, WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "DICOMDIR")
			before, _ := os.ReadFile(path)
			ctx := context.Background()
			selected := []FileID{id}
			opts := FileSetUpdateOptions{Mode: FileSetRefresh}
			switch mode {
			case "identity":
				source.Put(uiElementValue(tagSOPInstanceUIDFS, "1.2.826.0.1.3680043.10.999.200"))
				testDirectoryWriteSource(t, root, id, source, transfer.ExplicitVRLittleEndian)
			case "missing":
				if err := os.Remove(filepath.Join(root, id[0])); err != nil {
					t.Fatal(err)
				}
				selected = nil
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "budget":
				opts.SourceOptions.Limits.MaxTotalRecordKeyBytes = 32
			case "duplicate":
				selected = append(selected, id)
			}
			result, report, err := PrepareFileSetUpdate(ctx, path, selected, opts)
			if err == nil || result != nil || report != (FileSetUpdateReport{}) {
				t.Fatalf("accepted failed %s: %v %+v", mode, err, report)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("failure changed directory")
			}
		})
	}
}
func TestFileSetRefreshMovesHierarchyAndRetainsOpaqueKeys(t *testing.T) {
	root := t.TempDir()
	id := FileID{"IMAGE001"}
	src := testDirectorySource(t, RecordTypeImage, 1)
	testDirectoryWriteSource(t, root, id, src, transfer.ExplicitVRLittleEndian)
	set := testDirectorySet(t, root, Options{})
	if err := set.Add(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	roots, flat, err := buildDirectoryHierarchy(set.records)
	if err != nil {
		t.Fatal(err)
	}
	leaf := flat[len(flat)-1]
	leaf.imported = core.DataSet{Elements: []core.Element{core.NewRawElement(keyTag(0x0011, 0x0010), core.VRLO, []byte("SYNTHETIC ")), core.NewRawElement(keyTag(0x0011, 0x1001), core.VROB, []byte{1, 2, 3, 4})}}
	encoded, err := encodeDirectoryTwoPass(context.Background(), roots, flat, set.uid, set.id, nil, "", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "DICOMDIR")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	src.Put(textElement(tagPatientIDFS, core.VRLO, "NEW_PATIENT"))
	src.Put(uiElementValue(tagStudyInstanceUIDFS, "1.2.826.0.1.3680043.10.999.401"))
	src.Put(uiElementValue(tagSeriesInstanceUIDFS, "1.2.826.0.1.3680043.10.999.402"))
	testDirectoryWriteSource(t, root, id, src, transfer.ExplicitVRLittleEndian)
	next, _, err := PrepareFileSetUpdate(context.Background(), path, []FileID{id}, FileSetUpdateOptions{Mode: FileSetRefresh})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := WriteDICOMDIR(context.Background(), &output, next, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	refs := References(file.Dataset)
	if len(refs) != 1 {
		t.Fatalf("refs %+v", refs)
	}
	items, _ := file.Dataset.GetSequence(tagDirectoryRecordSequence)
	for _, item := range items {
		typ, _ := item.GetString(tagDirectoryRecordType)
		if typ == "IMAGE" {
			e, ok := item.Get(keyTag(0x0011, 0x1001))
			if !ok {
				t.Fatal("opaque key lost")
			}
			raw, _ := e.RawBytes()
			if !bytes.Equal(raw, []byte{1, 2, 3, 4}) {
				t.Fatal("opaque key changed")
			}
		}
	}
	if next.Records()[0].PatientID != "NEW_PATIENT" {
		t.Fatal("not moved")
	}
}

func TestFileSetUpdateRefusesEmptyEntityUIDRoleCollision(t *testing.T) {
	root := t.TempDir()
	set := testDirectorySet(t, root, Options{})
	id := FileID{"IMAGE001"}
	testDirectoryWriteSource(t, root, id, testDirectorySource(t, RecordTypeImage, 1), transfer.ExplicitVRLittleEndian)
	if err := set.Add(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r := set.Records()[0]
	r.StudyInstanceUID = set.uid
	study := &directoryNode{typ: RecordTypeStudy, record: directoryEntityRecord(r)}
	patient := &directoryNode{typ: RecordTypePatient, record: directoryEntityRecord(r), children: []*directoryNode{study}}
	roots := []*directoryNode{patient}
	flat := []*directoryNode{patient, study}
	// Use the raw first/second pass only to create an invalid import fixture;
	// the normal validated writer must refuse the same File-set UID collision.
	data, err := encodeDirectory(context.Background(), roots, flat, set.uid, set.id, nil, "", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	offsets, err := encodedDirectoryOffsets(data, len(flat))
	if err != nil {
		t.Fatal(err)
	}
	for i := range flat {
		flat[i].offset = offsets[i]
	}
	data, err = encodeDirectory(context.Background(), roots, flat, set.uid, set.id, nil, "", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "DICOMDIR"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if candidate, _, err := PrepareFileSetUpdate(context.Background(), filepath.Join(root, "DICOMDIR"), nil, FileSetUpdateOptions{Mode: FileSetAppend}); err == nil || candidate != nil {
		t.Fatal("empty entity repurposed File-set UID")
	}
	if _, err := encodeDirectoryTwoPass(context.Background(), roots, flat, set.uid, set.id, nil, "", 64<<20); err == nil {
		t.Fatal("writer allowed UID role collision")
	}
}
