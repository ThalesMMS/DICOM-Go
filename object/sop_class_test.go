package object

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

const (
	sopClassTestMR = "1.2.840.10008.5.1.4.1.1.4"
	sopClassTestSC = "1.2.840.10008.5.1.4.1.1.7"
)

func sopClassTestFile(dataset, meta string) *File {
	file := &File{TransferSyntax: transfer.ExplicitVRLittleEndian}
	if dataset != "" {
		file.Dataset = FromElements([]core.Element{newStringElement(tagSOPClassUID, core.VRUI, dataset)}, nil)
	}
	if meta != "" {
		file.Meta = FromElements([]core.Element{newStringElement(tagMediaStorageSOPClassUID, core.VRUI, meta)}, nil)
	}
	return file
}

func TestResolveSOPClassPolicies(t *testing.T) {
	for _, tc := range []struct {
		name, dataset, meta, want string
		policy                    SOPClassPolicy
		source                    SOPClassSource
		mismatch                  bool
		err                       error
	}{
		{name: "agreement", dataset: sopClassTestMR, meta: sopClassTestMR, want: sopClassTestMR, source: SOPClassSourceAgreed},
		{name: "reject mismatch", dataset: sopClassTestMR, meta: sopClassTestSC, mismatch: true, err: ErrSOPClassMismatch},
		{name: "prefer dataset", dataset: sopClassTestMR, meta: sopClassTestSC, policy: SOPClassPreferDataset, want: sopClassTestMR, source: SOPClassSourceDataset, mismatch: true},
		{name: "prefer meta", dataset: sopClassTestMR, meta: sopClassTestSC, policy: SOPClassPreferFileMeta, want: sopClassTestSC, source: SOPClassSourceFileMeta, mismatch: true},
		{name: "dataset only", dataset: sopClassTestMR, policy: SOPClassPreferFileMeta, want: sopClassTestMR, source: SOPClassSourceDataset},
		{name: "meta only DICOMDIR", meta: mediaStorageDirectoryStorageUID, policy: SOPClassPreferDataset, want: mediaStorageDirectoryStorageUID, source: SOPClassSourceFileMeta},
		{name: "both missing", err: ErrMissingSOPClassUID},
		{name: "both empty", dataset: "\x00", meta: " ", err: ErrMissingSOPClassUID},
		{name: "empty preferred", dataset: "\x00", meta: sopClassTestSC, policy: SOPClassPreferDataset, want: sopClassTestSC, source: SOPClassSourceFileMeta},
		{name: "padding normalized", dataset: sopClassTestMR + "\x00", meta: sopClassTestMR + " ", want: sopClassTestMR, source: SOPClassSourceAgreed},
		{name: "unknown valid class", dataset: "2.25.123456789", want: "2.25.123456789", source: SOPClassSourceDataset},
		{name: "unknown policy", dataset: sopClassTestMR, policy: 255, err: ErrInvalidSOPClassPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := sopClassTestFile(tc.dataset, tc.meta)
			beforeDataset, beforeMeta := file.Dataset.ToDataSet(), file.Meta.ToDataSet()
			result, err := file.ResolveSOPClass(tc.policy)
			if !errors.Is(err, tc.err) || result.UID != tc.want || result.Source != tc.source || result.Mismatch != tc.mismatch {
				t.Fatalf("resolution=%+v error=%v", result, err)
			}
			if tc.policy <= SOPClassPreferFileMeta && (result.DatasetUID != core.NormalizeUID(tc.dataset) || result.FileMetaUID != core.NormalizeUID(tc.meta)) {
				t.Fatalf("declarations not preserved: %+v", result)
			}
			if !reflect.DeepEqual(beforeDataset, file.Dataset.ToDataSet()) || !reflect.DeepEqual(beforeMeta, file.Meta.ToDataSet()) {
				t.Fatal("resolution mutated file")
			}
		})
	}
	var nilFile *File
	if _, err := nilFile.ResolveSOPClass(SOPClassRejectMismatch); !errors.Is(err, ErrNilFile) {
		t.Fatalf("nil file error=%v", err)
	}
}

func TestResolveSOPClassRejectsInvalidDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		vr    core.VR
		value core.Value
	}{
		{"wrong VR", core.VRLO, core.StringValue{sopClassTestMR}},
		{"malformed UID", core.VRUI, core.RawValue("1..2")},
		{"leading padding", core.VRUI, core.RawValue(" " + sopClassTestMR)},
		{"raw multiplicity", core.VRUI, core.RawValue(sopClassTestMR + "\\" + sopClassTestSC)},
		{"empty raw component", core.VRUI, core.RawValue(sopClassTestMR + "\\")},
		{"string multiplicity", core.VRUI, core.StringValue{sopClassTestMR, ""}},
		{"wrong value type", core.VRUI, core.Uint16Value{12}},
		{"unloaded value", core.VRUI, nil},
		{"oversized raw", core.VRUI, core.RawValue(strings.Repeat("2", 1<<20))},
		{"oversized string", core.VRUI, core.StringValue{strings.Repeat("2", 1<<20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, tag := range []core.Tag{tagSOPClassUID, tagMediaStorageSOPClassUID} {
				file := sopClassTestFile(sopClassTestMR, sopClassTestMR)
				file.objectForTag(tag).Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: tc.vr}, Value: tc.value})
				for _, policy := range []SOPClassPolicy{SOPClassRejectMismatch, SOPClassPreferDataset, SOPClassPreferFileMeta} {
					result, err := file.ResolveSOPClass(policy)
					if !errors.Is(err, ErrInvalidSOPClassUID) || result.UID != "" || result.Source != "" {
						t.Fatalf("tag=%s policy=%d resolution=%+v error=%v", tag, policy, result, err)
					}
					if strings.Contains(err.Error(), sopClassTestMR) || len(err.Error()) > 128 {
						t.Fatalf("error includes untrusted declaration: %q", err)
					}
				}
			}
		})
	}
}

func TestResolveSOPClassPreservesIdentityDiagnostic(t *testing.T) {
	file := sopClassTestFile(sopClassTestMR, sopClassTestSC)
	before, err := file.ValidatePart10Identity()
	if err != nil {
		t.Fatal(err)
	}
	result, err := file.ResolveSOPClass(SOPClassPreferDataset)
	if err != nil || !result.Mismatch {
		t.Fatalf("resolution=%+v error=%v", result, err)
	}
	after, err := file.ValidatePart10Identity()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("identity diagnostics changed: before=%+v after=%+v error=%v", before, after, err)
	}
	if before.Count(validation.CodeFileMetaMismatch) != 3 || before.Findings[0].Tag != tagMediaStorageSOPClassUID {
		t.Fatalf("missing class mismatch diagnostic: %+v", before)
	}
}

// This optional test reads the original upstream files without importing them
// into the repository. Default tests use synthetic metadata only.
func TestResolveSOPClassGDCMRegressions(t *testing.T) {
	dir := os.Getenv("DICOM_GO_GDCM_DATA_DIR")
	if dir == "" {
		t.Skip("set DICOM_GO_GDCM_DATA_DIR to the GDCM Testing/Data directory")
	}
	for _, name := range []string{"BugGDCM2_UndefItemWrongVL", "SIEMENS-MR-RGB-16Bits", "TheralysGDCM120Bug", "SIEMENS_MAGNETOM-12-MONO2-GDCM12-VRUN"} {
		t.Run(name, func(t *testing.T) {
			file, recovery, err := OpenFileWithRecoveryLadder(filepath.Join(dir, name+".dcm"), ReadFileOptions{MaxElementBytes: 1 << 20, MaxTotalBytes: 4 << 20, MaxElements: 10_000, SkipPixelData: true}, NonConformantReadRecoveries())
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			strict, err := file.ResolveSOPClass(SOPClassRejectMismatch)
			if !errors.Is(err, ErrSOPClassMismatch) || !strict.Mismatch {
				t.Fatalf("expected mismatch diagnostic: %+v error=%v", strict, err)
			}
			result, err := file.ResolveSOPClass(SOPClassPreferDataset)
			if err != nil || result.UID != sopClassTestMR || result.FileMetaUID != sopClassTestSC || result.Source != SOPClassSourceDataset || !result.Mismatch {
				t.Fatalf("resolution=%+v error=%v", result, err)
			}
			t.Logf("dataset MR selected; meta SC mismatch retained; parser recoveries=%v", recovery.Applied)
		})
	}
}
