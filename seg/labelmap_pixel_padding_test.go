package seg

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/roi"
)

// writeReadLabelMap writes doc, optionally edits the encoded data set, and
// reads it back through Part 10 bytes.
func writeReadLabelMap(t *testing.T, doc *Document, edit func(*object.Object)) (*Document, *object.Object) {
	t.Helper()
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if edit != nil {
		edit(file.Dataset)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parsed.Close() })
	back, err := Read(parsed.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	return back, parsed.Dataset
}

func pixelPaddingValue(t *testing.T, obj *object.Object) (core.Element, bool) {
	t.Helper()
	element, ok := obj.Get(tagPixelPaddingValue)
	return element, ok
}

// A label map whose background is not 0 (Sup 243: Pixel Padding Value
// (0028,0120) names it) is written with that Pixel Padding Value and reads
// back with the same background, which no consumer turns into a segment.
func TestLabelMapNonZeroBackgroundRoundTripsThroughPixelPaddingValue(t *testing.T) {
	const background = 7
	binary := patternDocument(SegmentationStorage, SegmentationTypeBinary)
	binary.Rows, binary.Columns = 2, 3
	binary.Segments = []Segment{{Number: 1, Label: "Liver"}, {Number: 2, Label: "Spleen"}}
	liver := roi.NewRasterMask(3, 2)
	liver.Set(0, 0, true)
	spleen := roi.NewRasterMask(3, 2)
	spleen.Set(2, 1, true)
	binary.Frames = []Frame{{SegmentNumber: 1, Mask: liver}, {SegmentNumber: 2, Mask: spleen}}

	mapped, _, err := ToLabelMap(binary, LabelMapOptions{SOPInstanceUID: "2.25.2023001", BackgroundLabel: background})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint16{1, background, background, background, background, 2}
	if mapped.LabelMapBackground != background || mapped.Segments[0].Number != background || len(mapped.Frames) != 1 ||
		!slices.Equal(mapped.Frames[0].LabelMap, want) {
		t.Fatalf("ToLabelMap background %d, segments %+v, frames %+v", mapped.LabelMapBackground, mapped.Segments, mapped.Frames)
	}

	back, dataset := writeReadLabelMap(t, mapped, nil)
	element, ok := pixelPaddingValue(t, dataset)
	if !ok || element.Header.VR != core.VRUS {
		t.Fatalf("Pixel Padding Value = %+v (present %t), want US %d", element, ok, background)
	}
	if values, err := dataset.GetInts(tagPixelPaddingValue); err != nil || !slices.Equal(values, []int64{background}) {
		t.Fatalf("Pixel Padding Value = %v %v, want %d", values, err, background)
	}
	if back.LabelMapBackground != background || !slices.Equal(back.Frames[0].LabelMap, want) {
		t.Fatalf("read background %d, samples %v", back.LabelMapBackground, back.Frames[0].LabelMap)
	}

	extractor, err := NewFrameMaskExtractor(back, ROIOptions{})
	if err != nil {
		t.Fatal(err)
	}
	masks, err := extractor.Masks(back.Frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(masks) != 2 || masks[0].SegmentNumber != 1 || masks[1].SegmentNumber != 2 || masks[0].Mask.Count() != 1 || masks[1].Mask.Count() != 1 {
		t.Fatalf("frame masks = %+v, want segments 1 and 2 of one pixel each", masks)
	}

	roundTrip, numbers, err := ToBinary(back, BinaryOptions{SOPInstanceUID: "2.25.2023002"})
	if err != nil {
		t.Fatal(err)
	}
	if len(roundTrip.Segments) != 2 || numbers[1] != 1 || numbers[2] != 2 || len(roundTrip.Frames) != 2 {
		t.Fatalf("ToBinary segments %+v, mapping %v", roundTrip.Segments, numbers)
	}
	withBackground, numbers, err := ToBinary(back, BinaryOptions{SOPInstanceUID: "2.25.2023003", IncludeBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withBackground.Segments) != 3 || numbers[background] != 3 {
		t.Fatalf("ToBinary with background: segments %+v, mapping %v", withBackground.Segments, numbers)
	}

	segmentation, err := ToROI(back, 0)
	if err != nil {
		t.Fatal(err)
	}
	if mask, ok := segmentation.MaskAt(0); !ok || mask.Count() != 1 || !mask.Get(0, 0) {
		t.Fatalf("ToROI default segment mask = %+v", mask)
	}
}

// The reader takes the background from Pixel Padding Value however the
// object was written, so a value 0 next to it is an ordinary segment; without
// the attribute the background is 0.
func TestReadLabelMapBackgroundFromPixelPaddingValue(t *testing.T) {
	doc := patternDocument(LabelMapSegmentationStorage, SegmentationTypeLabelMap)
	doc.Rows, doc.Columns = 1, 4
	doc.Segments = []Segment{{Number: 0, Label: "Tissue"}, {Number: 1, Label: "Lesion"}, {Number: 9, Label: "Background"}}
	doc.Frames = []Frame{{LabelMap: []uint16{9, 0, 1, 9}}}

	for _, test := range []struct {
		name       string
		edit       func(*object.Object)
		background uint16
		segments   []int
	}{
		{"written as 0, rewritten to 9", func(obj *object.Object) {
			obj.Put(derivedio.US(tagPixelPaddingValue, 9))
		}, 9, []int{0, 1}},
		{"non-negative SS", func(obj *object.Object) {
			obj.Put(core.NewRawElement(tagPixelPaddingValue, core.VRSS, []byte{9, 0}))
		}, 9, []int{0, 1}},
		{"absent", func(obj *object.Object) { obj.Remove(tagPixelPaddingValue) }, 0, []int{1, 9}},
		{"negative SS is dropped", func(obj *object.Object) {
			obj.Put(core.NewRawElement(tagPixelPaddingValue, core.VRSS, []byte{0xFF, 0xFF}))
		}, 0, []int{1, 9}},
	} {
		t.Run(test.name, func(t *testing.T) {
			back, _ := writeReadLabelMap(t, doc, test.edit)
			if back.LabelMapBackground != test.background {
				t.Fatalf("background = %d, want %d", back.LabelMapBackground, test.background)
			}
			extractor, err := NewFrameMaskExtractor(back, ROIOptions{})
			if err != nil {
				t.Fatal(err)
			}
			masks, err := extractor.Masks(back.Frames[0])
			if err != nil {
				t.Fatal(err)
			}
			var numbers []int
			for _, mask := range masks {
				numbers = append(numbers, mask.SegmentNumber)
			}
			slices.Sort(numbers)
			if !slices.Equal(numbers, test.segments) {
				t.Fatalf("segments with masks = %v, want %v", numbers, test.segments)
			}
		})
	}
}

// Short frames are padded with the background, by Write and WriteFrames
// alike, and Pixel Padding Value is written by both.
func TestLabelMapWritersPadWithNonZeroBackground(t *testing.T) {
	for _, profile := range []string{"label8", "label16"} {
		t.Run(profile, func(t *testing.T) {
			d := streamingSEGFixture(profile)
			d.LabelMapBackground = 1
			d.Frames[2].LabelMap = d.Frames[2].LabelMap[:4]
			whole, err := Write(d)
			if err != nil {
				t.Fatal(err)
			}
			defer whole.Close()
			var want, got bytes.Buffer
			if err := object.WriteFile(&want, whole); err != nil {
				t.Fatal(err)
			}
			if _, err := WriteFrames(context.Background(), &got, d, DocumentFrames(d), segStreamLimits()); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatal("WriteFrames and Write differ")
			}
			f, err := object.ReadFile(bytes.NewReader(got.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			back, err := Read(f.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if back.LabelMapBackground != 1 {
				t.Fatalf("background = %d, want 1", back.LabelMapBackground)
			}
			samples := back.Frames[2].LabelMap
			if !slices.Equal(samples[:4], d.Frames[2].LabelMap) {
				t.Fatalf("stored samples = %v, want %v", samples[:4], d.Frames[2].LabelMap)
			}
			for i, sample := range samples[4:] {
				if sample != 1 {
					t.Fatalf("padding sample %d = %d, want background 1", i+4, sample)
				}
			}
		})
	}
}

// Pixel Padding Value is written only for label maps (PS3.3 A.51.4).
func TestBinarySegmentationHasNoPixelPaddingValue(t *testing.T) {
	d := streamingSEGFixture("binary")
	d.LabelMapBackground = 3
	file, err := Write(d)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if element, ok := pixelPaddingValue(t, file.Dataset); ok {
		t.Fatalf("BINARY segmentation carries Pixel Padding Value %+v", element)
	}
}

// Resampling keeps the background out of the segments and a segment 0
// beside a nonzero background in.
func TestResampleLabelMapNonZeroBackground(t *testing.T) {
	doc := &Document{
		SOPClassUID: LabelMapSegmentationStorage, SegmentationType: SegmentationTypeLabelMap,
		Rows: 4, Columns: 4, LabelMapBackground: 5,
		Segments: []Segment{{Number: 0}, {Number: 1}, {Number: 5}},
	}
	for k := 0; k < 3; k++ {
		plane := resampleTestPlane(4, 4, 3*float64(k))
		labels := make([]uint16, 16)
		for i := range labels {
			switch {
			case i%4 == 0:
				labels[i] = 0
			case i%4 == 1:
				labels[i] = 1
			default:
				labels[i] = 5
			}
		}
		doc.Frames = append(doc.Frames, Frame{SliceIndex: k, Geometry: plane, LabelMap: labels})
	}
	r, err := NewResampler(context.Background(), doc, ResampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Resample(context.Background(), r.Grid())
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, segment := range out.Segments {
		numbers = append(numbers, segment.Number)
		if segment.Voxels != 12 {
			t.Fatalf("segment %d has %d voxels, want 12", segment.Number, segment.Voxels)
		}
	}
	if !slices.Equal(numbers, []int{0, 1}) {
		t.Fatalf("resampled segments = %v, want [0 1]", numbers)
	}
	if mask := findResampled(out, 0).Masks[1]; mask == nil || !mask.Get(0, 2) || mask.Get(1, 2) {
		t.Fatalf("segment 0 mask on slice 1 = %+v", mask)
	}
}
