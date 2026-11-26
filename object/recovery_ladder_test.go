package object_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func writeLadderFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// overrunStream returns the data set of overrunFile without its File Meta.
func overrunStream(t *testing.T) []byte {
	t.Helper()
	meta, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return overrunFile(t)[len(meta):]
}

func missingSyntaxPart10(dataSet []byte) []byte {
	var buf bytes.Buffer
	buf.Write(make([]byte, 128))
	buf.WriteString("DICM")
	buf.Write(dicomtest.NewFileMetaBuilder().WithTransferSyntax("").Encode())
	buf.Write(dataSet)
	return buf.Bytes()
}

// noGroupLengthPart10 writes File Meta without its leading (0002,0000) File
// Meta Information Group Length element (12 bytes in Explicit VR).
func noGroupLengthPart10(dataSet []byte) []byte {
	var buf bytes.Buffer
	buf.Write(make([]byte, 128))
	buf.WriteString("DICM")
	buf.Write(dicomtest.NewFileMetaBuilder().Encode()[12:])
	buf.Write(dataSet)
	return buf.Bytes()
}

func TestOpenFileWithRecoveryLadder(t *testing.T) {
	valid, err := dicomtest.MinimalPart10File(transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	minimal := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, dicomtest.MinimalDataset()...)
	for _, tc := range []struct {
		name         string
		data         []byte
		wantApplied  []string
		wantAttempts int
	}{
		{"strict", valid, nil, 1},
		{"one recovery", overrunFile(t), []string{"container-overrun"}, 3},
		{"transfer syntax only", missingSyntaxPart10(minimal), []string{"transfer-syntax"}, 11},
		// A container overrun and File Meta without its group length need two
		// recoveries: no single one reads the file, so the full set is
		// reduced by backward elimination to exactly the two.
		{"two recoveries", noGroupLengthPart10(overrunStream(t)), []string{"container-overrun", "missing-meta-group-length"}, 22},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, report, err := object.OpenFileWithRecoveryLadder(writeLadderFile(t, tc.data), object.ReadFileOptions{}, object.NonConformantReadRecoveries())
			if err != nil {
				t.Fatalf("OpenFileWithRecoveryLadder: %v", err)
			}
			defer file.Close()
			if !slices.Equal(report.Applied, tc.wantApplied) || report.Recovered() != (len(tc.wantApplied) > 0) {
				t.Fatalf("Applied = %v, want %v", report.Applied, tc.wantApplied)
			}
			if report.Attempts != tc.wantAttempts {
				t.Fatalf("Attempts = %d, want %d", report.Attempts, tc.wantAttempts)
			}
			if slices.Contains(tc.wantApplied, "transfer-syntax") && report.Resolution.Source == "" {
				t.Fatal("transfer syntax recovery did not report its resolution")
			}
		})
	}
}

func TestOpenFileWithRecoveryLadderReturnsStrictError(t *testing.T) {
	path := writeLadderFile(t, overrunFile(t))
	_, report, err := object.OpenFileWithRecoveryLadder(path, object.ReadFileOptions{}, nil)
	if !errors.Is(err, parser.ErrContainerOverrun) || report.Attempts != 1 || report.Recovered() {
		t.Fatalf("no recoveries: err %v, report %+v", err, report)
	}
	garbage := writeLadderFile(t, []byte("not a DICOM file at all"))
	_, report, err = object.OpenFileWithRecoveryLadder(garbage, object.ReadFileOptions{}, object.NonConformantReadRecoveries())
	if err == nil || report.Recovered() {
		t.Fatalf("garbage: err %v, report %+v", err, report)
	}
	if limit := 2*len(object.NonConformantReadRecoveries()) + 2; report.Attempts > limit {
		t.Fatalf("Attempts = %d, want at most %d", report.Attempts, limit)
	}
}
