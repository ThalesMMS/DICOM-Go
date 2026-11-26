// Package decoderecovery carries opt-in pixel decode recoveries in a
// context so pixeldata options reach codecs and the encapsulated frame
// planner without an import cycle.
package decoderecovery

import "context"

// Options lists the opt-in recoveries. The zero value is strict.
type Options struct {
	IgnoreInconsistentBasicOffsetTable bool
	JPEGLosslessEncoderQuirks          bool
	JPEGLosslessPredictor6Overflow     bool
	RLESegmentOverrun                  bool
	RLEReversedByteSegments            bool
	JPEGSequentialScanQuirks           bool
	JPEGLSJAIThresholds                bool
	JPEGLSHPColorTransforms            bool
}

type key struct{}

// With returns ctx carrying o.
func With(ctx context.Context, o Options) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, key{}, o)
}

// From returns the options carried by ctx (strict if none).
func From(ctx context.Context) Options {
	if ctx == nil {
		return Options{}
	}
	o, _ := ctx.Value(key{}).(Options)
	return o
}
