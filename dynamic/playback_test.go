package dynamic

import (
	"reflect"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func playbackFile(elements ...core.Element) *object.File {
	return &object.File{Dataset: object.FromElements(elements, std.Dictionary), TransferSyntax: transfer.ImplicitVRLittleEndian}
}

func TestReadPlaybackTiming(t *testing.T) {
	ms := time.Millisecond
	frames := dynamicStrings(tagNumberOfFrames, core.VRIS, "4")
	vector := dynamicStrings(tagFrameTimeVector, core.VRDS, "0", "40", "60", "33.3")
	frameTime := dynamicStrings(tagFrameTime, core.VRDS, "50")
	for _, tc := range []struct {
		name     string
		elements []core.Element
		want     PlaybackTiming
	}{
		{"recommended rate wins", []core.Element{frames, vector, dynamicStrings(tagRecommendedDisplayFrameRate, core.VRIS, "25")},
			PlaybackTiming{Intervals: []time.Duration{40 * ms, 40 * ms, 40 * ms, 40 * ms}, Source: PlaybackRecommendedDisplayFrameRate}},
		{"irregular frame time vector", []core.Element{frames, vector, frameTime},
			PlaybackTiming{Intervals: []time.Duration{40 * ms, 60 * ms, 33300 * time.Microsecond, 33300 * time.Microsecond}, Source: PlaybackFrameTimeVector}},
		{"frame time", []core.Element{frames, frameTime},
			PlaybackTiming{Intervals: []time.Duration{50 * ms, 50 * ms, 50 * ms, 50 * ms}, Source: PlaybackFrameTime}},
		{"cine rate", []core.Element{frames, dynamicStrings(tagCineRate, core.VRIS, "10")},
			PlaybackTiming{Intervals: []time.Duration{100 * ms, 100 * ms, 100 * ms, 100 * ms}, Source: PlaybackCineRate}},
		{"invalid vector falls back to frame time", []core.Element{frames, dynamicStrings(tagFrameTimeVector, core.VRDS, "0", "40", "-1", "40"), frameTime},
			PlaybackTiming{Intervals: []time.Duration{50 * ms, 50 * ms, 50 * ms, 50 * ms}, Source: PlaybackFrameTime}},
		{"acquisition-length frame time is not a cadence", []core.Element{frames, dynamicStrings(tagFrameTime, core.VRDS, "60000")},
			PlaybackTiming{}},
		{"zero rate is ignored", []core.Element{frames, dynamicStrings(tagRecommendedDisplayFrameRate, core.VRIS, "0"), frameTime},
			PlaybackTiming{Intervals: []time.Duration{50 * ms, 50 * ms, 50 * ms, 50 * ms}, Source: PlaybackFrameTime}},
		{"no timing", []core.Element{frames}, PlaybackTiming{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ReadPlaybackTiming(playbackFile(tc.elements...))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ReadPlaybackTiming = %+v, want %+v", got, tc.want)
			}
			if got.Known() != (len(tc.want.Intervals) > 0) {
				t.Fatalf("Known() = %v", got.Known())
			}
		})
	}
	if got := ReadPlaybackTiming(nil); got.Known() {
		t.Fatalf("nil file timing = %+v", got)
	}
}
