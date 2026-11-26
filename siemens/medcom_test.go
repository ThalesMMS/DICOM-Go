package siemens

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func utf16le(s string) []byte {
	var out []byte
	for _, unit := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, unit)
	}
	return out
}

func TestParseMedComHistory(t *testing.T) {
	text := "MEDCOM HISTORY V1.1\x00CsaPatient\x000%&Synthetic^Name%&20260926%&user\x005%&ID-1%&20260926\x00CsaStudy\x001%&ACC\x00"
	history, err := ParseMedComHistory(append(utf16le(text), 0), MedComLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if history.Version != "MEDCOM HISTORY V1.1" || len(history.Lines) != 6 || len(history.Sections) != 2 {
		t.Fatalf("history %+v", history)
	}
	patient := history.Sections[0]
	if patient.Name != "CsaPatient" || len(patient.Records) != 2 || strings.Join(patient.Records[0], "|") != "0|Synthetic^Name|20260926|user" {
		t.Fatalf("patient section %+v", patient)
	}
	for name, tc := range map[string]struct {
		data   []byte
		limits MedComLimits
		want   error
	}{
		"not MedCom":  {utf16le("OTHER"), MedComLimits{}, ErrInvalidCSA},
		"odd length":  {append(utf16le("MEDCOM HISTORY"), 'x'), MedComLimits{}, ErrInvalidCSA},
		"byte limit":  {utf16le(text), MedComLimits{MaxBytes: 8}, ErrCSALimit},
		"lines limit": {utf16le(text), MedComLimits{MaxLines: 2}, ErrCSALimit},
	} {
		if _, err := ParseMedComHistory(tc.data, tc.limits); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	dataset := object.New(std.Dictionary)
	dataset.Put(core.NewRawElement(core.NewTag(0x0029, 0x0011), core.VRLO, []byte("SIEMENS MEDCOM HEADER")))
	dataset.Put(core.NewRawElement(core.NewTag(0x0029, 0x1120), core.VROB, utf16le(text)))
	if history, err := MedComHistoryOf(dataset, MedComLimits{}); err != nil || len(history.Sections) != 2 {
		t.Fatalf("MedComHistoryOf: %+v %v", history, err)
	}
	if _, err := MedComHistoryOf(object.New(std.Dictionary), MedComLimits{}); !errors.Is(err, ErrInvalidCSA) {
		t.Fatalf("absent: err = %v", err)
	}
}

func FuzzParseMedComHistory(f *testing.F) {
	f.Add(utf16le("MEDCOM HISTORY V1.1\x00CsaPatient\x000%&a%&b\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		history, err := ParseMedComHistory(data, MedComLimits{MaxBytes: 1 << 16, MaxLines: 100})
		if err == nil && (len(history.Lines) > 100 || history.Version != history.Lines[0]) {
			t.Fatalf("history %+v", history)
		}
	})
}

// TestMedComHistoryMatchesGDCM compares every MedCom History of the
// gdcmData corpus with gdcmdump --medcom. Enable with DICOMGO_GDCMDATA_ROOT
// and gdcmdump on PATH.
func TestMedComHistoryMatchesGDCM(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("opt-in: DICOMGO_GDCMDATA_ROOT with gdcmdump on PATH")
	}
	gdcmdump, err := exec.LookPath("gdcmdump")
	if err != nil {
		t.Fatal("gdcmdump is required")
	}
	paths, _ := filepath.Glob(filepath.Join(root, "*"))
	found := 0
	for _, path := range paths {
		file, err := object.OpenFile(path)
		if err != nil {
			continue
		}
		history, err := MedComHistoryOf(file.Dataset, MedComLimits{})
		_ = file.Close()
		if err != nil {
			continue
		}
		found++
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := exec.CommandContext(ctx, gdcmdump, "--medcom", path).Output()
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		want := strings.TrimRight(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
		if got := strings.Join(history.Lines, "\n"); got != want {
			t.Errorf("%s: MedCom History\n%s\ngdcmdump\n%s", filepath.Base(path), got, want)
		}
	}
	t.Logf("%d MedCom Histories match gdcmdump --medcom", found)
	if found == 0 {
		t.Fatal("no MedCom History found in the corpus")
	}
}
