package seg

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/roi"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// truncatePixelData replaces the Pixel Data of dataset with its first n bytes.
func truncatePixelData(t *testing.T, dataset *object.Object, n int) {
	t.Helper()
	element, ok := dataset.Get(derivedio.TagPixelData)
	if !ok {
		t.Fatal("no Pixel Data")
	}
	raw, _ := element.RawBytes()
	dataset.Put(derivedio.Raw(derivedio.TagPixelData, element.VR(), append([]byte(nil), raw[:n]...)))
}

func binaryPatternDocument(frames int) *Document {
	doc := patternDocument(SegmentationStorage, SegmentationTypeBinary)
	doc.Segments = []Segment{{Number: 1, Label: "A"}}
	for slice := 0; slice < frames; slice++ {
		mask := roi.NewRasterMask(patternColumns, patternRows)
		for i := 0; i < patternRows*patternColumns; i++ {
			if (i*7+slice)%3 == 0 {
				mask.Set(i%patternColumns, i/patternColumns, true)
			}
		}
		doc.Frames = append(doc.Frames, Frame{SegmentNumber: 1, SliceIndex: slice, Mask: mask})
	}
	return doc
}

func fractionalPatternDocument(frames int) *Document {
	doc := patternDocument(SegmentationStorage, SegmentationTypeFractional)
	doc.FractionalType, doc.MaxFractionalValue = FractionalTypeProbability, 255
	doc.Segments = []Segment{{Number: 1, Label: "A"}}
	for slice := 0; slice < frames; slice++ {
		samples := make([]uint8, patternRows*patternColumns)
		for i := range samples {
			samples[i] = fractionalPattern(1, slice, i, 255) | 1
		}
		doc.Frames = append(doc.Frames, Frame{SegmentNumber: 1, SliceIndex: slice, Fractional: samples})
	}
	return doc
}

// TestReadRejectsTruncatedPixelData checks the #1024 contract for every
// sample layout: Read fails with ErrTruncatedPixelData, and the opt-in
// tolerant read keeps the complete frames, zero-fills the missing samples and
// reports the incomplete frames.
func TestReadRejectsTruncatedPixelData(t *testing.T) {
	const pixels = patternRows * patternColumns // 35: 1-bit frames straddle bytes
	for _, tc := range []struct {
		name string
		doc  *Document
		// keep is the number of Pixel Data bytes kept and incomplete the
		// frames it leaves incomplete.
		keep       int
		incomplete []int
		expected   int64
	}{
		// 3 BINARY frames need ceil(105/8) = 14 bytes; 9 bytes (72 bits) hold
		// frame 0 (bits 0-34) and frame 1 (35-69), not frame 2; 13 bytes miss
		// only the last bit of frame 2.
		{"binary", binaryPatternDocument(3), 9, []int{2}, 14},
		{"binary one bit short", binaryPatternDocument(3), 13, []int{2}, 14},
		{"binary empty", binaryPatternDocument(3), 0, []int{0, 1, 2}, 14},
		{"fractional", fractionalPatternDocument(3), 2*pixels + 10, []int{2}, 3 * pixels},
		{"labelmap 8", labelMapPatternDocument(200, 3), pixels + 1, []int{1, 2}, 3 * pixels},
		{"labelmap 16", labelMapPatternDocument(300, 3), 2*pixels*2 + 6, []int{2}, 3 * pixels * 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := Write(tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			complete, err := Read(file.Dataset)
			if err != nil {
				t.Fatalf("complete Pixel Data: %v", err)
			}
			truncatePixelData(t, file.Dataset, tc.keep)

			if _, err := Read(file.Dataset); !errors.Is(err, ErrTruncatedPixelData) {
				t.Fatalf("Read: %v, want ErrTruncatedPixelData", err)
			}
			_, report, err := ReadWithOptions(file.Dataset, ReadOptions{})
			if !errors.Is(err, ErrTruncatedPixelData) || !reflect.DeepEqual(report.IncompleteFrames, tc.incomplete) {
				t.Fatalf("strict ReadWithOptions: %v, report %+v", err, report)
			}

			got, report, err := ReadWithOptions(file.Dataset, ReadOptions{AllowTruncatedPixelData: true})
			if err != nil {
				t.Fatal(err)
			}
			want := ReadReport{PixelDataBytes: int64(tc.keep), ExpectedPixelDataBytes: tc.expected, IncompleteFrames: tc.incomplete}
			if !reflect.DeepEqual(report, want) || !report.Truncated() {
				t.Fatalf("report %+v, want %+v", report, want)
			}
			if len(got.Frames) != len(complete.Frames) {
				t.Fatalf("%d frames, want %d", len(got.Frames), len(complete.Frames))
			}
			// Every sample either matches the complete read (it was present)
			// or is zero (it was missing); complete frames match in full.
			for i := range got.Frames {
				gotValues, wantValues := frameSamples(got, got.Frames[i]), frameSamples(complete, complete.Frames[i])
				if len(gotValues) != pixels {
					t.Fatalf("frame %d has %d samples, want %d", i, len(gotValues), pixels)
				}
				incomplete := containsInt(tc.incomplete, i)
				for p := range gotValues {
					if gotValues[p] != wantValues[p] && (!incomplete || gotValues[p] != 0) {
						t.Fatalf("frame %d sample %d = %d, want %d or a zero fill", i, p, gotValues[p], wantValues[p])
					}
				}
			}
		})
	}
}

// frameSamples returns a frame's value at every pixel: 0/1 for BINARY, the
// sample for FRACTIONAL and the label for a label map.
func frameSamples(doc *Document, frame Frame) []int {
	pixels := doc.Rows * doc.Columns
	values := make([]int, pixels)
	switch {
	case doc.SOPClassUID == LabelMapSegmentationStorage:
		for i := 0; i < pixels && i < len(frame.LabelMap); i++ {
			values[i] = int(frame.LabelMap[i])
		}
		if frame.LabelMap != nil && len(frame.LabelMap) != pixels {
			return nil // a short frame fails the caller's length check
		}
	case doc.SegmentationType == SegmentationTypeFractional:
		for i := 0; i < pixels && i < len(frame.Fractional); i++ {
			values[i] = int(frame.Fractional[i])
		}
	default:
		frame.Mask.ForEachPixel(func(x, y int) { values[y*doc.Columns+x] = 1 })
	}
	return values
}

func containsInt(values []int, v int) bool {
	for _, value := range values {
		if value == v {
			return true
		}
	}
	return false
}

// TestReadLabelMapBigEndian reads label maps written in Explicit VR Big
// Endian: 16-bit labels are Big Endian words, and 8-bit labels in an OW
// value are byte-swapped within each word (PS3.5 8.1.1).
func TestReadLabelMapBigEndian(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxLabel int
		vr       core.VR
	}{
		{"16-bit OW", 300, core.VROW},
		{"8-bit OB", 200, core.VROB},
		{"8-bit OW", 200, core.VROW},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := labelMapPatternDocument(tc.maxLabel, 3)
			file, err := Write(doc)
			if err != nil {
				t.Fatal(err)
			}
			element, _ := file.Dataset.Get(derivedio.TagPixelData)
			raw, _ := element.RawBytes()
			file.Dataset.Put(derivedio.Raw(derivedio.TagPixelData, tc.vr, append([]byte(nil), raw...)))
			bigEndian, _, err := pixeldata.TranscodeDataSet(context.Background(), file.Dataset, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var encoded bytes.Buffer
			if err := object.WriteDataSet(&encoded, bigEndian, transfer.ExplicitVRBigEndian); err != nil {
				t.Fatal(err)
			}
			back, err := object.ReadDataSet(&encoded, transfer.ExplicitVRBigEndian)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Read(back)
			if err != nil {
				t.Fatal(err)
			}
			for i := range doc.Frames {
				if !reflect.DeepEqual(got.Frames[i].LabelMap, doc.Frames[i].LabelMap) {
					t.Fatalf("frame %d labels %v, want %v", i, got.Frames[i].LabelMap[:6], doc.Frames[i].LabelMap[:6])
				}
			}
		})
	}
}
