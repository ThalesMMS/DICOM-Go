package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/seg"
)

// A label map's background (Pixel Padding Value) is no segment in the NIfTI
// labelmap or its colour table, whatever its value.
func TestLabelMapBackgroundIsNoSegment(t *testing.T) {
	document := &seg.Document{
		SOPClassUID: seg.LabelMapSegmentationStorage, SegmentationType: seg.SegmentationTypeLabelMap,
		Rows: 1, Columns: 3, LabelMapBackground: 7,
		Segments: []seg.Segment{{Number: 7, Label: "Background"}, {Number: 2, Label: "Liver"}},
	}
	frame := seg.Frame{LabelMap: []uint16{7, 2, 7}}
	for column, want := range []int{0, 2, 0} {
		got, err := pixelSegment(document, frame, column, 0, 0.5)
		if err != nil || got != want {
			t.Fatalf("pixel %d = %d %v, want %d", column, got, err, want)
		}
	}
	var table bytes.Buffer
	if err := writeTable(&table, document); err != nil {
		t.Fatal(err)
	}
	if text := table.String(); strings.Contains(text, "Background") || !strings.Contains(text, "2 Liver ") {
		t.Fatalf("colour table:\n%s", text)
	}

	// Beside a nonzero background, 0 is a segment that a NIfTI labelmap
	// cannot hold.
	document.Segments = append(document.Segments, seg.Segment{Number: 0, Label: "Tissue"})
	if _, err := pixelSegment(document, seg.Frame{LabelMap: []uint16{0}}, 0, 0, 0.5); err == nil {
		t.Fatal("segment 0 beside background 7 was written as no segment")
	}
}
