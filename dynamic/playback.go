package dynamic

import (
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagRecommendedDisplayFrameRate = core.NewTag(0x0008, 0x2144)
	tagCineRate                    = core.NewTag(0x0018, 0x0040)
)

// PlaybackSource names the attribute a PlaybackTiming was derived from.
type PlaybackSource string

const (
	PlaybackUnknown                     PlaybackSource = ""
	PlaybackRecommendedDisplayFrameRate PlaybackSource = "RecommendedDisplayFrameRate"
	PlaybackFrameTimeVector             PlaybackSource = "FrameTimeVector"
	PlaybackFrameTime                   PlaybackSource = "FrameTime"
	PlaybackCineRate                    PlaybackSource = "CineRate"
)

// maxPlaybackInterval bounds one frame's display interval; longer declared
// intervals (acquisition timing rather than a cine cadence) are not used.
const maxPlaybackInterval = 10 * time.Second

// PlaybackTiming is the cine display cadence of a multi-frame object (#1243).
// Intervals[i] is how long frame i stays on screen before the next frame.
type PlaybackTiming struct {
	Intervals []time.Duration
	Source    PlaybackSource
}

// Known reports whether the object declares a usable cadence.
func (p PlaybackTiming) Known() bool { return len(p.Intervals) > 0 }

// ReadPlaybackTiming returns the display cadence of file without touching
// Pixel Data. Precedence: Recommended Display Frame Rate (0008,2144), the
// explicit playback recommendation; Frame Time Vector (0018,1065), whose
// increments give an irregular cadence; Frame Time (0018,1063); Cine Rate
// (0018,0040). Invalid or out-of-range declarations are skipped in favour of
// the next source; a zero PlaybackTiming means the caller's default applies.
// Actual Frame Duration is acquisition timing (for example minutes-long PET
// frames), not a display cadence, and is not used.
func ReadPlaybackTiming(file *object.File) PlaybackTiming {
	if file == nil || file.Dataset == nil {
		return PlaybackTiming{}
	}
	obj := file.Dataset
	frames := derivedio.Int(obj, tagNumberOfFrames)
	if frames <= 0 {
		frames = 1
	}
	if frames > 1_000_000 {
		return PlaybackTiming{}
	}
	uniform := func(interval time.Duration, source PlaybackSource) (PlaybackTiming, bool) {
		if interval <= 0 || interval > maxPlaybackInterval {
			return PlaybackTiming{}, false
		}
		intervals := make([]time.Duration, frames)
		for i := range intervals {
			intervals[i] = interval
		}
		return PlaybackTiming{Intervals: intervals, Source: source}, true
	}
	if rate, ok := directInt(obj, tagRecommendedDisplayFrameRate); ok && rate > 0 {
		if timing, ok := uniform(time.Second/time.Duration(rate), PlaybackRecommendedDisplayFrameRate); ok {
			return timing
		}
	}
	if timing, ok := frameTimeVectorTiming(directFloats(obj, tagFrameTimeVector), frames); ok {
		return timing
	}
	if milliseconds, ok := directMilliseconds(obj, tagFrameTime); ok {
		if interval, ok := millisecondsDuration(milliseconds); ok {
			if timing, ok := uniform(interval, PlaybackFrameTime); ok {
				return timing
			}
		}
	}
	if rate, ok := directInt(obj, tagCineRate); ok && rate > 0 {
		if timing, ok := uniform(time.Second/time.Duration(rate), PlaybackCineRate); ok {
			return timing
		}
	}
	return PlaybackTiming{}
}

// frameTimeVectorTiming converts Frame Time Vector increments (the first is 0,
// increment i is the time from frame i-1 to frame i) into display intervals:
// frame i is shown for increment i+1, and the last frame repeats the last
// increment.
func frameTimeVectorTiming(vector []float64, frames int) (PlaybackTiming, bool) {
	if frames < 2 || len(vector) != frames || vector[0] != 0 {
		return PlaybackTiming{}, false
	}
	intervals := make([]time.Duration, frames)
	for i := 1; i < frames; i++ {
		interval, ok := millisecondsDuration(vector[i])
		if !ok || interval <= 0 || interval > maxPlaybackInterval {
			return PlaybackTiming{}, false
		}
		intervals[i-1] = interval
	}
	intervals[frames-1] = intervals[frames-2]
	return PlaybackTiming{Intervals: intervals, Source: PlaybackFrameTimeVector}, true
}
