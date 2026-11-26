package vendors

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
)

// gdcmdump runs gdcmdump with option on path and returns its lines.
func gdcmdump(t *testing.T, tool, option, path string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, tool, option, path).Output()
	if err != nil {
		t.Fatalf("gdcmdump %s %s: %v", option, filepath.Base(path), err)
	}
	return strings.Split(strings.TrimRight(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n"), "\n")
}

// elscintLines prints protocol as gdcmdump --elscint does.
func elscintLines(protocol *ElscintProtocol) []string {
	var lines []string
	entries := func(item []ElscintEntry) {
		for _, entry := range item {
			lines = append(lines, "  "+entry.Key+" ["+strings.Join(entry.Values, `\`)+"]")
		}
	}
	for _, group := range protocol.Groups {
		if !group.Multiple {
			lines = append(lines, "ELSCINT1/Item name: ["+group.Name+"]")
			entries(group.Items[0])
		} else {
			lines = append(lines, "ELSCINT1/Item name: "+group.Name)
			for i, item := range group.Items {
				if len(item) == 0 {
					lines = append(lines, " No SubItems (Empty)")
				} else {
					lines = append(lines, " SubItems #"+strconv.Itoa(i))
				}
				entries(item)
			}
		}
		lines = append(lines, "")
	}
	return lines
}

// TestVendorBlobsMatchGDCM decodes every GE Protocol Data Block, ELSCINT1
// protocol and VEPRO data of the gdcmData corpus and compares them with
// gdcmdump --pdb, --elscint and --vepro. Enable with DICOMGO_GDCMDATA_ROOT
// and gdcmdump on PATH.
func TestVendorBlobsMatchGDCM(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("opt-in: DICOMGO_GDCMDATA_ROOT with gdcmdump on PATH")
	}
	tool, err := exec.LookPath("gdcmdump")
	if err != nil {
		t.Fatal("gdcmdump is required")
	}
	paths, _ := filepath.Glob(filepath.Join(root, "*"))
	counts := map[string]int{}
	for _, path := range paths {
		file, err := object.OpenFile(path)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if block, err := GEProtocolDataBlockOf(file.Dataset, Limits{}); err == nil {
			counts["pdb"]++
			want := gdcmdump(t, tool, "--pdb", path)
			got := strings.Split(strings.TrimRight(strings.ReplaceAll(block.Text, "\r\n", "\n"), "\n"), "\n")
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s: Protocol Data Block differs from gdcmdump (%d/%d lines)", name, len(got), len(want))
			}
			if !block.XML && len(block.Entries) == 0 {
				t.Errorf("%s: no entries", name)
			}
		} else if err != ErrNotFound {
			t.Errorf("%s: %v", name, err)
		}
		if protocol, err := ElscintProtocolOf(file.Dataset, Limits{}); err == nil {
			counts["elscint"]++
			want := gdcmdump(t, tool, "--elscint", path)[1:] // after the tag line
			got := elscintLines(protocol)
			if strings.TrimSpace(strings.Join(got, "\n")) != strings.TrimSpace(strings.Join(want, "\n")) {
				t.Errorf("%s: ELSCINT1 protocol differs from gdcmdump (%d/%d lines)", name, len(got), len(want))
			}
		} else if err != ErrNotFound {
			t.Errorf("%s: %v", name, err)
		}
		if data, err := VEPRODataOf(file.Dataset, Limits{}); err == nil {
			counts["vepro"]++
			fields := map[string]string{}
			for _, line := range gdcmdump(t, tool, "--vepro", path)[1:] {
				key, value, _ := strings.Cut(strings.TrimSpace(line), ": ")
				fields[key] = strings.TrimRight(value, " ")
			}
			got := map[string]string{"ID": data.ID, "Version": data.Version, "UserName": data.UserName, "RecDate": data.RecordingDate,
				"RecTime": data.RecordingTime, "RecPlace": data.RecordingPlace, "RecSource": data.RecordingSource,
				"StudyUID": data.StudyInstanceUID, "SeriesUID": data.SeriesUID, "Modality": data.Modality}
			for i, v := range data.UserAddress {
				got["UserAdress"+strconv.Itoa(i+1)] = v
			}
			for i, v := range data.DataFields {
				got["DF"+strconv.Itoa(i+1)] = v
			}
			for key, want := range fields {
				if key == "" {
					continue
				}
				if got[key] != strings.TrimRight(want, " \x00") {
					t.Errorf("%s: VEPRO %s = %q, gdcmdump %q", name, key, got[key], want)
				}
			}
		} else if err != ErrNotFound {
			t.Errorf("%s: %v", name, err)
		}
		_ = file.Close()
	}
	t.Logf("decoded and matched with gdcmdump: %v", counts)
	if counts["pdb"] == 0 || counts["elscint"] == 0 || counts["vepro"] == 0 {
		t.Fatalf("corpus blobs not found: %v", counts)
	}
}
