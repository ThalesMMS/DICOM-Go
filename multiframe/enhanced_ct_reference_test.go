package multiframe

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// DCMTK dcmect_overflow (dcmect/tests/t_overflow.cc) attempts to write an
// Enhanced CT whose frame samples are shorter than the declared dimensions.
func TestReferenceEnhancedCTRejectsShortNativePixelData(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*object.Object)
		wantErr error
	}{
		{"matching dimensions", func(*object.Object) {}, nil},
		{"more rows than samples", func(ds *object.Object) {
			ds.Put(derivedio.US(tagRows, concatRows+1))
		}, pixeldata.ErrPixelDataSizeMismatch},
		{"more columns than samples", func(ds *object.Object) {
			ds.Put(derivedio.US(tagColumns, concatColumns+1))
		}, pixeldata.ErrPixelDataSizeMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := enhancedFixture(t, 2)
			test.change(file.Dataset)
			ctx := context.Background()
			reader, err := pixeldata.NewFrameReader(ctx, file, nil, pixeldata.FrameReaderOptions{})
			if test.wantErr != nil {
				if reader != nil || !errors.Is(err, test.wantErr) {
					t.Fatalf("NewFrameReader = %v, %v; want no reader and %v", reader, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			parts, parsed := splitToMemory(t, reader, SplitOptions{MaxFrames: 1})
			checkLogicalFrames(t, checkRoundTrip(t, reader, parts, parsed), 2)
		})
	}
}

// Each instance in DCMTK dcmect_huge_concat retains the Enhanced CT identity
// and image pixel description of the source. Mixing a mismatched instance
// must fail regardless of which file arrives first.
func TestReferenceEnhancedCTConcatenationRejectsMixedInstances(t *testing.T) {
	for _, test := range []struct {
		name    string
		element core.Element
	}{
		{"SOP class", derivedio.UI(tagSOPClassUID, EnhancedMRImageStorageUID)},
		{"study", derivedio.UI(tagStudyInstanceUID, "2.25.8001")},
		{"series", derivedio.UI(tagSeriesInstanceUID, "2.25.8002")},
		{"frame of reference", derivedio.UI(tagFrameOfReferenceUID, "2.25.8003")},
		{"columns", derivedio.US(tagColumns, concatColumns+1)},
		{"samples per pixel", derivedio.US(tagSamplesPerPixel, 3)},
		{"photometric interpretation", derivedio.CS(tagPhotometricInterpretation, "MONOCHROME1")},
		{"bits allocated", derivedio.US(tagBitsAllocated, 8)},
		{"bits stored", derivedio.US(tagBitsStored, 12)},
		{"pixel representation", derivedio.US(tagPixelRepresentation, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := splitForTest(t, enhancedFixture(t, 2), 1)
			files[1].Dataset.Put(test.element)
			for range len(files) {
				logical, err := NewConcatenation(files)
				if logical != nil || !errors.Is(err, ErrConcatenationInconsistent) {
					t.Fatalf("NewConcatenation = %v, %v; want no view and ErrConcatenationInconsistent", logical, err)
				}
				slices.Reverse(files)
			}
		})
	}
}

func TestReferenceEnhancedCTConcatenationRejectsInstanceCountOutsideUS(t *testing.T) {
	// Exercise the part count bound through the metadata planner with no large
	// allocation for samples or per-frame functional groups.
	const frames = 1 << 16
	file := enhancedFixture(t, 1)
	file.Dataset.Put(derivedio.IS(tagNumberOfFrames, frames))
	file.Dataset.Remove(tagPerFrameFunctionalGroupsSequence)
	file.Dataset.Remove(core.TagPixelData)
	lengths := make([]int64, frames)
	for i := range lengths {
		lengths[i] = 24
	}
	opts := SplitOptions{MaxFrames: 1}
	if parts, err := PlanConcatenation(file, lengths, opts); parts != nil || !errors.Is(err, ErrConcatenationSplit) {
		t.Fatalf("PlanConcatenation returned %d parts, %v; want no output and ErrConcatenationSplit", len(parts), err)
	}

	opts.MaxFrames = frames / 2
	parts, err := PlanConcatenation(file, lengths, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Frames != frames/2 || parts[1].FirstFrame != frames/2 || parts[1].Frames != frames/2 {
		t.Fatalf("bounded plan = %+v", parts)
	}
}
