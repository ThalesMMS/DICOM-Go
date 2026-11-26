package dicomdir_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomdir"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// dcmtkRun runs tool (a path from dcmtkoracle.Tool) in dir and returns its
// combined output.
func dcmtkRun(t *testing.T, dir, tolerance, tool string, args ...string) (string, error) {
	t.Helper()
	cmd := dcmtkoracle.Command(context.Background(), t, tolerance, tool, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestDICOMDIRProfilesMatchDCMTK writes profile DICOMDIRs and checks them
// with DCMTK 3.7.0: dcmdump reads them without warnings, dcmmkdir accepts
// them for update in its profile mode without changing them (-w), and the
// directory records carry the keys and icon geometry dcmmkdir itself
// writes for the same files. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestDICOMDIRProfilesMatchDCMTK(t *testing.T) {
	dcmdump := dcmtkoracle.Tool(t, "dcmdump")
	dcmmkdir := dcmtkoracle.Tool(t, "dcmmkdir")
	window := []core.Element{
		filesetStringElement(core.NewTag(0x0028, 0x1050), core.VRDS, "2048"),
		filesetStringElement(core.NewTag(0x0028, 0x1051), core.VRDS, "4096"),
	}
	gradient := func(x, y int) int { return (x*37 + y*101) % 4096 }
	xa1 := xaImage("IMG00001", 64, 64, gradient)
	xa1.extra = window
	xa2 := xaImage("IMG00002", 32, 48, gradient, constant(2048), constant(4095))
	xa2.extra = window
	ct := profileImage{
		fileID: dicomdir.FileID{"CT", "IMG00001"}, sopClass: profileCTClass, modality: "CT",
		syntax: transfer.ExplicitVRLittleEndian, rows: 32, columns: 32, stored: 12, frames: []func(x, y int) int{gradient},
		imageType: []string{"ORIGINAL", "PRIMARY", "AXIAL"}, extra: window,
	}
	cases := []struct {
		profile dicomdir.Profile
		flag    string
		images  []profileImage
	}{
		{dicomdir.ProfileXA1KCD, "-Pxa", []profileImage{xa1, xa2}},
		{dicomdir.ProfileGeneralPurpose, "-Pgp", []profileImage{ct}},
		{dicomdir.ProfileGeneralDVDJPEG, "-Pdv", []profileImage{ct, xa1}},
		{dicomdir.ProfileGeneralUSBJPEG, "-Pfl", []profileImage{ct, xa1}},
	}
	for _, tc := range cases {
		t.Run(string(tc.profile), func(t *testing.T) {
			root, fileSet := profileFileSet(t, tc.images...)
			if _, err := dicomdir.CommitDICOMDIR(context.Background(), fileSet, dicomdir.WriteOptions{Profile: tc.profile}); err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, image := range tc.images {
				files = append(files, filepath.ToSlash(filepath.Join(image.fileID...)))
			}
			if out, err := dcmtkRun(t, root, dcmtkoracle.Accepted, dcmdump, "-q", "DICOMDIR"); err != nil || strings.Contains(out, "W: ") || strings.Contains(out, "E: ") {
				t.Fatalf("dcmdump: %v\n%s", err, out)
			}
			ours, err := os.ReadFile(filepath.Join(root, "DICOMDIR"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "OURSDIR"), ours, 0o600); err != nil {
				t.Fatal(err)
			}
			// dcmmkdir +U loads the DICOMDIR, checks each file against the
			// profile and rewrites it; only its own icons may differ.
			args := append([]string{"-v", tc.flag, "+U", "-nb", "+F", "ICONS"}, files...)
			out, err := dcmtkRun(t, root, "dcmmkdir keys present with equal values; icon geometry only", dcmmkdir, args...)
			if err != nil || strings.Contains(out, "E: ") || strings.Contains(out, "W: ") || !strings.Contains(out, "updating DICOMDIR") {
				t.Fatalf("dcmmkdir %s +U rejected the DICOMDIR: %v\n%s", tc.flag, err, out)
			}
			compareDirectoryRecords(t, filepath.Join(root, "OURSDIR"), filepath.Join(root, "DICOMDIR"), true)

			reference := filepath.Join(root, "DCMTKDIR")
			args = append([]string{tc.flag, "+D", "DCMTKDIR", "+F", "ICONS"}, files...)
			if out, err := dcmtkRun(t, root, "every dcmmkdir key present; icon geometry only", dcmmkdir, args...); err != nil {
				t.Fatalf("dcmmkdir %s: %v\n%s", tc.flag, err, out)
			}
			compareDirectoryRecords(t, filepath.Join(root, "OURSDIR"), reference, false)
		})
	}
}

// compareDirectoryRecords checks that every key dcmmkdir writes in a record
// is also written here (with the same value when sameValues is set) and that
// icons have the same geometry; icon pixels are rendered differently.
func compareDirectoryRecords(t *testing.T, oursPath, theirsPath string, sameValues bool) {
	t.Helper()
	read := func(path string) map[string]*object.Object {
		file, err := object.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		records, _ := file.Dataset.GetSequence(filesetTagDirectorySequence)
		out := map[string]*object.Object{}
		for _, record := range records {
			kind, _ := record.GetString(filesetTagRecordType)
			key := kind
			for _, tag := range []core.Tag{core.NewTag(0x0010, 0x0020), core.NewTag(0x0020, 0x000D), core.NewTag(0x0020, 0x000E), core.NewTag(0x0004, 0x1511)} {
				if value, ok := record.GetString(tag); ok {
					key += "|" + strings.TrimRight(value, "\x00 ")
				}
			}
			out[key] = record
		}
		return out
	}
	ours, theirs := read(oursPath), read(theirsPath)
	if len(ours) != len(theirs) {
		t.Fatalf("%d records, dcmmkdir %d", len(ours), len(theirs))
	}
	for key, reference := range theirs {
		record := ours[key]
		if record == nil {
			t.Fatalf("record %s missing", key)
		}
		for _, element := range reference.Elements() {
			tag := element.Tag()
			if tag.Group == 0x0004 && tag.Element >= 0x1400 && tag.Element <= 0x1420 {
				continue
			}
			if !record.Has(tag) {
				t.Errorf("%s: dcmmkdir writes %s, this writer does not", key, tag)
				continue
			}
			if theirs, ok := element.RawBytes(); ok && sameValues {
				mine, _ := record.GetRaw(tag)
				if strings.TrimRight(string(mine), " \x00") != strings.TrimRight(string(theirs), " \x00") {
					t.Errorf("%s: %s = %q, dcmmkdir %q", key, tag, mine, theirs)
				}
			}
		}
		if icons, ok := reference.GetSequence(profileTagIconSequence); ok {
			mine, _ := record.GetSequence(profileTagIconSequence)
			if len(mine) != 1 || len(icons) != 1 {
				t.Fatalf("%s: icons %d, dcmmkdir %d", key, len(mine), len(icons))
			}
			for _, tag := range []core.Tag{core.NewTag(0x0028, 0x0010), core.NewTag(0x0028, 0x0011), core.NewTag(0x0028, 0x0100), core.NewTag(0x0028, 0x0004)} {
				a, _ := mine[0].GetRaw(tag)
				b, _ := icons[0].GetRaw(tag)
				if strings.TrimRight(string(a), " ") != strings.TrimRight(string(b), " ") {
					t.Errorf("%s: icon %s = %q, dcmmkdir %q", key, tag, a, b)
				}
			}
		}
	}
}
