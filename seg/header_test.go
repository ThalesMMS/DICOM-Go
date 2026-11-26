package seg

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/roi"
	"testing"
)

func TestReadHeaderDoesNotRequireOrMaterializePixelSamples(t *testing.T) {
	masks := roi.NewSegmentation3D(testGeometry(), 8, 6)
	mask := roi.NewRasterMask(8, 6)
	mask.SetRun(1, 1, 2)
	masks.SetMask(0, mask)
	doc, err := FromROI(FromROIOptions{StudyInstanceUID: "1.2.1389.1", SeriesInstanceUID: "1.2.1389.2", SOPInstanceUID: "1.2.1389.3", FrameOfReferenceUID: "1.2.1389.4", Segment: Segment{Number: 1, Label: "Synthetic", AlgorithmType: AlgorithmManual}, ReferencedImages: []ReferencedImage{{SeriesInstanceUID: "1.2.1389.5", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.1389.6"}}, Segmentation: masks})
	if err != nil {
		t.Fatal(err)
	}
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Remove(core.TagPixelData)
	header, err := ReadHeader(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(header.Segments) != 1 || header.Segments[0].Number != 1 || len(header.ReferencedImages) != 1 || len(header.Frames) != 0 {
		t.Fatal("header identity/evidence or pixel boundary lost")
	}
	if _, err := Read(file.Dataset); err == nil {
		t.Fatal("full reader accepted omitted pixel samples")
	}
	if _, err := ReadHeader(nil); err == nil {
		t.Fatal("nil SEG header accepted")
	}
}
