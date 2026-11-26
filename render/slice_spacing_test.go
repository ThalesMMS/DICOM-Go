package render

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func spacingTestFrame(z, thickness, spacingBetweenSlices float64) *Frame {
	return &Frame{
		Metadata: pixeldata.Metadata{
			Rows: 2, Columns: 2, SamplesPerPixel: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7,
			NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2",
		},
		PixelBytes:           []byte{1, 2, 3, 4},
		ImagePosition:        []float64{0, 0, z},
		ImageOrientation:     []float64{1, 0, 0, 0, 1, 0},
		PixelSpacing:         []float64{1, 1},
		SliceThickness:       thickness,
		SpacingBetweenSlices: spacingBetweenSlices,
	}
}

// TestVolumeSliceSpacingPrecedence: positions measure the spacing when they
// can; a single slice takes Spacing Between Slices before Slice Thickness
// (the render half of ITK 9467d94115) and reports the source.
func TestVolumeSliceSpacingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stack      *Stack
		want       float64
		wantSource object.SliceSpacingSource
	}{
		{
			name:       "positions win over both attributes",
			stack:      &Stack{Frames: []*Frame{spacingTestFrame(0, 5, 3), spacingTestFrame(2, 5, 3)}},
			want:       2,
			wantSource: object.SliceSpacingFromPositions,
		},
		{
			name:       "single slice uses frame spacing between slices",
			stack:      &Stack{Frames: []*Frame{spacingTestFrame(0, 5, 3)}},
			want:       3,
			wantSource: object.SliceSpacingFromSpacingBetweenSlices,
		},
		{
			name:       "single slice uses series spacing between slices",
			stack:      &Stack{SliceThickness: 5, SpacingBetweenSlices: 4, Frames: []*Frame{spacingTestFrame(0, 5, 0)}},
			want:       4,
			wantSource: object.SliceSpacingFromSpacingBetweenSlices,
		},
		{
			name:       "negative spacing between slices sizes by magnitude",
			stack:      &Stack{Frames: []*Frame{spacingTestFrame(0, 5, -3.5)}},
			want:       3.5,
			wantSource: object.SliceSpacingFromSpacingBetweenSlices,
		},
		{
			name:       "single slice falls back to thickness",
			stack:      &Stack{Frames: []*Frame{spacingTestFrame(0, 5, 0)}},
			want:       5,
			wantSource: object.SliceSpacingFromSliceThickness,
		},
		{
			name:       "single slice default",
			stack:      &Stack{Frames: []*Frame{spacingTestFrame(0, 0, 0)}},
			want:       1,
			wantSource: object.SliceSpacingDefault,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volume, err := BuildVolume(tc.stack)
			if err != nil {
				t.Fatalf("BuildVolume: %v", err)
			}
			defer volume.Close()
			if volume.SliceSpacing != tc.want || volume.SliceSpacingSource() != tc.wantSource {
				t.Fatalf("SliceSpacing = %v from %q, want %v from %q", volume.SliceSpacing, volume.SliceSpacingSource(), tc.want, tc.wantSource)
			}
			if got := seriesSliceSpacing(tc.stack); got != tc.want {
				t.Fatalf("seriesSliceSpacing = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVolumeCandidateStackKeepsSpacingBetweenSlices: a sub-volume keeps the
// spacing attribute its fallback needs.
func TestVolumeCandidateStackKeepsSpacingBetweenSlices(t *testing.T) {
	stack := &Stack{SpacingBetweenSlices: 7, Frames: []*Frame{spacingTestFrame(0, 5, 3)}}
	candidates := stack.VolumeCandidates()
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	if sub := stack.VolumeCandidateStack(candidates[0]); sub.SpacingBetweenSlices != 3 {
		t.Fatalf("sub-stack SpacingBetweenSlices = %v, want the first frame's 3", sub.SpacingBetweenSlices)
	}
}
