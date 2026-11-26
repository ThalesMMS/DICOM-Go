package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func streamTestRegistries(t testing.TB) pixeldata.TranscodeOptions {
	t.Helper()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	for _, register := range []func(pixeldata.EncoderRegistry) error{
		rle.RegisterEncoder, jpegls.RegisterEncoder, jpeglossless.RegisterEncoder,
		func(r pixeldata.EncoderRegistry) error { return jpeg.RegisterEncoder(r, 90) },
	} {
		if err := register(encoders); err != nil {
			t.Fatal(err)
		}
	}
	return pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders}
}

// streamTestSource is a synthetic multi-frame image: samples vary with the
// position and the frame so every frame differs.
func streamTestSource(rows, columns, frames, samples, bits int) *object.Object {
	us := func(tag core.Tag, v int) core.Element {
		return core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(v)))
	}
	text := func(tag core.Tag, vr core.VR, v string) core.Element {
		if len(v)%2 == 1 {
			v += " "
		}
		return core.NewRawElement(tag, vr, []byte(v))
	}
	photometric := "MONOCHROME2"
	if samples == 3 {
		photometric = "RGB"
	}
	bytesPer := bits / 8
	pixels := make([]byte, 0, rows*columns*samples*bytesPer*frames)
	for f := 0; f < frames; f++ {
		for i := 0; i < rows*columns*samples; i++ {
			v := (i*7 + f*131 + (i/columns)*3) % (1 << (bits - 4))
			if bytesPer == 2 {
				pixels = binary.LittleEndian.AppendUint16(pixels, uint16(v))
			} else {
				pixels = append(pixels, byte(v))
			}
		}
	}
	vr := core.VROB
	if bits > 8 {
		vr = core.VROW
	}
	elements := []core.Element{
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.1040.1"),
		text(core.NewTag(0x0008, 0x0060), core.VRCS, "OT"),
		us(core.NewTag(0x0028, 0x0002), samples),
		text(core.NewTag(0x0028, 0x0004), core.VRCS, photometric),
		text(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(frames)),
		us(core.NewTag(0x0028, 0x0010), rows),
		us(core.NewTag(0x0028, 0x0011), columns),
		us(core.NewTag(0x0028, 0x0100), bits),
		us(core.NewTag(0x0028, 0x0101), bits),
		us(core.NewTag(0x0028, 0x0102), bits-1),
		us(core.NewTag(0x0028, 0x0103), 0),
		core.NewRawElement(core.TagPixelData, vr, pixels),
	}
	if samples == 3 {
		elements = append(elements, us(core.NewTag(0x0028, 0x0006), 0))
	}
	return object.FromElements(elements, nil)
}

func writeStreamTestFile(t testing.TB, dir, name string, file *object.File) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var b bytes.Buffer
	if err := object.WriteFile(&b, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// inMemoryTranscode is TranscodePath's in-memory route: the whole file read,
// TranscodeFile and WriteFile.
func inMemoryTranscode(t *testing.T, path string, target transfer.Syntax, options pixeldata.TranscodeOptions) []byte {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	output, _, err := pixeldata.TranscodeFile(context.Background(), file, target, options)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := object.WriteFile(&b, output); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// withFixedSOPInstanceUID rewrites a file with a fixed SOP Instance UID, so
// that lossy outputs (each given a new UID) can be compared.
func withFixedSOPInstanceUID(t *testing.T, data []byte) []byte {
	t.Helper()
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.3.4"}})
	file.Meta.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0002, 0x0003), VR: core.VRUI}, Value: core.StringValue{"1.2.3.4"}})
	var b bytes.Buffer
	if err := object.WriteFile(&b, file); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// splitItems rewrites an encapsulated file with every frame split over two
// Items and a Basic Offset Table, so frames must be joined through a plan.
func splitItems(t *testing.T, data []byte) *object.File {
	t.Helper()
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	pixel, _ := file.Dataset.Get(core.TagPixelData)
	sequence := pixel.Value.(core.FragmentSequence)
	var fragments [][]byte
	var bot []byte
	var offset uint32
	for _, fragment := range sequence.Fragments {
		bot = binary.LittleEndian.AppendUint32(bot, offset)
		half := (len(fragment) / 4) * 2
		for _, part := range [][]byte{fragment[:half], fragment[half:]} {
			fragments = append(fragments, part)
			offset += 8 + uint32(len(part)+len(part)%2)
		}
	}
	file.Dataset.Put(core.Element{Header: pixel.Header, Value: core.FragmentSequence{OffsetTable: bot, Fragments: fragments}})
	return file
}

func TestTranscodePathStreamingRequiresDecoderRegistry(t *testing.T) {
	ctx := context.Background()
	options := streamTestRegistries(t)
	previous := pixeldata.DefaultRegistry
	pixeldata.DefaultRegistry = options.DecoderRegistry
	t.Cleanup(func() { pixeldata.DefaultRegistry = previous })
	for _, syntax := range []transfer.Syntax{transfer.RLELossless, transfer.EncapsulatedUncompressedExplicitVRLittleEndian} {
		input, _, err := pixeldata.TranscodeFile(ctx, &object.File{
			Dataset: streamTestSource(2, 4, 2, 1, 8), TransferSyntax: transfer.ExplicitVRLittleEndian,
		}, syntax, options)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		path := writeStreamTestFile(t, dir, "input.dcm", input)
		for _, target := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.EncapsulatedUncompressedExplicitVRLittleEndian} {
			_, err := pixeldata.TranscodePath(ctx, path, filepath.Join(dir, "output.dcm"), target, pixeldata.TranscodeOptions{})
			if syntax.UID == transfer.RLELossless.UID {
				if !errors.Is(err, pixeldata.ErrCodecRegistryNil) {
					t.Fatalf("target %s without registry: %v", target.UID, err)
				}
			} else if err != nil {
				t.Fatalf("encapsulated uncompressed to %s: %v", target.UID, err)
			}
		}
	}
}

func TestTranscodePathStreamingMatchesInMemory(t *testing.T) {
	options := streamTestRegistries(t)
	dir := t.TempDir()
	mono := &object.File{Dataset: streamTestSource(21, 33, 7, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian}
	rgb := &object.File{Dataset: streamTestSource(16, 24, 4, 3, 8), TransferSyntax: transfer.ExplicitVRLittleEndian}
	monoPath := writeStreamTestFile(t, dir, "mono.dcm", mono)
	rgbPath := writeStreamTestFile(t, dir, "rgb.dcm", rgb)
	implicitPath := writeStreamTestFile(t, dir, "implicit.dcm", &object.File{Dataset: streamTestSource(21, 33, 7, 1, 16), TransferSyntax: transfer.ImplicitVRLittleEndian})
	rlePath := filepath.Join(dir, "rle.dcm")
	if err := os.WriteFile(rlePath, inMemoryTranscode(t, monoPath, transfer.RLELossless, options), 0o600); err != nil {
		t.Fatal(err)
	}
	jlsPath := filepath.Join(dir, "jls.dcm")
	if err := os.WriteFile(jlsPath, inMemoryTranscode(t, rgbPath, transfer.JPEGLSLossless, options), 0o600); err != nil {
		t.Fatal(err)
	}
	jlsData, _ := os.ReadFile(jlsPath)
	splitPath := writeStreamTestFile(t, dir, "split.dcm", splitItems(t, jlsData))

	lossy := options
	lossy.AllowLossy = true
	for _, c := range []struct {
		name    string
		source  string
		target  transfer.Syntax
		options pixeldata.TranscodeOptions
		lossy   bool
	}{
		{"native to RLE", monoPath, transfer.RLELossless, options, false},
		{"native to JPEG-LS", monoPath, transfer.JPEGLSLossless, options, false},
		{"native to JPEG lossless", monoPath, transfer.JPEGLosslessSV1, options, false},
		{"RGB to JPEG baseline", rgbPath, transfer.JPEGBaseline, lossy, true},
		{"RLE to native", rlePath, transfer.ExplicitVRLittleEndian, options, false},
		{"JPEG-LS to implicit", jlsPath, transfer.ImplicitVRLittleEndian, options, false},
		{"JPEG-LS to RLE", jlsPath, transfer.RLELossless, options, false},
		{"split JPEG-LS to native", splitPath, transfer.ExplicitVRLittleEndian, options, false},
		{"RLE to encapsulated uncompressed", rlePath, transfer.EncapsulatedUncompressedExplicitVRLittleEndian, options, false},
		{"implicit to explicit", implicitPath, transfer.ExplicitVRLittleEndian, options, false},
		{"RLE equivalent", rlePath, transfer.RLELossless, options, false},
		{"split JPEG-LS equivalent", splitPath, transfer.JPEGLSLossless, options, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := inMemoryTranscode(t, c.source, c.target, c.options)
			streamed := 0
			restore := pixeldata.CountTranscodeStreams(&streamed)
			defer restore()
			out := filepath.Join(t.TempDir(), "out.dcm")
			report, err := pixeldata.TranscodePath(context.Background(), c.source, out, c.target, c.options)
			if err != nil {
				t.Fatal(err)
			}
			if streamed != 1 {
				t.Fatalf("streaming route not taken")
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if c.lossy {
				if !report.Lossy {
					t.Fatal("lossy report expected")
				}
				want, got = withFixedSOPInstanceUID(t, want), withFixedSOPInstanceUID(t, got)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("streamed output (%d bytes) differs from the in-memory route (%d bytes)", len(got), len(want))
			}
		})
	}
}

// TestTranscodePathStreamingFallsBack checks that options the streaming
// route does not cover still transcode, through the in-memory route.
func TestTranscodePathStreamingFallsBack(t *testing.T) {
	options := streamTestRegistries(t)
	dir := t.TempDir()
	source := writeStreamTestFile(t, dir, "mono.dcm", &object.File{Dataset: streamTestSource(8, 8, 2, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian})
	options.RecoverUN = &object.UNRecoveryOptions{}
	streamed := 0
	restore := pixeldata.CountTranscodeStreams(&streamed)
	defer restore()
	if _, err := pixeldata.TranscodePath(context.Background(), source, filepath.Join(dir, "out.dcm"), transfer.RLELossless, options); err != nil {
		t.Fatal(err)
	}
	if streamed != 0 {
		t.Fatal("RecoverUN took the streaming route")
	}
}

// peakLiveHeap runs run and returns the largest live heap above the start,
// sampled each time the streaming route's progress (frames encoded to the
// spool, then output written) crosses another every bytes. Each sample
// forces a collection, so it measures what the transcode retains, not
// garbage a busy machine has not collected yet, and is taken at a fixed
// point of the work rather than whenever a sampler gets scheduled.
func peakLiveHeap(every int64, run func() error) (peak uint64, samples int, err error) {
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var progress int64
	restore := pixeldata.ObserveTranscodeProgress(func(bytes int) {
		before := progress
		progress += int64(bytes)
		if progress/every == before/every {
			return
		}
		var m runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > base.HeapAlloc {
			peak = max(peak, m.HeapAlloc-base.HeapAlloc)
		}
		samples++
	})
	defer restore()
	err = run()
	return peak, samples, err
}

// writeLargeNativeFile writes a native multi-frame file frame by frame with
// object.WriteFileFrames, without holding its Pixel Data.
func writeLargeNativeFile(t testing.TB, path string, rows, columns, frames int) {
	t.Helper()
	header := streamTestSource(rows, columns, 1, 1, 16)
	count := strconv.Itoa(frames)
	if len(count)%2 == 1 {
		count += " "
	}
	header.Put(core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte(count)))
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	source := &syntheticFrames{rows: rows, columns: columns, frames: frames}
	if err := object.WriteFileFrames(out, &object.File{Dataset: header, TransferSyntax: transfer.ExplicitVRLittleEndian}, source, object.FrameWriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

// syntheticFrames generates 16-bit frames one at a time.
type syntheticFrames struct {
	rows, columns, frames int
	buffer                []byte
}

func (s *syntheticFrames) Frames() int { return s.frames }
func (s *syntheticFrames) FrameLength(int) (int64, error) {
	return int64(s.rows * s.columns * 2), nil
}
func (s *syntheticFrames) WriteFrameTo(i int, w io.Writer) (int64, error) {
	if s.buffer == nil {
		s.buffer = make([]byte, s.rows*s.columns*2)
	}
	for p := 0; p < s.rows*s.columns; p++ {
		binary.LittleEndian.PutUint16(s.buffer[2*p:], uint16((p/64+i*131)%4096))
	}
	n, err := w.Write(s.buffer)
	return int64(n), err
}

func TestTranscodePathStreamingMemoryIsPerFrame(t *testing.T) {
	const rows, columns, frames = 256, 256, 96 // 12 MiB of Pixel Data, 128 KiB frames
	dir := t.TempDir()
	source := filepath.Join(dir, "large.dcm")
	writeLargeNativeFile(t, source, rows, columns, frames)
	options := streamTestRegistries(t)
	for _, target := range []transfer.Syntax{transfer.RLELossless, transfer.ImplicitVRLittleEndian} {
		peak, samples, err := peakLiveHeap(rows*columns*2, func() error {
			_, err := pixeldata.TranscodePath(context.Background(), source, filepath.Join(dir, "out-"+target.UID+".dcm"), target, options)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: live heap peak %d KiB above the start over %d samples", target.Name, peak>>10, samples)
		if limit := uint64(4 << 20); peak > limit {
			t.Fatalf("%s: live heap peak %d bytes for 12 MiB of Pixel Data, want under %d", target.Name, peak, limit)
		}
		// A sample per frame: each frame is either encoded to the spool or
		// written to the output, at least its native size, a frame at a time.
		if samples < frames {
			t.Fatalf("%s: %d live heap samples, want one per frame", target.Name, samples)
		}
	}
}

// TestTranscodePathStreamingMultiGigabyte transcodes a synthetic file of
// several GiB; set DICOMGO_LARGE_TRANSCODE=1 to run it.
func TestTranscodePathStreamingMultiGigabyte(t *testing.T) {
	if os.Getenv("DICOMGO_LARGE_TRANSCODE") == "" {
		t.Skip("set DICOMGO_LARGE_TRANSCODE=1 to transcode a multi-GiB file")
	}
	const rows, columns, frames = 1024, 1024, 2000 // 3.9 GiB (native Pixel Data has a 32-bit length), 2 MiB frames
	dir := t.TempDir()
	source := filepath.Join(dir, "huge.dcm")
	writeLargeNativeFile(t, source, rows, columns, frames)
	options := streamTestRegistries(t)
	options.Limits = pixeldata.TranscodeLimits{MaxFrames: frames, MaxPixels: 1 << 40, MaxInputBytes: 8 << 30, MaxOutputBytes: 8 << 30, MaxDuration: time.Hour}
	for _, target := range []transfer.Syntax{transfer.RLELossless, transfer.ImplicitVRLittleEndian} {
		start := time.Now()
		out := filepath.Join(dir, "out.dcm")
		peak, _, err := peakLiveHeap(64<<20, func() error {
			_, err := pixeldata.TranscodePath(context.Background(), source, out, target, options)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(out)
		t.Logf("%s: %d-byte output in %v, peak heap %d bytes (frame %d bytes)", target.Name, info.Size(), time.Since(start), peak, rows*columns*2)
		if peak > 64<<20 {
			t.Fatalf("peak heap %d bytes", peak)
		}
		_ = os.Remove(out)
	}
}
