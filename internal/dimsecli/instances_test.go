package dimsecli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dimsetest"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestCollectInstancesFromFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	const study = "1.2.826.0.1.3680043.10.1191.5"
	dimsetest.WriteInstance(t, nested, dimsetest.Instance{StudyUID: study, SeriesUID: study + ".2", SOPInstanceUID: study + ".2.1", NonImage: true})
	dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: study, SeriesUID: study + ".1", SOPInstanceUID: study + ".1.2"})
	file := dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: study, SeriesUID: study + ".1", SOPInstanceUID: study + ".1.1"})
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	scan, err := CollectInstances(context.Background(), []string{dir, file}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Instances) != 3 || scan.Skipped != 1 {
		t.Fatalf("instances %d skipped %d", len(scan.Instances), scan.Skipped)
	}
	if got := scan.Instances[0].Record.Instance.SOPInstanceUID; got != study+".1.1" || !scan.Instances[0].Image || scan.Instances[2].Image {
		t.Fatalf("order or image classification: %+v", scan.Instances)
	}
	groups := GroupSeries(scan.Instances)
	if len(groups) != 2 || len(groups[0].Instances) != 2 || groups[1].SeriesInstanceUID != study+".2" {
		t.Fatalf("groups %+v", groups)
	}

	notDICOM := filepath.Join(dir, "readme.txt")
	if _, err := CollectInstances(context.Background(), []string{notDICOM}, 0); err == nil || strings.Contains(err.Error(), notDICOM) {
		t.Fatalf("named non-DICOM file: %v", err)
	}
	if _, err := CollectInstances(context.Background(), []string{filepath.Join(dir, "absent")}, 0); err == nil {
		t.Fatal("absent input accepted")
	}
}

func TestLoadDataSetFormats(t *testing.T) {
	ds := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0020), VR: core.VRLO}, Value: core.StringValue{"SYNTHETIC"}},
	}, std.Dictionary)
	encoded, err := dicomjson.Marshal(ds, dicomjson.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cases := map[string][]byte{
		"object.json": encoded,
		"array.txt":   append(append([]byte("[\n"), encoded...), []byte("\n]")...),
	}
	part10 := dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: "1.2.3", SeriesUID: "1.2.3.4", SOPInstanceUID: "1.2.3.4.5", PatientID: "SYNTHETIC"})
	for name, data := range cases {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(dir, "object.json"), filepath.Join(dir, "array.txt"), part10} {
		loaded, err := LoadDataSet(context.Background(), path)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		if id, _ := loaded.GetString(core.NewTag(0x0010, 0x0020)); id != "SYNTHETIC" {
			t.Fatalf("%s: Patient ID %q", filepath.Base(path), id)
		}
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDataSet(context.Background(), bad); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	if _, err := LoadDataSet(context.Background(), filepath.Join(dir, "absent.json")); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("absent file error %v", err)
	}
}
