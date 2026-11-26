package seg

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/roi"
)

const (
	patternRows    = 5
	patternColumns = 7 // 35 pixels: odd, so 1-bit frames straddle bytes
)

func patternDocument(sopClassUID, segmentationType string) *Document {
	return &Document{
		SOPClassUID: sopClassUID, SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1000.1",
		StudyInstanceUID: "1.2.826.0.1.3680043.10.543.1000.2", SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.1000.3",
		FrameOfReferenceUID: "1.2.826.0.1.3680043.10.543.1000.4",
		Rows:                patternRows, Columns: patternColumns, SegmentationType: segmentationType,
	}
}

// fractionalPattern is the sample of pixel i of a segment at a slice.
func fractionalPattern(segment, slice, i, maxValue int) uint8 {
	return uint8((i*13 + slice*5 + segment) % (maxValue + 1))
}

func TestFractionalSegmentationRoundTripsBitExact(t *testing.T) {
	for _, tc := range []struct {
		fractionalType string
		maxValue       int
	}{{FractionalTypeProbability, 200}, {FractionalTypeOccupancy, 255}, {FractionalTypeProbability, 1}} {
		t.Run(fmt.Sprintf("%s-%d", tc.fractionalType, tc.maxValue), func(t *testing.T) {
			doc := patternDocument(SegmentationStorage, SegmentationTypeFractional)
			doc.FractionalType, doc.MaxFractionalValue = tc.fractionalType, tc.maxValue
			doc.Segments = []Segment{{Number: 1, Label: "A"}, {Number: 2, Label: "B"}}
			for segment := 1; segment <= 2; segment++ {
				for slice := 0; slice < 2; slice++ {
					samples := make([]uint8, patternRows*patternColumns)
					for i := range samples {
						samples[i] = fractionalPattern(segment, slice, i, tc.maxValue)
					}
					doc.Frames = append(doc.Frames, Frame{SegmentNumber: segment, SliceIndex: slice, Fractional: samples})
				}
			}
			file, err := Write(doc)
			if err != nil {
				t.Fatal(err)
			}
			for tag, want := range map[core.Tag]int{derivedio.TagBitsAllocated: 8, derivedio.TagBitsStored: 8, derivedio.TagHighBit: 7, tagMaximumFractionalValue: tc.maxValue} {
				if got := derivedio.Int(file.Dataset, tag); got != want {
					t.Fatalf("%s = %d, want %d", tag, got, want)
				}
			}
			wantOverlap := SegmentsOverlapNo
			for slice := 0; slice < 2; slice++ {
				for i := 0; i < patternRows*patternColumns; i++ {
					if fractionalPattern(1, slice, i, tc.maxValue) > 0 && fractionalPattern(2, slice, i, tc.maxValue) > 0 {
						wantOverlap = SegmentsOverlapYes
					}
				}
			}
			if got := derivedio.CleanString(file.Dataset, tagSegmentsOverlap); got != wantOverlap {
				t.Fatalf("Segments Overlap = %q, want %q", got, wantOverlap)
			}
			got, err := Read(file.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if got.FractionalType != tc.fractionalType || got.MaxFractionalValue != tc.maxValue || got.SegmentationType != SegmentationTypeFractional {
				t.Fatalf("read %s %d %s", got.FractionalType, got.MaxFractionalValue, got.SegmentationType)
			}
			if len(got.Frames) != len(doc.Frames) {
				t.Fatalf("%d frames", len(got.Frames))
			}
			for i := range doc.Frames {
				if got.Frames[i].SegmentNumber != doc.Frames[i].SegmentNumber || !reflect.DeepEqual(got.Frames[i].Fractional, doc.Frames[i].Fractional) {
					t.Fatalf("frame %d differs", i)
				}
			}
		})
	}
}

func TestFractionalWriteValidation(t *testing.T) {
	base := func() *Document {
		doc := patternDocument(SegmentationStorage, SegmentationTypeFractional)
		doc.FractionalType, doc.MaxFractionalValue = FractionalTypeProbability, 100
		doc.Segments = []Segment{{Number: 1}}
		doc.Frames = []Frame{{SegmentNumber: 1, Fractional: make([]uint8, patternRows*patternColumns)}}
		return doc
	}
	mask := roi.NewRasterMask(patternColumns, patternRows)
	mask.Set(1, 1, true)
	for name, tc := range map[string]struct {
		mutate func(*Document)
		want   error
	}{
		"sample above maximum": {func(d *Document) { d.Frames[0].Fractional[3] = 101 }, ErrInvalidObject},
		"unknown type":         {func(d *Document) { d.FractionalType = "LIKELIHOOD" }, ErrInvalidObject},
		"maximum zero":         {func(d *Document) { d.MaxFractionalValue = 0 }, ErrInvalidObject},
		"maximum above 255":    {func(d *Document) { d.MaxFractionalValue = 256 }, ErrInvalidObject},
		"short frame":          {func(d *Document) { d.Frames[0].Fractional = d.Frames[0].Fractional[:4] }, ErrInvalidObject},
		"mask only":            {func(d *Document) { d.Frames[0].Fractional, d.Frames[0].Mask = nil, mask }, ErrUnsupportedFramePayload},
		"samples in binary":    {func(d *Document) { d.SegmentationType = SegmentationTypeBinary }, ErrUnsupportedFramePayload},
	} {
		t.Run(name, func(t *testing.T) {
			doc := base()
			tc.mutate(doc)
			if _, err := Write(doc); !errors.Is(err, tc.want) {
				t.Fatalf("Write error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadRejectsBitsAllocatedThatContradictsTheType(t *testing.T) {
	for name, tc := range map[string]struct {
		sopClassUID, segmentationType string
		bits                          int
	}{
		"binary with 8 bits":     {SegmentationStorage, SegmentationTypeBinary, 8},
		"fractional with 1 bit":  {SegmentationStorage, SegmentationTypeFractional, 1},
		"label map with 32 bits": {LabelMapSegmentationStorage, SegmentationTypeLabelMap, 32},
	} {
		t.Run(name, func(t *testing.T) {
			dataset := derivedio.Object(
				derivedio.UI(derivedio.TagSOPClassUID, tc.sopClassUID),
				derivedio.CS(tagSegmentationType, tc.segmentationType),
				derivedio.CS(tagSegmentationFractionalType, FractionalTypeProbability),
				derivedio.US(tagMaximumFractionalValue, 255),
				derivedio.US(derivedio.TagRows, 1), derivedio.US(derivedio.TagColumns, 2),
				derivedio.IS(derivedio.TagNumberOfFrames, 1),
				derivedio.US(derivedio.TagBitsAllocated, uint16(tc.bits)),
				derivedio.Raw(derivedio.TagPixelData, core.VROB, []byte{1, 2, 3, 4, 5, 6, 7, 8}),
			)
			if _, err := Read(dataset); !errors.Is(err, ErrUnsupportedPixelData) {
				t.Fatalf("Read error = %v, want ErrUnsupportedPixelData", err)
			}
		})
	}
}

func TestToROIThresholdsFractionalFrames(t *testing.T) {
	doc := patternDocument(SegmentationStorage, SegmentationTypeFractional)
	doc.FractionalType, doc.MaxFractionalValue = FractionalTypeProbability, 10
	doc.Segments = []Segment{{Number: 1}}
	samples := make([]uint8, patternRows*patternColumns)
	copy(samples, []uint8{0, 1, 4, 5, 6, 10})
	doc.Frames = []Frame{{SegmentNumber: 1, Fractional: samples}}
	count := func(opts ROIOptions) int {
		t.Helper()
		volume, err := ToROIWithOptions(doc, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		return volume.Mask(0).Count()
	}
	if got := count(ROIOptions{}); got != 3 { // 5, 6, 10 reach 0.5
		t.Fatalf("default threshold: %d pixels", got)
	}
	if got := count(ROIOptions{FractionalThreshold: 0.01}); got != 5 { // above zero
		t.Fatalf("low threshold: %d pixels", got)
	}
	if got := count(ROIOptions{FractionalThreshold: 1}); got != 1 {
		t.Fatalf("threshold 1: %d pixels", got)
	}
	if _, err := ToROIWithOptions(doc, 1, ROIOptions{FractionalThreshold: 1.5}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("threshold 1.5: %v", err)
	}
}

func labelMapPatternDocument(maxLabel int, frames int) *Document {
	doc := patternDocument(LabelMapSegmentationStorage, SegmentationTypeLabelMap)
	for number := 0; number <= maxLabel; number++ {
		doc.Segments = append(doc.Segments, Segment{Number: number, Label: fmt.Sprintf("L%d", number)})
	}
	for slice := 0; slice < frames; slice++ {
		labels := make([]uint16, patternRows*patternColumns)
		for i := range labels {
			labels[i] = uint16((i*97 + slice) % (maxLabel + 1))
		}
		doc.Frames = append(doc.Frames, Frame{SliceIndex: slice, LabelMap: labels})
	}
	return doc
}

func TestLabelMapPreservesBackgroundLabel(t *testing.T) {
	for _, tc := range []struct{ label, want string }{
		{"", "Segment 0"}, {"  ", "Segment 0"},
		{"Segment background", "Segment background"}, {"Segment 0 custom", "Segment 0 custom"},
	} {
		doc := labelMapPatternDocument(1, 1)
		doc.Segments[0].Label = tc.label
		file, err := Write(doc)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Read(file.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		if got := back.Segments[0]; got.Number != 0 || got.Label != tc.want {
			t.Errorf("label %q: got %+v, want %q", tc.label, got, tc.want)
		}
	}
}

func TestLabelMapBitsAllocated(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxLabel  int
		requested int
		want      int
		vr        core.VR
	}{
		{"auto 8", 255, 0, 8, core.VROB},
		{"auto 16", 256, 0, 16, core.VROW},
		{"forced 16", 3, 16, 16, core.VROW},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := labelMapPatternDocument(tc.maxLabel, 3)
			doc.LabelMapBitsAllocated = tc.requested
			file, err := Write(doc)
			if err != nil {
				t.Fatal(err)
			}
			if got := derivedio.Int(file.Dataset, derivedio.TagBitsAllocated); got != tc.want {
				t.Fatalf("Bits Allocated %d, want %d", got, tc.want)
			}
			if element, _ := file.Dataset.Get(derivedio.TagPixelData); element.VR() != tc.vr {
				t.Fatalf("Pixel Data VR %s, want %s", element.VR(), tc.vr)
			}
			if got := derivedio.CleanString(file.Dataset, tagSegmentsOverlap); got != SegmentsOverlapNo {
				t.Fatalf("Segments Overlap %q", got)
			}
			got, err := Read(file.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if got.LabelMapBitsAllocated != tc.want {
				t.Fatalf("read Bits Allocated %d", got.LabelMapBitsAllocated)
			}
			for i := range doc.Frames {
				if !reflect.DeepEqual(got.Frames[i].LabelMap, doc.Frames[i].LabelMap) {
					t.Fatalf("frame %d labels differ", i)
				}
			}
			if len(got.Segments) != len(doc.Segments) || got.Segments[0].Number != 0 {
				t.Fatalf("segments %+v", got.Segments[:1])
			}
		})
	}
	doc := labelMapPatternDocument(300, 1)
	doc.LabelMapBitsAllocated = 8
	if _, err := Write(doc); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("8-bit label map with label 300: %v", err)
	}
	doc = labelMapPatternDocument(3, 1)
	doc.Segments = doc.Segments[1:] // value 0 no longer described
	if _, err := Write(doc); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("undescribed value 0: %v", err)
	}
}

// overlapDocument has three segments over two slices: 1 and 2 are disjoint
// stripes, 3 covers the left three columns and overlaps both.
func overlapDocument() *Document {
	doc := patternDocument(SegmentationStorage, SegmentationTypeBinary)
	doc.Segments = []Segment{{Number: 1}, {Number: 2}, {Number: 3}}
	for segment := 1; segment <= 3; segment++ {
		for slice := 0; slice < 2; slice++ {
			mask := roi.NewRasterMask(patternColumns, patternRows)
			for y := 0; y < patternRows; y++ {
				for x := 0; x < patternColumns; x++ {
					switch segment {
					case 1:
						mask.Set(x, y, (x+y+slice)%3 == 0)
					case 2:
						mask.Set(x, y, (x+y+slice)%3 == 1)
					case 3:
						mask.Set(x, y, x < 3)
					}
				}
			}
			doc.Frames = append(doc.Frames, Frame{SegmentNumber: segment, SliceIndex: slice, Mask: mask})
		}
	}
	return doc
}

func bruteForceOverlap(doc *Document, a, b int) int64 {
	var count int64
	for _, fa := range doc.Frames {
		for _, fb := range doc.Frames {
			if fa.SegmentNumber != a || fb.SegmentNumber != b || fa.SliceIndex != fb.SliceIndex {
				continue
			}
			for y := 0; y < doc.Rows; y++ {
				for x := 0; x < doc.Columns; x++ {
					if fa.Mask.Get(x, y) && fb.Mask.Get(x, y) {
						count++
					}
				}
			}
		}
	}
	return count
}

func TestOverlapsMatchesBruteForce(t *testing.T) {
	doc := overlapDocument()
	report, err := Overlaps(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Segments, []int{1, 2, 3}) {
		t.Fatalf("segments %v", report.Segments)
	}
	if report.Overlapping(1, 2) || !report.Overlapping(3, 1) || !report.Overlapping(2, 3) {
		t.Fatalf("overlaps %+v", report.Overlaps)
	}
	for _, overlap := range report.Overlaps {
		if want := bruteForceOverlap(doc, overlap.First, overlap.Second); overlap.Pixels != want || overlap.Positions != 2 {
			t.Fatalf("%+v, want %d pixels at 2 positions", overlap, want)
		}
	}
	if groups := report.DisjointGroups(); !reflect.DeepEqual(groups, [][]int{{1, 2}, {3}}) {
		t.Fatalf("groups %v", groups)
	}

	// A second frame of segment 1 at slice 0 is united with the first, and
	// the caller's masks are not modified.
	extra := roi.NewRasterMask(patternColumns, patternRows)
	extra.SetRun(0, 0, patternColumns)
	before := doc.Frames[0].Mask.Count()
	doc.Frames = append(doc.Frames, Frame{SegmentNumber: 1, SliceIndex: 0, Mask: extra})
	report, _ = Overlaps(doc)
	if !report.Overlapping(1, 2) || doc.Frames[0].Mask.Count() != before {
		t.Fatalf("united frames: %+v (mask %d→%d)", report.Overlaps, before, doc.Frames[0].Mask.Count())
	}

	file, err := Write(overlapDocument())
	if err != nil {
		t.Fatal(err)
	}
	if got := derivedio.CleanString(file.Dataset, tagSegmentsOverlap); got != SegmentsOverlapYes {
		t.Fatalf("Segments Overlap %q", got)
	}
}

func TestBinaryLabelMapConversion(t *testing.T) {
	// Overlapping segments are refused with the report.
	_, report, err := ToLabelMap(overlapDocument(), LabelMapOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1000.5"})
	if !errors.Is(err, ErrOverlappingSegments) || len(report.Overlaps) != 2 {
		t.Fatalf("overlapping: err=%v report=%+v", err, report)
	}

	disjoint := overlapDocument()
	disjoint.Segments = disjoint.Segments[:2]
	disjoint.Frames = disjoint.Frames[:4]
	labelMap, _, err := ToLabelMap(disjoint, LabelMapOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1000.5"})
	if err != nil {
		t.Fatal(err)
	}
	if len(labelMap.Frames) != 2 || labelMap.Segments[0].Number != 0 || labelMap.Segments[0].Label != "Background" {
		t.Fatalf("label map: %d frames, segments %+v", len(labelMap.Frames), labelMap.Segments)
	}
	file, err := Write(labelMap)
	if err != nil {
		t.Fatal(err)
	}
	read, err := Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if read.LabelMapBitsAllocated != 8 {
		t.Fatalf("Bits Allocated %d", read.LabelMapBitsAllocated)
	}
	binary, numbers, err := ToBinary(read, BinaryOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1000.6"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(numbers, map[int]int{1: 1, 2: 2}) || len(binary.Segments) != 2 {
		t.Fatalf("numbers %v segments %d", numbers, len(binary.Segments))
	}
	// Round trip: every original mask comes back bit for bit.
	for _, original := range disjoint.Frames {
		found := false
		for _, frame := range binary.Frames {
			if frame.SegmentNumber == original.SegmentNumber && frame.SliceIndex == original.SliceIndex {
				found = true
				for y := 0; y < patternRows; y++ {
					if !reflect.DeepEqual(frame.Mask.Runs(y), original.Mask.Runs(y)) {
						t.Fatalf("segment %d slice %d row %d differs", original.SegmentNumber, original.SliceIndex, y)
					}
				}
			}
		}
		if !found {
			t.Fatalf("segment %d slice %d lost", original.SegmentNumber, original.SliceIndex)
		}
	}
	if _, err := Write(binary); err != nil {
		t.Fatal(err)
	}

	// Label-map numbers are renumbered from 1; value 0 may be kept.
	sparse := labelMapPatternDocument(2, 1)
	sparse.Segments[1].Number, sparse.Segments[2].Number = 3, 7
	for i, value := range sparse.Frames[0].LabelMap {
		sparse.Frames[0].LabelMap[i] = []uint16{0, 3, 7}[value]
	}
	_, numbers, err = ToBinary(sparse, BinaryOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1000.7"})
	if err != nil || !reflect.DeepEqual(numbers, map[int]int{3: 1, 7: 2}) {
		t.Fatalf("renumbering: %v %v", numbers, err)
	}
	withZero, numbers, err := ToBinary(sparse, BinaryOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1000.8", IncludeZero: true})
	if err != nil || !reflect.DeepEqual(numbers, map[int]int{0: 1, 3: 2, 7: 3}) || len(withZero.Frames) != 3 {
		t.Fatalf("with zero: %v %v", numbers, err)
	}
	if _, _, err := ToBinary(sparse, BinaryOptions{}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("missing UID: %v", err)
	}
}

func TestReadLimitsLargeFractionalAndLabelMapObjects(t *testing.T) {
	for _, sopClassUID := range []string{SegmentationStorage, LabelMapSegmentationStorage} {
		segmentationType := SegmentationTypeFractional
		if sopClassUID == LabelMapSegmentationStorage {
			segmentationType = SegmentationTypeLabelMap
		}
		dataset := derivedio.Object(
			derivedio.UI(derivedio.TagSOPClassUID, sopClassUID),
			derivedio.CS(tagSegmentationType, segmentationType),
			derivedio.CS(tagSegmentationFractionalType, FractionalTypeProbability),
			derivedio.US(tagMaximumFractionalValue, 255),
			derivedio.US(derivedio.TagRows, 65535), derivedio.US(derivedio.TagColumns, 65535),
			derivedio.IS(derivedio.TagNumberOfFrames, 2),
			derivedio.US(derivedio.TagBitsAllocated, 8),
			derivedio.Raw(derivedio.TagPixelData, core.VROB, []byte{0, 0}),
		)
		if _, err := Read(dataset); !errors.Is(err, ErrResourceLimitExceeded) {
			t.Fatalf("%s: Read error = %v, want ErrResourceLimitExceeded before allocating", segmentationType, err)
		}
	}
}

func BenchmarkOverlapsManySegments(b *testing.B) {
	const size, segments = 512, 200
	doc := &Document{Rows: size, Columns: size}
	for segment := 1; segment <= segments; segment++ {
		mask := roi.NewRasterMask(size, size)
		for y := 0; y < size; y++ {
			start := (segment * 2) % size
			mask.SetRun(y, start, min(size, start+3)) // neighbours share one column
		}
		doc.Segments = append(doc.Segments, Segment{Number: segment})
		doc.Frames = append(doc.Frames, Frame{SegmentNumber: segment, Mask: mask})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Overlaps(doc); err != nil {
			b.Fatal(err)
		}
	}
}
