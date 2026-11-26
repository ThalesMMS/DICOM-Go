package dicomdir

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func testLegacyDirectory(t *testing.T) (string, []byte) {
	t.Helper()
	root := t.TempDir()
	if export := os.Getenv("DICOMDIR_1328_LEGACY_FIXTURE_DIR"); export != "" {
		root = export
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	id := FileID{"STRUCT01"}
	src := testDirectorySource(t, RecordTypeRTStructureSet, 1)
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
	physical := &directoryNode{typ: recordTypeMRDR, record: FileRecord{FileID: id}, references: 2}
	second := &directoryNode{typ: leaf.typ, record: cloneFileRecord(leaf.record), mrdr: physical}
	leaf.mrdr = physical
	leaf.next = second
	parent := flat[len(flat)-2]
	parent.children = append(parent.children, second)
	flat = append(flat, second, physical)
	data, err := encodeDirectoryTwoPass(context.Background(), roots, flat, set.uid, set.id, nil, "", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "DICOMDIR"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return root, data
}
func TestLegacyMRDRReadAppendRefreshAndRelocation(t *testing.T) {
	root, before := testLegacyDirectory(t)
	path := filepath.Join(root, "DICOMDIR")
	if _, err := ParseFileSet(path); !errors.Is(err, ErrLegacyMRDRRequiresOptIn) {
		t.Fatalf("default %v", err)
	}
	parsed, err := ParseFileSetWithOptions(context.Background(), path, Options{LegacyMRDR: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Records()) != 1 || parsed.Statistics().Files != 1 {
		t.Fatal("aliases counted as physical files")
	}
	var refused bytes.Buffer
	if _, err := WriteDICOMDIR(context.Background(), &refused, parsed, WriteOptions{}); !errors.Is(err, ErrLegacyMRDRRequiresOptIn) || refused.Len() != 0 {
		t.Fatalf("writer opt-in %v", err)
	}
	file, err := object.ReadFile(bytes.NewReader(before))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if refs := References(file.Dataset); len(refs) != 2 || refs[0].Type != "RT STRUCTURE SET" || refs[1].FileID[0] != "STRUCT01" {
		t.Fatalf("refs %+v", refs)
	}
	// Move the one physical source; all aliases must follow without flattening.
	src := testDirectorySource(t, RecordTypeRTStructureSet, 1)
	src.Put(textElement(tagPatientIDFS, core.VRLO, "REFRESHED_PAT"))
	src.Put(uiElementValue(tagStudyInstanceUIDFS, "1.2.826.0.1.3680043.10.999.301"))
	src.Put(uiElementValue(tagSeriesInstanceUIDFS, "1.2.826.0.1.3680043.10.999.302"))
	src.Put(textElement(keyTag(0x3006, 0x0002), core.VRSH, "LONGER_LABEL"))
	testDirectoryWriteSource(t, root, FileID{"STRUCT01"}, src, transfer.ExplicitVRBigEndian)
	next, report, err := PrepareFileSetUpdate(context.Background(), path, []FileID{{"STRUCT01"}}, FileSetUpdateOptions{Mode: FileSetRefresh, LegacyMRDR: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.RefreshedFiles != 1 || report.LogicalReferences != 2 || report.MRDRRecords != 1 {
		t.Fatalf("report %+v", report)
	}
	result, err := CommitDICOMDIR(context.Background(), next, WriteOptions{LegacyMRDR: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.LogicalReferences != 2 || result.MRDRRecords != 1 {
		t.Fatalf("result %+v", result)
	}
	after, _ := os.ReadFile(path)
	if bytes.Equal(before, after) {
		t.Fatal("refresh did not change encoding")
	}
	assertDirectoryByteOffsets(t, after, 2, 1)
	reopened, err := ParseFileSetWithOptions(context.Background(), path, Options{LegacyMRDR: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Records()) != 1 || reopened.Records()[0].PatientID != "REFRESHED_PAT" {
		t.Fatal("refresh graph lost source")
	}
	testDirectoryWriteSource(t, root, FileID{"IMAGE002"}, testDirectorySource(t, RecordTypeImage, 2), transfer.ExplicitVRLittleEndian)
	appended, report, err := PrepareFileSetUpdate(context.Background(), path, []FileID{{"IMAGE002"}}, FileSetUpdateOptions{Mode: FileSetAppend, LegacyMRDR: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.NewFiles != 1 || report.LogicalReferences != 3 {
		t.Fatalf("append %+v", report)
	}
	var output bytes.Buffer
	if _, err := WriteDICOMDIR(context.Background(), &output, appended, WriteOptions{LegacyMRDR: true}); err != nil {
		t.Fatal(err)
	}
	assertDirectoryByteOffsets(t, output.Bytes(), 3, 1)
}
func TestLegacyMRDRRejectsCorruptGraph(t *testing.T) {
	for _, kind := range []string{"count", "target", "cycle", "identity", "inactive"} {
		t.Run(kind, func(t *testing.T) {
			root, data := testLegacyDirectory(t)
			file, err := object.ReadFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			items, _ := file.Dataset.GetSequence(tagDirectoryRecordSequence)
			var mrdr *object.Object
			var leaf *object.Object
			for _, item := range items {
				typ, _ := item.GetString(tagDirectoryRecordType)
				if typ == "MRDR" {
					mrdr = item
				}
				if typ == "RT STRUCTURE SET" {
					leaf = item
				}
			}
			offset, _ := mrdr.ItemOffset()
			leafOffset, _ := leaf.ItemOffset()
			// Patch fixed-width values directly so independent encoded offsets stay valid.
			patch := func(item *object.Object, tag core.Tag, value uint32) {
				start, _ := item.ItemOffset()
				found := false
				for i := int(start) + 8; i+12 <= len(data); i++ {
					if binary.LittleEndian.Uint16(data[i:]) == tag.Group && binary.LittleEndian.Uint16(data[i+2:]) == tag.Element && string(data[i+4:i+6]) == "UL" {
						binary.LittleEndian.PutUint32(data[i+8:], value)
						found = true
						break
					}
				}
				if !found {
					t.Fatal("patch tag missing")
				}
			}
			switch kind {
			case "count":
				patch(mrdr, tagNumberOfReferences, 3)
			case "target":
				patch(leaf, tagMRDRDirectoryRecordOffset, uint32(leafOffset))
			case "cycle":
				patch(mrdr, tagOffsetNextDirectoryRecord, uint32(offset))
			case "identity":
				start, _ := leaf.ItemOffset()
				tag := tagReferencedSOPInstanceUIDInFile
				for i := int(start) + 8; i+10 < len(data); i++ {
					if binary.LittleEndian.Uint16(data[i:]) == tag.Group && binary.LittleEndian.Uint16(data[i+2:]) == tag.Element && string(data[i+4:i+6]) == "UI" {
						data[i+8] = '2'
						break
					}
				}
			case "inactive":
				start, _ := mrdr.ItemOffset()
				for i := int(start) + 8; i+10 < len(data); i++ {
					if binary.LittleEndian.Uint16(data[i:]) == tagRecordInUseFlag.Group && binary.LittleEndian.Uint16(data[i+2:]) == tagRecordInUseFlag.Element {
						data[i+8] = 0
						data[i+9] = 0
						break
					}
				}
			}
			if err := os.WriteFile(filepath.Join(root, "DICOMDIR"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if got, _, err := PrepareFileSetUpdate(context.Background(), filepath.Join(root, "DICOMDIR"), nil, FileSetUpdateOptions{Mode: FileSetAppend, LegacyMRDR: true}); err == nil || got != nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}

// This oracle parses Explicit VR bytes itself, without Object.ItemOffset,
// directoryNodes or writer offset helpers. It records actual top-level Item
// tag locations and validates every UL edge against those byte positions.
func assertDirectoryByteOffsets(t *testing.T, data []byte, wantLogical, wantMRDR int) {
	t.Helper()
	type entry struct {
		typ                        string
		next, lower, target, count uint32
	}
	items := map[uint32]entry{}
	readElement := func(pos int) (core.Tag, string, int, int) {
		if pos < 0 || pos+8 > len(data) {
			t.Fatal("truncated element")
		}
		tag := core.NewTag(binary.LittleEndian.Uint16(data[pos:]), binary.LittleEndian.Uint16(data[pos+2:]))
		vr := string(data[pos+4 : pos+6])
		header := 8
		length := int(binary.LittleEndian.Uint16(data[pos+6:]))
		switch vr {
		case "OB", "OW", "OF", "OL", "OD", "OV", "SQ", "SV", "UV", "UC", "UR", "UT", "UN":
			header = 12
			if pos+12 > len(data) {
				t.Fatal("truncated long element")
			}
			length = int(binary.LittleEndian.Uint32(data[pos+8:]))
		}
		return tag, vr, pos + header, length
	}
	var skipDataSet func(int, int) int
	skipDataSet = func(pos, end int) int {
		for pos < end {
			if pos+8 > len(data) {
				t.Fatal("dataset extent")
			}
			group := binary.LittleEndian.Uint16(data[pos:])
			el := binary.LittleEndian.Uint16(data[pos+2:])
			if group == 0xFFFE {
				if el == 0xE00D || el == 0xE0DD {
					return pos + 8
				}
				if el != 0xE000 {
					t.Fatal("bad item control")
				}
				n := binary.LittleEndian.Uint32(data[pos+4:])
				if n == 0xFFFFFFFF {
					pos = skipDataSet(pos+8, end)
				} else {
					pos += 8 + int(n)
				}
				continue
			}
			_, vr, value, n := readElement(pos)
			if vr == "SQ" && uint32(n) == 0xFFFFFFFF {
				pos = skipDataSet(value, end)
			} else {
				pos = value + n
			}
		}
		return pos
	}
	first, last := uint32(0), uint32(0)
	pos := 132
	for pos < len(data) {
		tag, vr, value, n := readElement(pos)
		if tag == tagOffsetFirstRootDirectoryRecord {
			first = binary.LittleEndian.Uint32(data[value:])
		}
		if tag == tagOffsetLastRootDirectoryRecord {
			last = binary.LittleEndian.Uint32(data[value:])
		}
		if tag != tagDirectoryRecordSequence {
			pos = value + n
			continue
		}
		if vr != "SQ" {
			t.Fatal("directory VR")
		}
		pos = value
		end := len(data)
		if uint32(n) != 0xFFFFFFFF {
			end = value + n
		}
		for pos < end {
			if pos+8 > len(data) {
				t.Fatal("directory item header")
			}
			if binary.LittleEndian.Uint16(data[pos:]) != 0xFFFE {
				t.Fatal("directory item tag")
			}
			el := binary.LittleEndian.Uint16(data[pos+2:])
			if el == 0xE0DD {
				pos += 8
				break
			}
			if el != 0xE000 {
				t.Fatal("directory nonitem")
			}
			offset := uint32(pos)
			size := binary.LittleEndian.Uint32(data[pos+4:])
			start := pos + 8
			stop := len(data)
			if size != 0xFFFFFFFF {
				stop = start + int(size)
			}
			entry := entry{}
			for cursor := start; cursor < stop; {
				if binary.LittleEndian.Uint16(data[cursor:]) == 0xFFFE {
					cursor += 8
					break
				}
				key, vr, value, n := readElement(cursor)
				if vr == "UL" && n == 4 {
					number := binary.LittleEndian.Uint32(data[value:])
					switch key {
					case tagOffsetNextDirectoryRecord:
						entry.next = number
					case tagOffsetLowerLevelRecord:
						entry.lower = number
					case tagMRDRDirectoryRecordOffset:
						entry.target = number
					case tagNumberOfReferences:
						entry.count = number
					}
				}
				if key == tagDirectoryRecordType {
					entry.typ = string(bytes.TrimRight(data[value:value+n], " \x00"))
				}
				if vr == "SQ" {
					if uint32(n) == 0xFFFFFFFF {
						cursor = skipDataSet(value, stop)
					} else {
						cursor = value + n
					}
				} else {
					cursor = value + n
				}
			}
			items[offset] = entry
			if size == 0xFFFFFFFF {
				pos = skipDataSet(start, end)
			} else {
				pos = stop
			}
		}
		break
	}
	logical, mrdr := 0, 0
	counts := map[uint32]uint32{}
	for _, item := range items {
		for _, edge := range []uint32{item.next, item.lower, item.target} {
			if edge != 0 {
				if _, ok := items[edge]; !ok {
					t.Fatalf("edge %d not an actual Item tag", edge)
				}
			}
		}
		if item.typ == "MRDR" {
			mrdr++
			if item.next != 0 || item.lower != 0 {
				t.Fatal("MRDR hierarchy link")
			}
		} else if item.typ != "PATIENT" && item.typ != "STUDY" && item.typ != "SERIES" {
			logical++
		}
		if item.target != 0 {
			if items[item.target].typ != "MRDR" {
				t.Fatal("wrong indirect target")
			}
			counts[item.target]++
		}
	}
	for offset, count := range counts {
		if items[offset].count != count {
			t.Fatalf("count %d != %d", items[offset].count, count)
		}
	}
	if logical != wantLogical || mrdr != wantMRDR {
		t.Fatalf("byte oracle logical/MRDR %d/%d want %d/%d", logical, mrdr, wantLogical, wantMRDR)
	}
	for _, edge := range []uint32{first, last} {
		if _, ok := items[edge]; !ok && edge != 0 {
			t.Fatal("root is not Item tag")
		}
	}
	if first != 0 {
		seen := map[uint32]bool{}
		end := first
		for items[end].next != 0 {
			if seen[end] {
				t.Fatal("root cycle")
			}
			seen[end] = true
			end = items[end].next
		}
		if end != last {
			t.Fatal("last root mismatch")
		}
	}
}

func TestLegacyMRDRSafeInactiveLeavesAreOmitted(t *testing.T) {
	root, data := testLegacyDirectory(t)
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	items, _ := file.Dataset.GetSequence(tagDirectoryRecordSequence)
	for _, item := range items {
		typ, _ := item.GetString(tagDirectoryRecordType)
		if typ != "MRDR" && typ != "RT STRUCTURE SET" {
			continue
		}
		start, _ := item.ItemOffset()
		for i := int(start) + 8; i+12 <= len(data); i++ {
			tag := core.NewTag(binary.LittleEndian.Uint16(data[i:]), binary.LittleEndian.Uint16(data[i+2:]))
			if tag == tagRecordInUseFlag && string(data[i+4:i+6]) == "US" {
				binary.LittleEndian.PutUint16(data[i+8:], 0)
			}
			if tag == tagNumberOfReferences && string(data[i+4:i+6]) == "UL" {
				binary.LittleEndian.PutUint32(data[i+8:], 0)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, "DICOMDIR"), data, 0600); err != nil {
		t.Fatal(err)
	}
	set, _, err := PrepareFileSetUpdate(context.Background(), filepath.Join(root, "DICOMDIR"), nil, FileSetUpdateOptions{Mode: FileSetAppend, LegacyMRDR: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Records()) != 0 {
		t.Fatal("inactive sources retained")
	}
	var output bytes.Buffer
	report, err := WriteDICOMDIR(context.Background(), &output, set, WriteOptions{LegacyMRDR: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.LogicalReferences != 0 || report.MRDRRecords != 0 {
		t.Fatalf("inactive graph %+v", report)
	}
	assertDirectoryByteOffsets(t, output.Bytes(), 0, 0)
}
