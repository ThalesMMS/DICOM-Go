package seg

import (
	"bytes"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/roi"
)

func TestFrameDerivationDescriptionSurvivesPart10RoundTrip(t *testing.T) {
	source := roi.NewSegmentation3D(testGeometry(), 8, 6)
	mask := roi.NewRasterMask(8, 6)
	mask.Set(2, 2, true)
	source.SetMask(0, mask)
	source.SetMask(1, mask.Clone())
	doc, err := FromROI(FromROIOptions{SOPInstanceUID: "1.2.3.701", StudyInstanceUID: "1.2.3.702", SeriesInstanceUID: "1.2.3.703", FrameOfReferenceUID: "1.2.3.704", Segment: Segment{Number: 1, Label: "Contour", AlgorithmType: AlgorithmManual}, ReferencedImages: []ReferencedImage{{SeriesInstanceUID: "1.2.3.705", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.706"}, {SeriesInstanceUID: "1.2.3.705", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.707"}}, Segmentation: source})
	if err != nil {
		t.Fatal(err)
	}
	doc.Frames[1].DerivationDescription = "Contour interpolation; method=morphological-geodesic; manual slices=1,3"
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	read, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(read.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Frames) != 2 || got.Frames[0].DerivationDescription != "" || got.Frames[1].DerivationDescription != doc.Frames[1].DerivationDescription {
		t.Fatalf("frame provenance changed: %+v", got.Frames)
	}
}
