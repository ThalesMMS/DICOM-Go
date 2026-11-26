package dynamic

import (
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
)

func TestReadFrameMetadataPlaybackCadenceDoesNotDeclareActualDuration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timing   core.Element
		duration []time.Duration
	}{
		{"frame time", dynamicStrings(tagFrameTime, core.VRDS, "250"), []time.Duration{250 * time.Millisecond, 250 * time.Millisecond, 250 * time.Millisecond}},
		{"frame time vector", dynamicStrings(tagFrameTimeVector, core.VRDS, "0", "1000", "1500"), []time.Duration{time.Second, 1500 * time.Millisecond, 1500 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := playbackFile(dynamicStrings(tagNumberOfFrames, core.VRIS, "3"), tc.timing)
			frames, err := ReadFrameMetadata(file)
			if err != nil {
				t.Fatal(err)
			}
			if len(frames) != len(tc.duration) {
				t.Fatalf("frame count = %d, want %d", len(frames), len(tc.duration))
			}
			for i, frame := range frames {
				if !frame.HasDuration || frame.Duration != tc.duration[i] {
					t.Fatalf("frame %d legacy duration = %v (%v), want %v", i, frame.Duration, frame.HasDuration, tc.duration[i])
				}
				if frame.HasActualDuration || frame.ActualDuration != 0 {
					t.Fatalf("frame %d playback cadence qualified as actual duration: %+v", i, frame)
				}
			}
		})
	}
}

func TestReadFrameMetadataEnhancedActualDurationPreservesEachFrame(t *testing.T) {
	file := playbackFile(
		dynamicStrings(tagNumberOfFrames, core.VRIS, "2"),
		dynamicStrings(tagFrameTime, core.VRDS, "250"),
		dynamicStrings(tagActualFrameDuration, core.VRIS, "900"),
		dynamicSequence(tagPerFrameFunctionalGroups,
			core.DataSet{Elements: []core.Element{
				dynamicSequence(tagFrameContentSequence, core.DataSet{Elements: []core.Element{
					dynamicUL(tagTemporalPositionIndex, 1),
					dynamicStrings(tagActualFrameDuration, core.VRIS, "500"),
				}}),
			}},
			core.DataSet{Elements: []core.Element{
				dynamicSequence(tagFrameContentSequence, core.DataSet{Elements: []core.Element{
					dynamicUL(tagTemporalPositionIndex, 2),
					dynamicStrings(tagActualFrameDuration, core.VRIS, "750"),
				}}),
			}},
		),
	)
	frames, err := ReadFrameMetadata(file)
	if err != nil {
		t.Fatal(err)
	}
	timeline, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || len(timeline.Points) != 2 {
		t.Fatalf("frames/points = %d/%d, want 2/2", len(frames), len(timeline.Points))
	}
	for i, want := range []time.Duration{500 * time.Millisecond, 750 * time.Millisecond} {
		frame := frames[i]
		if !frame.HasActualDuration || frame.ActualDuration != want || !frame.HasDuration || frame.Duration != want {
			t.Fatalf("frame %d declared duration = %+v, want %v", i, frame, want)
		}
		if !timeline.Points[i].HasDuration || timeline.Points[i].Duration != want {
			t.Fatalf("point %d legacy duration = %+v, want %v", i, timeline.Points[i], want)
		}
	}
}

func TestReadFrameMetadataInvalidActualDurationIsNotQualified(t *testing.T) {
	file := playbackFile(dynamicStrings(tagActualFrameDuration, core.VRIS, "-1"))
	frames, err := ReadFrameMetadata(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].HasActualDuration || frames[0].HasDuration || frames[0].ActualDuration != 0 {
		t.Fatalf("invalid declaration qualified as actual duration: %+v", frames)
	}
}
