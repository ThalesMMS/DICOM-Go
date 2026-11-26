package videoffmpeg_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapimage"
	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/videoffmpeg"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/video"
)

func TestFFmpegAVCInterop(t *testing.T) {
	executable := os.Getenv("DICOMGO_VIDEO_FFMPEG")
	probe := os.Getenv("DICOMGO_VIDEO_FFPROBE")
	if executable == "" || probe == "" {
		t.Skip("set DICOMGO_VIDEO_FFMPEG and DICOMGO_VIDEO_FFPROBE to explicit external tool paths")
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(binary)
	backend, err := videoffmpeg.New(executable, hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mp4, err := os.ReadFile("../../../video/testdata/avc-high41.mp4")
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := encapimage.FromImages(ctx, [][]byte{mp4}, encapimage.ImageOptions{
		AnatomicRegion: &encapimage.AnatomicRegion{CodeValue: "T-D0010", CodingSchemeDesignator: "SRT", CodeMeaning: "Entire body"},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := video.DecodeAVCMP4(ctx, source, backend, video.AVCCodecLimits{})
	if err != nil {
		t.Fatal(err)
	}
	reference := exec.CommandContext(ctx, executable, "-v", "error", "-nostdin", "-f", "mov", "-i", "pipe:0",
		"-map", "0:v:0", "-pix_fmt", "yuv420p", "-f", "rawvideo", "pipe:1")
	reference.Stdin = bytes.NewReader(mp4)
	expected, err := reference.Output()
	if err != nil || !bytes.Equal(decoded.Samples, expected) {
		t.Fatalf("decode differs from independent direct FFmpeg invocation, err %v", err)
	}
	output, report, err := video.TranscodeAVCMP4(ctx, source, backend, backend, video.AVCTranscodeOptions{AllowLossy: true, CRF: 23})
	if err != nil {
		t.Fatal(err)
	}
	inputID, _ := source.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	if inputID == report.SOPInstanceUID || report.Profile != decoded.Profile {
		t.Fatalf("identity or profile %#v", report)
	}
	var container bytes.Buffer
	if _, err := video.Copy(ctx, output, &container, video.Limits{}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "transcoded.mp4")
	if err := os.WriteFile(file, container.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if reader := os.Getenv("DICOMGO_VIDEO_MF_READER"); reader != "" {
		nv12 := filepath.Join(t.TempDir(), "mf.nv12")
		out, err := exec.CommandContext(ctx, reader, file, nv12).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "width=96 height=64 fps=25/1") ||
			!strings.Contains(string(out), "frames=6") {
			t.Fatalf("Media Foundation: %v: %s", err, out)
		}
		for i := 0; i < 6; i++ {
			if !strings.Contains(string(out), fmt.Sprintf("frame=%d timestamp100ns=%d bytes=9216", i, i*400000)) {
				t.Fatalf("Media Foundation frame order: %s", out)
			}
		}
		mf, err := os.ReadFile(nv12)
		if err != nil {
			t.Fatal(err)
		}
		reference := exec.CommandContext(ctx, executable, "-v", "error", "-nostdin", "-i", file,
			"-map", "0:v:0", "-pix_fmt", "nv12", "-f", "rawvideo", "pipe:1")
		expected, err := reference.Output()
		if err != nil || !bytes.Equal(mf, expected) {
			t.Fatalf("Media Foundation differs from FFmpeg: %v", err)
		}
		t.Logf("independent Windows Media Foundation decoded %d exact NV12 bytes", len(mf))
	}
	if artifacts := os.Getenv("DICOMGO_VIDEO_ARTIFACT_DIR"); artifacts != "" {
		if err := os.MkdirAll(artifacts, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifacts, "transcoded.mp4"), container.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		var dicom bytes.Buffer
		if err := object.WriteFile(&dicom, output); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifacts, "transcoded.dcm"), dicom.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifacts, "decoded-source.yuv"), decoded.Samples, 0600); err != nil {
			t.Fatal(err)
		}
	}
	probeOutput, err := exec.CommandContext(ctx, probe, "-v", "error", "-select_streams", "v", "-count_frames",
		"-show_entries", "stream=codec_name,profile,level,width,height,pix_fmt,nb_read_frames,r_frame_rate",
		"-show_entries", "frame=pts_time", "-of", "json", file).Output()
	if err != nil {
		t.Fatalf("ffprobe refused MP4: %v", err)
	}
	var raw struct {
		Streams []map[string]json.RawMessage `json:"streams"`
		Frames  []struct {
			PTS string `json:"pts_time"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(probeOutput, &raw); err != nil || len(raw.Streams) != 1 || len(raw.Frames) != 6 {
		t.Fatalf("metadata %s, %v", probeOutput, err)
	}
	for key, expected := range map[string]string{"codec_name": "h264", "profile": "High", "pix_fmt": "yuv420p", "nb_read_frames": "6", "r_frame_rate": "25/1"} {
		var value string
		if err := json.Unmarshal(raw.Streams[0][key], &value); err != nil || value != expected {
			t.Fatalf("%s = %q want %q: %s", key, value, expected, probeOutput)
		}
	}
	for key, expected := range map[string]int{"width": 96, "height": 64, "level": 41} {
		var value int
		if err := json.Unmarshal(raw.Streams[0][key], &value); err != nil || value != expected {
			t.Fatalf("%s = %d want %d", key, value, expected)
		}
	}
	for i, frame := range raw.Frames {
		pts, err := strconv.ParseFloat(frame.PTS, 64)
		if err != nil || pts != float64(i)/25 {
			t.Fatalf("frame %d time %v %v", i, pts, err)
		}
	}
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, output); err != nil {
		t.Fatal(err)
	}
	reopened, err := object.ReadFile(bytes.NewReader(wire.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := video.DecodeAVCMP4(ctx, reopened, backend, video.AVCCodecLimits{}); err != nil {
		t.Fatal(err)
	}
	limited := video.AVCTranscodeOptions{AllowLossy: true, CRF: 23, MaxOutputBytes: 128}
	if result, _, err := video.TranscodeAVCMP4(ctx, source, backend, backend, limited); err == nil || result != nil {
		t.Fatalf("truncated backend output accepted: %v", err)
	}
}

func TestBackendRequiresPinnedExecutable(t *testing.T) {
	name := filepath.Join(t.TempDir(), "backend.exe")
	if err := os.WriteFile(name, []byte("not a codec"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := videoffmpeg.New(name, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong executable digest accepted")
	}
	if _, err := videoffmpeg.New("backend.exe", strings.Repeat("0", 64)); err == nil {
		t.Fatal("relative executable accepted")
	}
}
