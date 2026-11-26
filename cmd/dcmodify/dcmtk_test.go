package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
)

// flatten lists every attribute below the File Meta group as
// "path VR=value", descending into sequence items.
func flatten(dataset *object.Object, prefix string, out *[]string) {
	for _, element := range dataset.Elements() {
		tag := element.Tag()
		name := fmt.Sprintf("%s(%04X,%04X)", prefix, tag.Group, tag.Element)
		if element.VR() == core.VRSQ {
			items, _ := dataset.GetSequence(tag)
			*out = append(*out, fmt.Sprintf("%s SQ items=%d", name, len(items)))
			for i, item := range items {
				flatten(item, fmt.Sprintf("%s[%d].", name, i), out)
			}
			continue
		}
		value, _ := dataset.GetStrings(tag)
		*out = append(*out, fmt.Sprintf("%s %s=%s", name, element.VR(), strings.Join(value, `\`)))
	}
}

// TestDcmodifyMatchesDCMTK applies the same edits with DCMTK dcmodify and
// with this command and compares the resulting data sets. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestDcmodifyMatchesDCMTK(t *testing.T) {
	dcmtk := dcmtkoracle.Tool(t, "dcmodify")
	edits := [][2]string{
		{"-i", "ReferencedSeriesSequence[2].SeriesInstanceUID=1.2.3"},
		{"-i", "ContentSequence[1].ConceptNameCodeSequence[0].CodeValue=121071"},
		{"-i", "(0040,A730)[0].(0008,0104)=Finding"},
		{"-m", "PatientName=ANON^A"},
		{"-i", `ImageType=DERIVED\SECONDARY`},
		{"-e", "(0010,1000)"},
		{"-i", "Rows=512"},
		{"-e", "ReferencedSeriesSequence[0]"},
	}
	input := writeInput(t)
	dir := filepath.Dir(input)
	theirs := filepath.Join(dir, "dcmtk.dcm")
	data, _ := os.ReadFile(input)
	if err := os.WriteFile(theirs, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ours := filepath.Join(dir, "ours.dcm")
	var args []string
	for _, edit := range edits {
		// DCMTK applies each option as given; run them one by one to keep
		// the order explicit.
		dcmtkoracle.Run(t, dcmtkoracle.Exact, dcmtk, "-nb", edit[0], edit[1], theirs)
		args = append(args, edit[0], edit[1])
	}
	args = append(args, "-o", ours, input)
	var stderr bytes.Buffer
	if code := run(args, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var got, want []string
	for path, out := range map[string]*[]string{ours: &got, theirs: &want} {
		file, err := object.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		flatten(file.Dataset, "", out)
		_ = file.Close()
		sort.Strings(*out)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("data sets differ\nours:\n%s\n\nDCMTK:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
