package seg

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/roi"
)

// DCMTK dcmseg_labelmap_bg_custom_codes and dcmseg_labelmap_bg_single_invariant
// exercise the distinction between the background description and foreground
// segments. This fixture uses the supported zero-valued LABELMAP background.
func TestReferenceLabelMapBackgroundCodesAndBinarySelection(t *testing.T) {
	background := Segment{
		Number: 31, Label: "Unassigned tissue", AlgorithmType: AlgorithmManual,
		SegmentedPropertyCategory: CodedEntry{CodeValue: "SYN.BG", CodingSchemeDesignator: "99SYN", CodeMeaning: "Fixture category"},
		SegmentedPropertyType:     CodedEntry{CodeValue: "SYN.01", CodingSchemeDesignator: "99SYN", CodeMeaning: "Fixture background"},
	}
	for _, test := range []struct {
		name     string
		label    uint16
		wantBits int
	}{
		{"eight-bit background and foreground", 31, 8},
		{"sixteen-bit maximum foreground label", 65535, 16},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := patternDocument(SegmentationStorage, SegmentationTypeBinary)
			doc.Rows, doc.Columns = 2, 3
			doc.Segments = []Segment{{Number: int(test.label), Label: "Foreground"}}
			mask := roi.NewRasterMask(doc.Columns, doc.Rows)
			mask.Set(1, 0, true)
			mask.Set(0, 1, true)
			doc.Frames = []Frame{{SegmentNumber: int(test.label), Mask: mask}}
			mapped, _, err := ToLabelMap(doc, LabelMapOptions{SOPInstanceUID: "2.25.8080", Background: &background})
			if err != nil {
				t.Fatal(err)
			}
			if background.Number != 31 {
				t.Fatalf("conversion changed caller's background number to %d", background.Number)
			}

			file, err := Write(mapped)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var encoded bytes.Buffer
			if err := object.WriteFile(&encoded, file); err != nil {
				t.Fatal(err)
			}
			parsed, err := object.ReadFile(&encoded)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.Close()
			back, err := Read(parsed.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if back.LabelMapBitsAllocated != test.wantBits || len(back.Segments) != 2 || len(back.Frames) != 1 {
				t.Fatalf("LABELMAP read back with %d bits, %d segments and %d frames", back.LabelMapBitsAllocated, len(back.Segments), len(back.Frames))
			}
			bg, fg := back.Segments[0], back.Segments[1]
			if bg.Number != 0 || bg.Label != background.Label || bg.AlgorithmType != background.AlgorithmType ||
				bg.SegmentedPropertyCategory != background.SegmentedPropertyCategory || bg.SegmentedPropertyType != background.SegmentedPropertyType ||
				fg.Number != int(test.label) || fg.Label != "Foreground" {
				t.Fatalf("read background %+v and foreground %+v", bg, fg)
			}
			wantSamples := []uint16{0, test.label, 0, test.label, 0, 0}
			if !slices.Equal(back.Frames[0].LabelMap, wantSamples) {
				t.Fatalf("samples = %v, want %v", back.Frames[0].LabelMap, wantSamples)
			}

			for _, selection := range []struct {
				name        string
				includeZero bool
				wantLabels  []uint16
			}{
				{"foreground only", false, []uint16{test.label}},
				{"background and foreground", true, []uint16{0, test.label}},
			} {
				t.Run(selection.name, func(t *testing.T) {
					binary, numbers, err := ToBinary(back, BinaryOptions{SOPInstanceUID: "2.25.8081", IncludeZero: selection.includeZero})
					if err != nil {
						t.Fatal(err)
					}
					if len(binary.Frames) != len(selection.wantLabels) || len(numbers) != len(selection.wantLabels) {
						t.Fatalf("binary frames = %d, source label mapping = %v", len(binary.Frames), numbers)
					}
					for i, label := range selection.wantLabels {
						frame := binary.Frames[i]
						if frame.SegmentNumber != i+1 || numbers[int(label)] != frame.SegmentNumber {
							t.Fatalf("source label %d mapped to frame %d with mapping %v", label, frame.SegmentNumber, numbers)
						}
						for index, sample := range wantSamples {
							if got := frame.Mask.Get(index%doc.Columns, index/doc.Columns); got != (sample == label) {
								t.Fatalf("label %d pixel %d mask bit = %t; sample = %d", label, index, got, sample)
							}
						}
					}
				})
			}
		})
	}
}

func TestReferenceLabelMapRejectsAmbiguousBackgroundAndUnknownLabels(t *testing.T) {
	for _, test := range []struct {
		name     string
		segments []Segment
		pixels   []uint16
	}{
		{"duplicate declared zero", []Segment{{Number: 0}, {Number: 0}}, []uint16{0}},
		{"used nonzero without description", []Segment{{Number: 0}, {Number: 3}}, []uint16{2}},
		{"implicit zero without description", []Segment{{Number: 3}}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := patternDocument(LabelMapSegmentationStorage, SegmentationTypeLabelMap)
			doc.Rows, doc.Columns = 1, 1
			doc.Segments = test.segments
			doc.Frames = []Frame{{LabelMap: test.pixels}}
			if file, err := Write(doc); file != nil || !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("Write = %v, %v; want no object and ErrInvalidObject", file, err)
			}
		})
	}
}
