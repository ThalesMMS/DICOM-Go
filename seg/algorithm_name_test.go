package seg

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/roi"
)

// TestSegmentAlgorithmName writes and reads Segment Algorithm Name
// (0062,0009), which PS3.3 C.8.20.2 requires for AUTOMATIC and SEMIAUTOMATIC
// segments, and refuses a segment that needs it but has none.
func TestSegmentAlgorithmName(t *testing.T) {
	build := func(segment Segment) (*Document, error) {
		mask := roi.NewRasterMask(8, 6)
		mask.SetRun(2, 1, 4)
		source := roi.NewSegmentation3D(testGeometry(), 8, 6)
		source.SetMask(0, mask)
		return FromROI(FromROIOptions{
			SOPInstanceUID: "2.25.1101.1", StudyInstanceUID: "2.25.1101.2",
			SeriesInstanceUID: "2.25.1101.3", FrameOfReferenceUID: "2.25.1101.4",
			Segment: segment,
			ReferencedImages: []ReferencedImage{
				{SeriesInstanceUID: "2.25.1101.5", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "2.25.1101.5.0"},
				{SeriesInstanceUID: "2.25.1101.5", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "2.25.1101.5.1"},
			},
			Segmentation: source,
		})
	}
	doc, err := build(Segment{Number: 1, Label: "Consensus", AlgorithmType: AlgorithmSemiautomatic, AlgorithmName: "STAPLE"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Segments[0]; got.AlgorithmType != AlgorithmSemiautomatic || got.AlgorithmName != "STAPLE" {
		t.Fatalf("segment read back as %q/%q", got.AlgorithmType, got.AlgorithmName)
	}
	for name, segment := range map[string]Segment{
		"automatic without name":   {Number: 1, Label: "A", AlgorithmType: AlgorithmAutomatic},
		"semiautomatic blank name": {Number: 1, Label: "A", AlgorithmType: AlgorithmSemiautomatic, AlgorithmName: "  "},
		"unknown type":             {Number: 1, Label: "A", AlgorithmType: "GUESSED", AlgorithmName: "x"},
	} {
		doc, err := build(segment)
		if err == nil {
			_, err = Write(doc)
		}
		if !errors.Is(err, ErrInvalidObject) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
