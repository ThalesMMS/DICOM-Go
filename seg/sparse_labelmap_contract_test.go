package seg

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
)

func TestReferenceSparseLabelMapBinaryConversionKeepsFrameSources(t *testing.T) {
	for _, high := range []uint16{239, 731} {
		t.Run(fmt.Sprintf("label_%d", high), func(t *testing.T) {
			doc := patternDocument(LabelMapSegmentationStorage, SegmentationTypeLabelMap)
			doc.Rows, doc.Columns = 3, 3
			doc.Segments = []Segment{{Number: 0, Label: "Background"}, {Number: 2, Label: "Region A"}, {Number: int(high), Label: "Region B"}}
			const sourceUID = "2.25.7314"
			doc.ReferencedImages = []ReferencedImage{{
				SeriesInstanceUID: "2.25.7315",
				SOPClassUID:       "1.2.840.10008.5.1.4.1.1.2.1",
				SOPInstanceUID:    sourceUID,
			}}
			planes := [][]uint16{
				{0, high, 0, 2, 0, 0, high, 0, 0},
				{0, 0, 0, 0, 0, 0, 0, 0, 0},
				{2, 0, 2, 0, 0, 0, 0, 2, 0},
				{0, 0, 0, high, 0, 0, 0, 0, high},
			}
			for i, z := range []float64{1, 4, 8, 12} {
				doc.Frames = append(doc.Frames, Frame{
					SliceIndex: i, ReferencedSOPInstanceUID: sourceUID, ReferencedFrameNumber: i*3 + 2,
					LabelMap: planes[i],
					Geometry: render.SliceGeometry{
						Origin: render.Vec3{X: 10, Y: -4, Z: z},
						RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1},
						Normal: render.Vec3{Z: 1}, RowSpacing: 0.75, ColSpacing: 1.5,
						Rows: doc.Rows, Columns: doc.Columns,
					},
				})
			}
			converted, numbers, err := ToBinary(doc, BinaryOptions{SOPInstanceUID: "2.25.7316", SeriesInstanceUID: "2.25.7317"})
			if err != nil {
				t.Fatal(err)
			}
			if len(numbers) != 2 || numbers[2] != 1 || numbers[int(high)] != 2 {
				t.Fatalf("label map conversion number mapping = %v", numbers)
			}
			file, err := Write(converted)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var data bytes.Buffer
			if err := object.WriteFile(&data, file); err != nil {
				t.Fatal(err)
			}
			parsed, err := object.ReadFile(&data)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.Close()
			pixelData, ok := parsed.Dataset.Get(derivedio.TagPixelData)
			if !ok || pixelData.Length() != 6 {
				t.Fatalf("four nine-bit frames with even padding should occupy 6 bytes")
			}
			round, err := Read(parsed.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if round.SegmentationType != SegmentationTypeBinary || round.SegmentsOverlap != SegmentsOverlapNo ||
				len(round.Segments) != 2 || round.Segments[0].Label != "Region A" || round.Segments[1].Label != "Region B" ||
				len(round.Frames) != 4 {
				t.Fatalf("converted object type=%q overlap=%q segments=%+v frames=%d", round.SegmentationType,
					round.SegmentsOverlap, round.Segments, len(round.Frames))
			}
			for i, want := range []struct {
				segment, plane int
				label          uint16
			}{{1, 0, 2}, {1, 2, 2}, {2, 0, high}, {2, 3, high}} {
				frame := round.Frames[i]
				original := doc.Frames[want.plane]
				if frame.SegmentNumber != want.segment || frame.Geometry.Origin != original.Geometry.Origin ||
					frame.ReferencedSOPInstanceUID != sourceUID || frame.ReferencedFrameNumber != original.ReferencedFrameNumber {
					t.Fatalf("binary frame %d: segment=%d origin=%v source=%s frame=%d, want segment %d at %v from %s frame %d",
						i, frame.SegmentNumber, frame.Geometry.Origin, frame.ReferencedSOPInstanceUID, frame.ReferencedFrameNumber,
						want.segment, original.Geometry.Origin, sourceUID, original.ReferencedFrameNumber)
				}
				for pixel, label := range original.LabelMap {
					if got := frame.Mask.Get(pixel%doc.Columns, pixel/doc.Columns); got != (label == want.label) {
						t.Errorf("frame %d pixel %d set = %v, want %v", i, pixel, got, label == want.label)
					}
				}
			}
		})
	}
}
