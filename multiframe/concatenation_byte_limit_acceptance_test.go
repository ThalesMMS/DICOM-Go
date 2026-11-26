package multiframe

import (
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

func TestReferenceAcceptanceConcatenationByteLimitDoesNotOverflow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		frameLengths []int64
		wantFrames   []int
	}{
		{"exact limit", []int64{math.MaxInt64 - 1, 1}, []int{2}},
		{"huge frame followed by small frame", []int64{math.MaxInt64, 1}, []int{1, 1}},
		{"small frame followed by huge frame", []int64{1, math.MaxInt64}, []int{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// PlanConcatenation only needs the declared frame sizes and metadata.
			source := enhancedFixture(t, len(tc.frameLengths))
			source.Dataset.Remove(core.TagPixelData)
			parts, err := PlanConcatenation(source, tc.frameLengths, SplitOptions{
				MaxPixelBytes: math.MaxInt64,
			})
			if err != nil || len(parts) != len(tc.wantFrames) {
				t.Fatalf("planning frame lengths %v with byte limit %d returned %d parts and error %v; want part frame counts %v",
					tc.frameLengths, int64(math.MaxInt64), len(parts), err, tc.wantFrames)
			}
			firstFrame := 0
			for i, want := range tc.wantFrames {
				part := parts[i]
				if part.Number != i+1 || part.FirstFrame != firstFrame || part.Frames != want {
					t.Fatalf("part %d: number %d, first frame %d, frame count %d; want %d, %d, %d",
						i, part.Number, part.FirstFrame, part.Frames, i+1, firstFrame, want)
				}
				firstFrame += want
			}
		})
	}
}
