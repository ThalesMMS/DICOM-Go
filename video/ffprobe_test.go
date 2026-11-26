package video

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The FFmpeg oracle runs with DICOMGO_FFPROBE=1 and ffmpeg/ffprobe on PATH.
// It encodes synthetic test patterns with every available H.264 and HEVC
// encoder (B-frames, several GOPs), muxes the Annex B streams, and requires
// ffprobe to accept the MP4 and FFmpeg to decode it to the same frames, in
// the same presentation order, as the elementary stream.

func requireFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	if os.Getenv("DICOMGO_FFPROBE") != "1" {
		t.Skip("set DICOMGO_FFPROBE=1 with ffmpeg and ffprobe on PATH to run the FFmpeg oracle")
	}
	var err error
	if ffmpeg, err = exec.LookPath("ffmpeg"); err != nil {
		t.Fatal("ffmpeg not found")
	}
	if ffprobe, err = exec.LookPath("ffprobe"); err != nil {
		t.Fatal("ffprobe not found")
	}
	return ffmpeg, ffprobe
}

// frameHashes decodes path and returns the per-frame MD5s in output order.
func frameHashes(t *testing.T, ffmpeg, path string) []string {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg framemd5 %s: %v", filepath.Base(path), err)
	}
	var hashes []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		hashes = append(hashes, strings.TrimSpace(fields[len(fields)-1]))
	}
	return hashes
}

func TestFFprobeAcceptsMuxedStreams(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	encoders, _ := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output()
	type variant struct {
		name, encoder, format string
		codec                 Codec
		args                  []string
	}
	all := []variant{
		{"x264-bframes", "libx264", "h264", CodecH264, []string{"-bf", "3", "-g", "12", "-x264-params", "keyint_min=12:scenecut=0"}},
		{"x264-poc2", "libx264", "h264", CodecH264, []string{"-bf", "0", "-g", "10"}},
		{"x264-cabac-high10", "libx264", "h264", CodecH264, []string{"-bf", "2", "-g", "8", "-pix_fmt", "yuv420p10le"}},
		{"x265-bframes", "libx265", "hevc", CodecHEVC, []string{"-x265-params", "bframes=4:keyint=12:min-keyint=12:scenecut=0:log-level=error"}},
		{"x265-open-gop", "libx265", "hevc", CodecHEVC, []string{"-x265-params", "bframes=3:keyint=10:open-gop=1:scenecut=0:log-level=error"}},
		{"vt-h264", "h264_videotoolbox", "h264", CodecH264, []string{"-profile:v", "high", "-bf", "2", "-g", "12"}},
		{"vt-hevc", "hevc_videotoolbox", "hevc", CodecHEVC, []string{"-bf", "2", "-g", "12"}},
	}
	dir := t.TempDir()
	ran := 0
	for _, v := range all {
		if !bytes.Contains(encoders, []byte(" "+v.encoder+" ")) {
			t.Logf("%s: encoder %s unavailable", v.name, v.encoder)
			continue
		}
		t.Run(v.name, func(t *testing.T) {
			es := filepath.Join(dir, v.name+"."+v.format)
			args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=96x64:rate=25", "-frames:v", "40"}
			if !strings.Contains(strings.Join(v.args, " "), "pix_fmt") {
				args = append(args, "-pix_fmt", "yuv420p")
			}
			args = append(append(append(args, "-c:v", v.encoder), v.args...), "-f", v.format, es)
			if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
				t.Skipf("encoder failed: %v %s", err, out)
			}
			data, err := os.ReadFile(es)
			if err != nil {
				t.Fatal(err)
			}
			var mp4 bytes.Buffer
			info, err := MuxFragmentedMP4(context.Background(), &mp4, data, MuxOptions{Codec: v.codec, FrameRate: 25, MaxSamplesPerFragment: 7})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, v.name+".mp4")
			if err := os.WriteFile(path, mp4.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			probe, err := exec.Command(ffprobe, "-v", "error", "-count_frames", "-show_streams", "-of", "json", path).Output()
			if err != nil {
				t.Fatalf("ffprobe rejected the MP4: %v", err)
			}
			var result struct {
				Streams []struct {
					CodecName   string `json:"codec_name"`
					Width       int    `json:"width"`
					Height      int    `json:"height"`
					FrameRate   string `json:"r_frame_rate"`
					ReadFrames  string `json:"nb_read_frames"`
					CodecTagStr string `json:"codec_tag_string"`
				} `json:"streams"`
			}
			if err := json.Unmarshal(probe, &result); err != nil || len(result.Streams) != 1 {
				t.Fatalf("ffprobe output: %v %s", err, probe)
			}
			st := result.Streams[0]
			if st.CodecName != v.format || st.Width != 96 || st.Height != 64 || st.FrameRate != "25/1" || st.CodecTagStr != info.SampleEntry {
				t.Fatalf("ffprobe stream %+v, mux info %+v", st, info)
			}
			if n, _ := strconv.Atoi(st.ReadFrames); n != info.Samples {
				t.Fatalf("ffprobe decoded %s frames, muxed %d", st.ReadFrames, info.Samples)
			}
			want, got := frameHashes(t, ffmpeg, es), frameHashes(t, ffmpeg, path)
			// Leading pictures that cannot be decoded are not muxed.
			if len(want) != len(got) || strings.Join(want, ",") != strings.Join(got, ",") {
				t.Fatalf("decoded frames differ: elementary stream %d frames, MP4 %d (dropped %d)", len(want), len(got), info.DroppedLeading)
			}
			// Container timestamps: decoded frames come out at c, c+d, c+2d,
			// ... The composition times start at 0; FFmpeg adds a constant
			// shift for negative composition offsets (its dts_shift).
			frames, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "frame=pts", "-of", "csv=p=0", path).Output()
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Fields(string(frames))
			if len(lines) != info.Samples {
				t.Fatalf("ffprobe listed %d frames", len(lines))
			}
			var shift int
			for i, line := range lines {
				p, err := strconv.Atoi(strings.Split(line, ",")[0])
				if err != nil {
					t.Fatalf("frame %d pts %q", i, line)
				}
				if i == 0 {
					shift = p
				}
				if want := shift + i*int(info.SampleDuration); p != want {
					t.Fatalf("frame %d pts %d, want %d (presentation order)", i, p, want)
				}
			}
			if shift%int(info.SampleDuration) != 0 || shift > 8*int(info.SampleDuration) {
				t.Fatalf("presentation shift %d", shift)
			}
			ran++
			t.Logf("%+v", info)
		})
	}
	if ran == 0 {
		t.Fatal("no encoder produced a stream")
	}
}
