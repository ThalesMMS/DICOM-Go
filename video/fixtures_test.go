package video

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The committed elementary streams in testdata/ are FFmpeg 9.0 testsrc2
// patterns (no patient data) encoded with the macOS VideoToolbox H.264 and
// HEVC encoders with B-frames and two GOPs, plus an H.264 stream in an
// MPEG-2 Transport Stream written by FFmpeg (whose PES timestamps are an
// independent record of the presentation order). TestGenerateVideoFixtures
// regenerates them with DICOMGO_VIDEO_GENERATE=1 on macOS.

var videoFixtures = []struct {
	name, encoder, format string
	args                  []string
}{
	{"vt-h264.h264", "h264_videotoolbox", "h264", []string{"-profile:v", "high", "-bf", "2", "-g", "12", "-b:v", "150k"}},
	{"vt-hevc.h265", "hevc_videotoolbox", "hevc", []string{"-bf", "2", "-g", "12", "-b:v", "150k"}},
	{"vt-h264.ts", "h264_videotoolbox", "mpegts", []string{"-profile:v", "high", "-bf", "2", "-g", "12", "-b:v", "150k"}},
}

func TestGenerateVideoFixtures(t *testing.T) {
	if os.Getenv("DICOMGO_VIDEO_GENERATE") != "1" {
		t.Skip("set DICOMGO_VIDEO_GENERATE=1 (macOS, ffmpeg with VideoToolbox) to regenerate testdata")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range videoFixtures {
		args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=96x64:rate=25", "-frames:v", "24",
			"-pix_fmt", "yuv420p", "-c:v", f.encoder}
		args = append(append(args, f.args...), "-f", f.format, filepath.Join("testdata", f.name))
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", f.name, err, out)
		}
	}
}
