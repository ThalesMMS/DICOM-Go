package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// tagTranscoder rewrites each JPEG Baseline frame as "JXL" + frame (and
// back), standing in for a real codestream-level conversion.
type tagTranscoder struct {
	calls  int
	frames int // frames to return; 0 means one per source fragment
	panics bool
}

func (*tagTranscoder) CanTranscodeCodestream(source, target string) bool {
	return source == transfer.JPEGBaseline.UID && target == transfer.JPEGXLJPEGRecompression.UID
}

func (c *tagTranscoder) TranscodeCodestreams(_ context.Context, _, _ string, pixel pixeldata.PixelData, _ *object.Object) ([][]byte, error) {
	c.calls++
	if c.panics {
		panic("synthetic transcoder fault")
	}
	var out [][]byte
	for _, fragment := range pixel.Sequence.Fragments {
		out = append(out, append([]byte("JXL"), fragment...))
	}
	if c.frames > 0 {
		out = out[:c.frames-1]
	}
	return out, nil
}

func TestCodestreamTranscoderRewritesFramesWithoutDecoding(t *testing.T) {
	source := codecfixture.JPEGBaselineSmall()
	transcoder := &tagTranscoder{}
	opts := pixeldata.TranscodeOptions{CodestreamTranscoder: transcoder}
	out, report, err := pixeldata.TranscodeDataSet(context.Background(), source.Object(), transfer.JPEGBaseline, transfer.JPEGXLJPEGRecompression, opts)
	if err != nil {
		t.Fatal(err)
	}
	if transcoder.calls != 1 || report.Lossy || report.Frames != 1 || report.EncodedBytes <= 0 {
		t.Fatalf("calls=%d report=%+v", transcoder.calls, report)
	}
	in, err := pixeldata.ExtractView(source.Object())
	if err != nil {
		t.Fatal(err)
	}
	got, err := pixeldata.ExtractView(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sequence.Fragments) != 1 || !bytes.Equal(got.Sequence.Fragments[0], append([]byte("JXL"), in.Sequence.Fragments[0]...)) {
		t.Fatal("output fragments are not the transcoder's frames")
	}
	wantMeta, _ := pixeldata.ExtractMetadata(source.Object())
	if gotMeta, _ := pixeldata.ExtractMetadata(out); gotMeta != wantMeta {
		t.Fatalf("metadata changed: %+v, want %+v", gotMeta, wantMeta)
	}

	// Other pairs and ForceReencode take the pixel route (no registry here).
	opts.ForceReencode = true
	if _, _, err := pixeldata.TranscodeDataSet(context.Background(), source.Object(), transfer.JPEGBaseline, transfer.JPEGXLJPEGRecompression, opts); err == nil || transcoder.calls != 1 {
		t.Fatalf("ForceReencode used the codestream transcoder: calls=%d err=%v", transcoder.calls, err)
	}
}

func TestCodestreamTranscoderFailuresAreTyped(t *testing.T) {
	source := codecfixture.JPEGBaselineSmall()
	for name, transcoder := range map[string]*tagTranscoder{
		"frame count": {frames: 1},
		"panic":       {panics: true},
	} {
		_, _, err := pixeldata.TranscodeDataSet(context.Background(), source.Object(), transfer.JPEGBaseline, transfer.JPEGXLJPEGRecompression,
			pixeldata.TranscodeOptions{CodestreamTranscoder: transcoder})
		var transcodeErr *pixeldata.TranscodeError
		if !errors.Is(err, pixeldata.ErrCodestreamTranscoderOutputInvalid) || !errors.As(err, &transcodeErr) || transcodeErr.Stage != "codestream" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
