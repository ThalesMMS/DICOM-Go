package nifti

import (
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/seg"
)

func TestReadLabelTableSlicerAndITKSNAP(t *testing.T) {
	slicer := `# 3D Slicer colour table
0 Background 0 0 0 0
1 liver 221 130 101 255
2 spleen 157 108 162 255
`
	table, err := ReadLabelTable(strings.NewReader(slicer))
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 2 || table[1].Name != "liver" || table[2].RGB != [3]uint8{157, 108, 162} {
		t.Fatalf("Slicer table = %+v", table)
	}

	itk := `################################################
# ITK-SnAP Label Description File
    0     0    0    0        0  0  0    "Clear Label"
    1   255    0    0        1  1  1    "Left kidney"
    7     0  255    0      0.5  1  1    "Inferior vena cava"
`
	table, err = ReadLabelTable(strings.NewReader(itk))
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 2 || table[1].Name != "Left kidney" || table[7].Name != "Inferior vena cava" || table[7].RGB != [3]uint8{0, 255, 0} {
		t.Fatalf("ITK-SNAP table = %+v", table)
	}
	segments := table.Segments()
	if got := segments[1]; got.Label != "Left kidney" || got.AlgorithmType != seg.AlgorithmManual || got.RecommendedDisplayCIELab != seg.RGBToDICOMCIELab([3]int{255, 0, 0}) {
		t.Fatalf("segment 1 = %+v", got)
	}

	freeSurfer := "2 Left_Cerebral_White_Matter 245 245 245 0\n"
	table, err = ReadLabelTable(strings.NewReader(freeSurfer))
	if err != nil || table[2].Name != "Left Cerebral White Matter" {
		t.Fatalf("FreeSurfer table = %+v, %v", table, err)
	}
}

func TestReadLabelTableRejectsMalformedLines(t *testing.T) {
	for name, text := range map[string]string{
		"short":        "1 liver 10 20\n",
		"bad value":    "70000 big 1 2 3\n",
		"bad colour":   "1 liver 300 0 0\n",
		"unterminated": "1 0 0 0 1 1 1 \"name\n",
	} {
		if _, err := ReadLabelTable(strings.NewReader(text)); !errors.Is(err, ErrLabelTable) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	huge := strings.Repeat("# padding padding padding padding padding padding\n", MaxLabelTableBytes/40)
	if _, err := ReadLabelTable(strings.NewReader(huge)); !errors.Is(err, ErrLabelTable) {
		t.Errorf("oversized table: err = %v", err)
	}
}
