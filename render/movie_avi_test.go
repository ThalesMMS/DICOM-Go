package render

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// parsedAVI is the structure recovered by parseTestAVI, an independent
// test-only reader that walks the RIFF chunk tree instead of trusting the
// writer's layout constants.
type parsedAVI struct {
	micros, totalFrames, streams, width, height uint32
	fccType, handler, compression               string
	rate, scale, length                         uint32
	bitCount                                    uint16
	frames                                      [][]byte // '00dc' payloads in 'movi' order
	moviFourCC                                  int      // absolute offset of the 'movi' FOURCC
	index                                       []aviTestIndexEntry
}

type aviTestIndexEntry struct {
	id             string
	flags, off, sz uint32
}

type riffChunk struct {
	id   string
	pos  int // absolute offset of the chunk header
	data []byte
}

func readRIFFChunks(t *testing.T, buf []byte, base int) []riffChunk {
	t.Helper()
	var out []riffChunk
	for p := 0; p < len(buf); {
		if len(buf)-p < 8 {
			t.Fatalf("truncated chunk header at %d", base+p)
		}
		id := string(buf[p : p+4])
		size := int(binary.LittleEndian.Uint32(buf[p+4 : p+8]))
		if size > len(buf)-p-8 {
			t.Fatalf("chunk %q at %d declares %d bytes, only %d remain", id, base+p, size, len(buf)-p-8)
		}
		out = append(out, riffChunk{id: id, pos: base + p, data: buf[p+8 : p+8+size]})
		next := p + 8 + size
		if size%2 == 1 {
			if next >= len(buf) {
				t.Fatalf("chunk %q at %d misses its pad byte", id, base+p)
			}
			if buf[next] != 0 {
				t.Fatalf("chunk %q pad byte = %#x, want 0", id, buf[next])
			}
			next++
		}
		if (base+next)%2 != 0 {
			t.Fatalf("chunk after %q starts at odd offset %d", id, base+next)
		}
		p = next
	}
	return out
}

func listChunks(t *testing.T, c riffChunk, wantType string) []riffChunk {
	t.Helper()
	if c.id != "LIST" || len(c.data) < 4 || string(c.data[:4]) != wantType {
		t.Fatalf("chunk at %d is %q, want LIST %q", c.pos, c.id, wantType)
	}
	return readRIFFChunks(t, c.data[4:], c.pos+12)
}

func parseTestAVI(t *testing.T, file []byte) parsedAVI {
	t.Helper()
	le := binary.LittleEndian
	if len(file) < 12 || string(file[:4]) != "RIFF" || string(file[8:12]) != "AVI " {
		t.Fatalf("not a RIFF AVI file")
	}
	if got, want := int(le.Uint32(file[4:8])), len(file)-8; got != want {
		t.Fatalf("RIFF size = %d, want %d", got, want)
	}
	top := readRIFFChunks(t, file[12:], 12)
	if len(top) != 3 || top[2].id != "idx1" {
		t.Fatalf("top-level chunks = %v, want hdrl, movi, idx1", chunkIDs(top))
	}
	var avi parsedAVI
	hdrl := listChunks(t, top[0], "hdrl")
	if len(hdrl) != 2 || hdrl[0].id != "avih" || len(hdrl[0].data) != 56 {
		t.Fatalf("hdrl = %v, want avih(56) + strl", chunkIDs(hdrl))
	}
	h := hdrl[0].data
	avi.micros, avi.totalFrames = le.Uint32(h[0:]), le.Uint32(h[16:])
	avi.streams, avi.width, avi.height = le.Uint32(h[24:]), le.Uint32(h[32:]), le.Uint32(h[36:])
	if flags := le.Uint32(h[12:]); flags&aviFlagHasIndex == 0 {
		t.Fatalf("avih flags %#x miss AVIF_HASINDEX", flags)
	}
	strl := listChunks(t, hdrl[1], "strl")
	if len(strl) != 2 || strl[0].id != "strh" || strl[1].id != "strf" || len(strl[0].data) != 56 || len(strl[1].data) != 40 {
		t.Fatalf("strl = %v, want strh(56) + strf(40)", chunkIDs(strl))
	}
	s := strl[0].data
	avi.fccType, avi.handler = string(s[0:4]), string(s[4:8])
	avi.scale, avi.rate, avi.length = le.Uint32(s[20:]), le.Uint32(s[24:]), le.Uint32(s[32:])
	f := strl[1].data
	if le.Uint32(f[0:]) != 40 || le.Uint32(f[4:]) != avi.width || le.Uint32(f[8:]) != avi.height {
		t.Fatalf("strf BITMAPINFOHEADER size/dimensions disagree with avih")
	}
	avi.bitCount, avi.compression = le.Uint16(f[14:]), string(f[16:20])

	avi.moviFourCC = top[1].pos + 8
	for _, c := range listChunks(t, top[1], "movi") {
		if c.id != "00dc" {
			t.Fatalf("movi holds chunk %q, want 00dc", c.id)
		}
		avi.frames = append(avi.frames, c.data)
	}
	idx := top[2].data
	if len(idx)%16 != 0 {
		t.Fatalf("idx1 size %d is not a multiple of 16", len(idx))
	}
	for p := 0; p < len(idx); p += 16 {
		avi.index = append(avi.index, aviTestIndexEntry{
			id: string(idx[p : p+4]), flags: le.Uint32(idx[p+4:]), off: le.Uint32(idx[p+8:]), sz: le.Uint32(idx[p+12:]),
		})
	}
	// Every index entry must point (relative to the 'movi' FOURCC) at the
	// chunk it describes.
	if len(avi.index) != len(avi.frames) {
		t.Fatalf("idx1 has %d entries for %d frames", len(avi.index), len(avi.frames))
	}
	for i, e := range avi.index {
		pos := avi.moviFourCC + int(e.off)
		if e.id != "00dc" || e.flags&aviIndexKeyframe == 0 {
			t.Fatalf("idx1[%d] = %+v, want keyframe 00dc", i, e)
		}
		if pos+8 > len(file) || string(file[pos:pos+4]) != "00dc" {
			t.Fatalf("idx1[%d] offset %d does not point at a 00dc chunk", i, e.off)
		}
		if got := le.Uint32(file[pos+4:]); got != e.sz || int(e.sz) != len(avi.frames[i]) {
			t.Fatalf("idx1[%d] size %d, chunk %d, payload %d", i, e.sz, got, len(avi.frames[i]))
		}
		if !bytes.Equal(file[pos+8:pos+8+int(e.sz)], avi.frames[i]) {
			t.Fatalf("idx1[%d] points at different bytes than movi frame %d", i, i)
		}
	}
	return avi
}

func chunkIDs(chunks []riffChunk) []string {
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.id
		if c.id == "LIST" && len(c.data) >= 4 {
			ids[i] += " " + string(c.data[:4])
		}
	}
	return ids
}

// testMovieFrames renders n distinct synthetic colour frames: a smooth ramp
// whose hue shifts per frame, so every frame is different and not flat.
func testMovieFrames(n, w, h int) []image.Image {
	frames := make([]image.Image, n)
	for i := range frames {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetRGBA(x, y, color.RGBA{
					R: uint8((x*255/w + i*40) % 256),
					G: uint8(y * 255 / h),
					B: uint8((i * 60) % 256),
					A: 255,
				})
			}
		}
		frames[i] = img
	}
	return frames
}

func meanRGB(img image.Image) [3]float64 {
	b := img.Bounds()
	var sum [3]float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, _ := img.At(x, y).RGBA()
			sum[0] += float64(r >> 8)
			sum[1] += float64(g >> 8)
			sum[2] += float64(bb >> 8)
		}
	}
	n := float64(b.Dx() * b.Dy())
	return [3]float64{sum[0] / n, sum[1] / n, sum[2] / n}
}

func checkAVIFrames(t *testing.T, avi parsedAVI, src []image.Image, fps int) {
	t.Helper()
	w, h := src[0].Bounds().Dx(), src[0].Bounds().Dy()
	if avi.fccType != "vids" || avi.handler != "MJPG" || avi.compression != "MJPG" || avi.bitCount != 24 {
		t.Fatalf("stream = %q/%q compression %q bits %d, want vids/MJPG/MJPG/24", avi.fccType, avi.handler, avi.compression, avi.bitCount)
	}
	if avi.streams != 1 || int(avi.width) != w || int(avi.height) != h {
		t.Fatalf("avih streams=%d %dx%d, want 1 %dx%d", avi.streams, avi.width, avi.height, w, h)
	}
	if int(avi.totalFrames) != len(src) || int(avi.length) != len(src) || len(avi.frames) != len(src) {
		t.Fatalf("frame counts avih=%d strh=%d movi=%d, want %d", avi.totalFrames, avi.length, len(avi.frames), len(src))
	}
	if avi.scale != 1 || int(avi.rate) != fps {
		t.Fatalf("strh rate/scale = %d/%d, want %d/1", avi.rate, avi.scale, fps)
	}
	if want := uint32(1e6/float64(fps) + 0.5); avi.micros != want {
		t.Fatalf("avih µs/frame = %d, want %d", avi.micros, want)
	}
	for i, data := range avi.frames {
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("frame %d does not decode as JPEG: %v", i, err)
		}
		if _, ok := img.(*image.YCbCr); !ok {
			t.Fatalf("frame %d decodes as %T, want three-component YCbCr", i, img)
		}
		if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
			t.Fatalf("frame %d is %v, want %dx%d", i, img.Bounds(), w, h)
		}
		got, want := meanRGB(img), meanRGB(src[i])
		for c := 0; c < 3; c++ {
			if d := got[c] - want[c]; d > 4 || d < -4 {
				t.Fatalf("frame %d channel %d mean %.1f, source %.1f", i, c, got[c], want[c])
			}
		}
	}
}

func TestEncodeMJPEGAVIRoundTrip(t *testing.T) {
	for _, size := range []image.Point{{64, 48}, {37, 23}} {
		t.Run(fmt.Sprintf("%dx%d", size.X, size.Y), func(t *testing.T) {
			frames := testMovieFrames(7, size.X, size.Y)
			var buf bytes.Buffer
			if err := EncodeMJPEGAVI(&buf, frames, AVIOptions{FPS: 15}); err != nil {
				t.Fatalf("EncodeMJPEGAVI: %v", err)
			}
			checkAVIFrames(t, parseTestAVI(t, buf.Bytes()), frames, 15)
		})
	}
}

// TestAVIWriterMatchesInMemoryEncoder streams through a seekable file and
// requires the result to be byte-identical to the single-pass encoder.
func TestAVIWriterMatchesInMemoryEncoder(t *testing.T) {
	frames := testMovieFrames(9, 50, 30)
	var want bytes.Buffer
	if err := EncodeMJPEGAVI(&want, frames, AVIOptions{FPS: 24, Quality: 75}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "movie.avi")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	aw, err := NewAVIWriter(f, AVIOptions{FPS: 24, Quality: 75})
	if err != nil {
		t.Fatal(err)
	}
	for i, frame := range frames {
		if err := aw.WriteFrame(frame); err != nil {
			t.Fatalf("WriteFrame %d: %v", i, err)
		}
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := aw.Close(); !errors.Is(err, ErrAVIClosed) {
		t.Fatalf("second Close = %v, want ErrAVIClosed", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("streamed AVI (%d bytes) differs from in-memory AVI (%d bytes)", len(got), want.Len())
	}
	checkAVIFrames(t, parseTestAVI(t, got), frames, 24)
}

func TestAVIChunkPadding(t *testing.T) {
	var buf bytes.Buffer
	if err := writeAVIChunk(&buf, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	want := []byte{'0', '0', 'd', 'c', 3, 0, 0, 0, 1, 2, 3, 0}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("odd chunk = %v, want %v", buf.Bytes(), want)
	}
	layout := aviLayout{fps: 10, width: 2, height: 2}
	layout.add(3)
	layout.add(4)
	idx := layout.index()
	if off := binary.LittleEndian.Uint32(idx[8+16+8:]); off != 4+8+4 {
		t.Fatalf("second idx1 offset = %d, want %d (after padded odd chunk)", off, 4+8+4)
	}
	if got := len(layout.header()); got != aviHeaderBytes {
		t.Fatalf("header length = %d, want %d", got, aviHeaderBytes)
	}
	if got := string(layout.header()[aviMoviFourCCPos:]); got != "movi" {
		t.Fatalf("movi FOURCC at %d = %q", aviMoviFourCCPos, got)
	}
}

func TestAVISizeBoundRefusesCleanly(t *testing.T) {
	frames := testMovieFrames(40, 64, 64)
	limit := int64(12000)

	var mem bytes.Buffer
	err := EncodeMJPEGAVI(&mem, frames, AVIOptions{FPS: 10, MaxBytes: limit})
	if !errors.Is(err, ErrAVITooLarge) {
		t.Fatalf("EncodeMJPEGAVI over bound = %v, want ErrAVITooLarge", err)
	}
	if mem.Len() != 0 {
		t.Fatalf("EncodeMJPEGAVI wrote %d bytes before refusing", mem.Len())
	}

	path := filepath.Join(t.TempDir(), "bounded.avi")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	aw, err := NewAVIWriter(f, AVIOptions{FPS: 10, MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	written := 0
	for _, frame := range frames {
		if err := aw.WriteFrame(frame); err != nil {
			if !errors.Is(err, ErrAVITooLarge) {
				t.Fatalf("WriteFrame = %v, want ErrAVITooLarge", err)
			}
			break
		}
		written++
	}
	if written == 0 || written == len(frames) {
		t.Fatalf("bound accepted %d of %d frames; test limit is not exercising the bound", written, len(frames))
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close after refusal: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) > limit {
		t.Fatalf("bounded AVI is %d bytes, limit %d", len(data), limit)
	}
	checkAVIFrames(t, parseTestAVI(t, data), frames[:written], 10)

	if opts, _ := (AVIOptions{FPS: 1, MaxBytes: 1 << 40}).normalized(); opts.MaxBytes != MaxAVIBytes {
		t.Fatalf("MaxBytes above the AVI 1.0 bound normalized to %d, want %d", opts.MaxBytes, MaxAVIBytes)
	}
}

func TestAVIRejectsInvalidInput(t *testing.T) {
	frames := testMovieFrames(2, 16, 16)
	cases := []struct {
		name   string
		frames []image.Image
		opts   AVIOptions
		want   error
	}{
		{"no frames", nil, AVIOptions{FPS: 10}, nil},
		{"zero fps", frames, AVIOptions{}, nil},
		{"fps too high", frames, AVIOptions{FPS: MaxAVIFPS + 1}, nil},
		{"bad quality", frames, AVIOptions{FPS: 10, Quality: 101}, nil},
		{"nil frame", []image.Image{frames[0], nil}, AVIOptions{FPS: 10}, nil},
		{"size change", []image.Image{frames[0], image.NewRGBA(image.Rect(0, 0, 8, 8))}, AVIOptions{FPS: 10}, ErrAVIFrameSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := EncodeMJPEGAVI(&buf, tc.frames, tc.opts)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("EncodeMJPEGAVI = %v, want error %v", err, tc.want)
			}
			if buf.Len() != 0 {
				t.Fatalf("wrote %d bytes before failing", buf.Len())
			}
		})
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "empty.avi"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	aw, err := NewAVIWriter(f, AVIOptions{FPS: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := aw.WriteFrame(frames[0]); err != nil {
		t.Fatal(err)
	}
	if err := aw.WriteFrame(image.NewRGBA(image.Rect(0, 0, 3, 3))); !errors.Is(err, ErrAVIFrameSize) {
		t.Fatalf("mismatched WriteFrame = %v, want ErrAVIFrameSize", err)
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close after rejected frame: %v", err)
	}
	if err := aw.WriteFrame(frames[1]); !errors.Is(err, ErrAVIClosed) {
		t.Fatalf("WriteFrame after Close = %v, want ErrAVIClosed", err)
	}
}

func TestAVIGrayFramesBecomeThreeComponentJPEG(t *testing.T) {
	gray := image.NewGray(image.Rect(0, 0, 20, 10))
	for i := range gray.Pix {
		gray.Pix[i] = uint8(i * 3)
	}
	var buf bytes.Buffer
	if err := EncodeMJPEGAVI(&buf, []image.Image{gray, gray}, AVIOptions{FPS: 2}); err != nil {
		t.Fatal(err)
	}
	checkAVIFrames(t, parseTestAVI(t, buf.Bytes()), []image.Image{gray, gray}, 2)
}

// TestAVIExternalProbe runs ffprobe on a generated AVI when
// DICOMGO_FFPROBE=1 is set. ffprobe is looked up on PATH and, on Windows, in
// C:/msys64/ucrt64/bin. The default offline run skips it.
func TestAVIExternalProbe(t *testing.T) {
	if os.Getenv("DICOMGO_FFPROBE") != "1" {
		t.Skip("set DICOMGO_FFPROBE=1 to probe the AVI with a local ffprobe")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil && runtime.GOOS == "windows" {
		candidate := `C:\msys64\ucrt64\bin\ffprobe.exe`
		if _, statErr := os.Stat(candidate); statErr == nil {
			probe, err = candidate, nil
		}
	}
	if err != nil {
		t.Skip("ffprobe not found on PATH or in C:/msys64/ucrt64/bin")
	}
	frames := testMovieFrames(12, 96, 64)
	path := filepath.Join(t.TempDir(), "probe.avi")
	var buf bytes.Buffer
	if err := EncodeMJPEGAVI(&buf, frames, AVIOptions{FPS: 12}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(probe, "-v", "error", "-count_frames", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height,nb_read_frames,r_frame_rate", "-of", "default=nw=1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	got := string(out)
	t.Logf("ffprobe:\n%s", got)
	for _, want := range []string{"codec_name=mjpeg", "width=96", "height=64", "nb_read_frames=12", "r_frame_rate=12/1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ffprobe output misses %q", want)
		}
	}
}

// TestWriteMovieSamples writes a sample AVI and GIF into
// DICOMGO_MOVIE_SAMPLE_DIR for manual inspection in desktop players.
func TestWriteMovieSamples(t *testing.T) {
	dir := os.Getenv("DICOMGO_MOVIE_SAMPLE_DIR")
	if dir == "" {
		t.Skip("set DICOMGO_MOVIE_SAMPLE_DIR to write sample movies")
	}
	frames := make([]image.Image, 24)
	for i := range frames {
		frames[i] = testVRShadedFrame(160, 120, float64(i)/float64(len(frames)))
	}
	var avi, gifBuf bytes.Buffer
	if err := EncodeMJPEGAVI(&avi, frames, AVIOptions{FPS: 12}); err != nil {
		t.Fatal(err)
	}
	if err := EncodeGIF(&gifBuf, frames, 8); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"sample-mjpeg.avi": avi.Bytes(), "sample-adaptive.gif": gifBuf.Bytes()} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
