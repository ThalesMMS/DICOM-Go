package seg

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/roi"
)

func TestFrameMaskExtractorLabelMapKeepsDeclaredNonbackgroundLabels(t *testing.T) {
	doc := &Document{
		SOPClassUID: LabelMapSegmentationStorage,
		Rows:        2,
		Columns:     3,
		Segments:    []Segment{{Number: 0}, {Number: 3}, {Number: 700}},
	}
	extractor, err := NewFrameMaskExtractor(doc, ROIOptions{})
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{LabelMap: []uint16{0, 700, 700, 9, 3, 0}}
	got, err := extractor.Masks(frame)
	if err != nil || len(got) != 2 {
		t.Fatalf("Masks = %v, %v; want two masks", got, err)
	}
	if got[0].SegmentNumber != 700 || got[0].Mask.Count() != 2 || !got[0].Mask.Get(1, 0) || !got[0].Mask.Get(2, 0) ||
		got[1].SegmentNumber != 3 || got[1].Mask.Count() != 1 || !got[1].Mask.Get(1, 1) {
		t.Fatalf("Masks = %+v; want labels 700 and 3 at their original positions", got)
	}
	got[0].Mask.Set(1, 0, false)
	if frame.LabelMap[1] != 700 {
		t.Fatal("editing the returned mask changed the stored label samples")
	}
	if _, err := extractor.Masks(Frame{LabelMap: frame.LabelMap[:5]}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("short label frame error = %v, want ErrInvalidObject", err)
	}
}

func TestFrameMaskExtractorFractionalCutoffMatchesWholeVolume(t *testing.T) {
	doc := &Document{
		SegmentationType:   SegmentationTypeFractional,
		MaxFractionalValue: 50,
		Rows:               1,
		Columns:            4,
		Segments:           []Segment{{Number: 3}},
		Frames:             []Frame{{SegmentNumber: 3, Fractional: []uint8{0, 14, 15, 50}}},
	}
	opts := ROIOptions{FractionalThreshold: 0.3}
	cut, err := FractionalCutoff(doc.MaxFractionalValue, opts.FractionalThreshold)
	if err != nil || cut != 15 {
		t.Fatalf("FractionalCutoff = %d, %v; want 15", cut, err)
	}
	extractor, err := NewFrameMaskExtractor(doc, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := extractor.Masks(doc.Frames[0])
	if err != nil || len(got) != 1 {
		t.Fatalf("Masks = %+v, %v; want one mask", got, err)
	}
	volume, err := ToROIWithOptions(doc, 3, opts)
	if err != nil {
		t.Fatal(err)
	}
	for x, sample := range doc.Frames[0].Fractional {
		want := int(sample) >= cut
		if got[0].Mask.Get(x, 0) != want || volume.Voxel(x, 0, 0) != want {
			t.Errorf("sample %d: frame and volume inclusion must both be %v", sample, want)
		}
	}
	if empty, err := extractor.Masks(Frame{SegmentNumber: 3}); err != nil || len(empty) != 0 {
		t.Fatalf("empty fractional frame = %v, %v; want no masks and no error", empty, err)
	}
	if _, err := extractor.Masks(Frame{SegmentNumber: 3, Fractional: []uint8{50}}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("short fractional frame error = %v, want ErrInvalidObject", err)
	}
}

func TestFractionalCutoffRejectsInvalidInputs(t *testing.T) {
	for _, tt := range []struct {
		max       int
		threshold float64
	}{
		{0, 0.5},
		{256, 0.5},
		{255, -0.1},
		{255, 1.1},
		{255, math.NaN()},
		{255, math.Inf(1)},
	} {
		if _, err := FractionalCutoff(tt.max, tt.threshold); !errors.Is(err, ErrInvalidObject) {
			t.Errorf("FractionalCutoff(%d, %v) error = %v, want ErrInvalidObject", tt.max, tt.threshold, err)
		}
	}
	if cut, err := FractionalCutoff(255, 0); err != nil || cut != 128 {
		t.Fatalf("default cutoff = %d, %v; want 128", cut, err)
	}
	if cut, err := FractionalCutoff(255, 1e-15); err != nil || cut != 1 {
		t.Fatalf("small positive cutoff = %d, %v; want 1", cut, err)
	}
}

func TestFrameMaskExtractorBorrowsBinaryMaskAndRejectsWrongDimensions(t *testing.T) {
	doc := &Document{Rows: 2, Columns: 2}
	extractor, err := NewFrameMaskExtractor(doc, ROIOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mask := roi.NewRasterMask(2, 2)
	mask.Set(1, 1, true)
	got, err := extractor.Masks(Frame{Mask: mask, SegmentNumber: 2})
	if err != nil || len(got) != 1 || got[0].Mask != mask || got[0].SegmentNumber != 2 {
		t.Fatalf("Masks = %+v, %v; want borrowed segment 2 mask", got, err)
	}
	if _, err := extractor.Masks(Frame{Mask: roi.NewRasterMask(1, 2), SegmentNumber: 2}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("mismatched BINARY mask error = %v, want ErrInvalidObject", err)
	}
	if _, err := NewFrameMaskExtractor(&Document{Rows: math.MaxInt, Columns: 2}, ROIOptions{}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("overflowing dimensions error = %v, want ErrInvalidObject", err)
	}
}
