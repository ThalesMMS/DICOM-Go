package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// referenceReduce crops region out of a decoded little-endian frame and
// reduces it by 2^level, written independently of the library: every output
// sample is the mean, rounded half up, of the stored values of the cell of
// 2^level x 2^level frame samples it stands for, clipped to the frame.
func referenceReduce(frame []byte, m pixeldata.Metadata, region image.Rectangle, level int) []byte {
	cell := 1 << level
	floor := func(a int) int { return a / cell } // a >= 0
	ceil := func(a int) int { return (a + cell - 1) / cell }
	out := image.Rect(floor(region.Min.X), floor(region.Min.Y), ceil(region.Max.X), ceil(region.Max.Y))
	width, samples := m.BytesPerSample(), int(m.SamplesPerPixel)
	columns, rows := int(m.Columns), int(m.Rows)
	planar := samples > 1 && m.PlanarConfiguration == 1
	offset := func(x, y, s int) int {
		if planar {
			return ((s*rows+y)*columns + x) * width
		}
		return ((y*columns+x)*samples + s) * width
	}
	bitsStored, shift := int(m.BitsStored), int(m.HighBit)+1-int(m.BitsStored)
	mask := int64(1)<<bitsStored - 1
	value := func(at int) int64 {
		var raw int64
		switch width {
		case 1:
			raw = int64(frame[at])
		case 2:
			raw = int64(binary.LittleEndian.Uint16(frame[at:]))
		default:
			raw = int64(binary.LittleEndian.Uint32(frame[at:]))
		}
		v := raw >> shift & mask
		if m.PixelRepresentation == 1 && v>>(bitsStored-1) == 1 {
			v -= 1 << bitsStored
		}
		return v
	}
	result := make([]byte, out.Dx()*out.Dy()*samples*width)
	put := func(x, y, s int, v int64) {
		var at int
		if planar {
			at = ((s*out.Dy()+y)*out.Dx() + x) * width
		} else {
			at = ((y*out.Dx()+x)*samples + s) * width
		}
		raw := uint64(v&mask) << shift
		switch width {
		case 1:
			result[at] = byte(raw)
		case 2:
			binary.LittleEndian.PutUint16(result[at:], uint16(raw))
		default:
			binary.LittleEndian.PutUint32(result[at:], uint32(raw))
		}
	}
	for oy := out.Min.Y; oy < out.Max.Y; oy++ {
		for ox := out.Min.X; ox < out.Max.X; ox++ {
			for s := 0; s < samples; s++ {
				var sum, count int64
				for y := oy * cell; y < (oy+1)*cell && y < rows; y++ {
					for x := ox * cell; x < (ox+1)*cell && x < columns; x++ {
						sum += value(offset(x, y, s))
						count++
					}
				}
				// floor((sum + count/2) / count), for negative sums too.
				n := sum + count/2
				mean := n / count
				if n%count != 0 && n < 0 {
					mean--
				}
				put(ox-out.Min.X, oy-out.Min.Y, s, mean)
			}
		}
	}
	return result
}

func partialTestFiles(t *testing.T) (map[string]string, pixeldata.TranscodeOptions) {
	t.Helper()
	options := streamTestRegistries(t)
	dir := t.TempDir()
	planar := streamTestSource(13, 17, 3, 3, 8)
	planar.Put(core.NewRawElement(core.NewTag(0x0028, 0x0006), core.VRUS, []byte{1, 0}))
	// Signed 12-bit samples in 16 bits, with bits above HighBit set that
	// the stored value does not include.
	signed := streamTestSource(19, 23, 2, 1, 16)
	signed.Put(core.NewRawElement(core.NewTag(0x0028, 0x0101), core.VRUS, []byte{12, 0}))
	signed.Put(core.NewRawElement(core.NewTag(0x0028, 0x0102), core.VRUS, []byte{11, 0}))
	signed.Put(core.NewRawElement(core.NewTag(0x0028, 0x0103), core.VRUS, []byte{1, 0}))
	samples := make([]byte, 19*23*2*2)
	for i := 0; i < len(samples)/2; i++ {
		v := int16((i*37)%4096 - 2048)
		binary.LittleEndian.PutUint16(samples[2*i:], uint16(v)&0x0FFF|0xA000)
	}
	signed.Put(core.NewRawElement(core.TagPixelData, core.VROW, samples))
	// Unsigned 32-bit samples near the top of their range, whose sums need
	// more than 32 bits.
	wide := streamTestSource(9, 11, 1, 1, 16)
	wide.Put(core.NewRawElement(core.NewTag(0x0028, 0x0100), core.VRUS, []byte{32, 0}))
	wide.Put(core.NewRawElement(core.NewTag(0x0028, 0x0101), core.VRUS, []byte{32, 0}))
	wide.Put(core.NewRawElement(core.NewTag(0x0028, 0x0102), core.VRUS, []byte{31, 0}))
	wideSamples := make([]byte, 9*11*4)
	for i := 0; i < 9*11; i++ {
		binary.LittleEndian.PutUint32(wideSamples[4*i:], 0xFFFFFFFF-uint32(i*i*977))
	}
	wide.Put(core.NewRawElement(core.TagPixelData, core.VROW, wideSamples))
	paths := map[string]string{}
	for name, file := range map[string]*object.File{
		"mono16": {Dataset: streamTestSource(21, 33, 2, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian},
		"rgb8":   {Dataset: streamTestSource(16, 24, 2, 3, 8), TransferSyntax: transfer.ImplicitVRLittleEndian},
		"planar": {Dataset: planar, TransferSyntax: transfer.ExplicitVRLittleEndian},
		"signed": {Dataset: signed, TransferSyntax: transfer.ExplicitVRLittleEndian},
		"u32":    {Dataset: wide, TransferSyntax: transfer.ExplicitVRLittleEndian},
	} {
		paths[name] = writeStreamTestFile(t, dir, name+".dcm", file)
	}
	for name, c := range map[string]struct {
		source string
		target transfer.Syntax
	}{
		"rle16": {"mono16", transfer.RLELossless}, "rlergb": {"rgb8", transfer.RLELossless},
		"jls16": {"mono16", transfer.JPEGLSLossless}, "bigendian": {"mono16", transfer.ExplicitVRBigEndian},
		"signedrle": {"signed", transfer.RLELossless},
	} {
		path := filepath.Join(dir, name+".dcm")
		if err := os.WriteFile(path, inMemoryTranscode(t, paths[c.source], c.target, options), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	return paths, options
}

// TestDecodePartialMatchesCropAndReduce compares DecodePartial, for regions
// at several reductions, with the independent crop and reduction of the
// full in-memory decode, for native, big endian, planar, signed, RLE and
// JPEG-LS frames, and checks the path and the fallback report.
func TestDecodePartialMatchesCropAndReduce(t *testing.T) {
	paths, options := partialTestFiles(t)
	wantPath := map[string]pixeldata.PartialDecodePath{
		"mono16": pixeldata.PartialDecodeNativeRows, "rgb8": pixeldata.PartialDecodeNativeRows,
		"planar": pixeldata.PartialDecodeNativeRows, "signed": pixeldata.PartialDecodeNativeRows, "u32": pixeldata.PartialDecodeNativeRows,
		"bigendian": pixeldata.PartialDecodeNativeRows, "rle16": pixeldata.PartialDecodeCodecRegion,
		"rlergb": pixeldata.PartialDecodeCodecRegion, "signedrle": pixeldata.PartialDecodeCodecRegion,
		"jls16": pixeldata.PartialDecodeCodecRegion,
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			frames, metadata := fullDecode(t, path, options.DecoderRegistry)
			reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			w, h := int(metadata.Columns), int(metadata.Rows)
			bounds := image.Rect(0, 0, w, h)
			for i, frame := range frames {
				for _, level := range []int{0, 1, 2, 3, 5} {
					// The whole frame reduced: a region's reduction must be
					// its crop, so that reduced tiles meet without seams.
					whole, err := reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{Frame: i, ReductionLevel: level})
					if err != nil {
						t.Fatalf("frame %d level %d: %v", i, level, err)
					}
					wholeMetadata := whole.Metadata
					for _, region := range []image.Rectangle{
						{}, image.Rect(1, 2, 5, 7), image.Rect(w-3, h-1, w, h), image.Rect(0, 3, w, 4), image.Rect(3, 5, w-2, h-3),
					} {
						got, err := reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{Frame: i, Region: region, ReductionLevel: level})
						if err != nil {
							t.Fatalf("frame %d %v level %d: %v", i, region, level, err)
						}
						if region == (image.Rectangle{}) {
							region = bounds
						}
						out := pixeldata.ReducedRegion(region, level)
						if got.Region != out || int(got.Metadata.Columns) != out.Dx() || int(got.Metadata.Rows) != out.Dy() || got.Metadata.NumberOfFrames != 1 {
							t.Fatalf("frame %d %v level %d: output %v, %dx%d", i, region, level, got.Region, got.Metadata.Columns, got.Metadata.Rows)
						}
						want := crop(frame, metadata, region)
						if level > 0 {
							want = referenceReduce(frame, metadata, region, level)
						}
						if !bytes.Equal(got.Data, want) {
							t.Fatalf("frame %d %v level %d differs from the reduced crop of the full decode", i, region, level)
						}
						if want := crop(whole.Data, wholeMetadata, out); !bytes.Equal(got.Data, want) {
							t.Fatalf("frame %d %v level %d differs from the crop of the reduced frame", i, region, level)
						}
						path := wantPath[name]
						if name == "jls16" && level == 0 {
							path = pixeldata.PartialDecodeCodec
						} else if path != pixeldata.PartialDecodeNativeRows && region == bounds && level == 0 {
							path = pixeldata.PartialDecodeFullFrame
						}
						fallback := level > 0 || (path == pixeldata.PartialDecodeFullFrame && region != bounds)
						if got.Path != path || got.Fallback != fallback || got.AveragedReduction != (level > 0) || got.QualityLayers != 0 {
							t.Fatalf("frame %d %v level %d: path %v fallback %v averaged %v, want %v %v", i, region, level, got.Path, got.Fallback, got.AveragedReduction, path, fallback)
						}
					}
				}
			}
			// A layer count is not served by any of these codecs: every layer
			// is decoded, and the result says so.
			got, err := reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{QualityLayers: 1, Region: image.Rect(0, 0, 2, 2)})
			if err != nil || got.QualityLayers != 0 || !got.Fallback || !bytes.Equal(got.Data, crop(frames[0], metadata, image.Rect(0, 0, 2, 2))) {
				t.Fatalf("one quality layer: %+v, %v", got.Path, err)
			}
		})
	}
}

func TestDecodePartialRejectsInvalidRequests(t *testing.T) {
	paths, options := partialTestFiles(t)
	reader, err := pixeldata.OpenFrameReader(context.Background(), paths["jls16"], pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	native, err := pixeldata.OpenFrameReader(context.Background(), paths["mono16"], pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	for _, c := range []struct {
		name    string
		reader  *pixeldata.FrameReader
		request pixeldata.PartialDecodeRequest
		want    error
	}{
		{"region past the edge", reader, pixeldata.PartialDecodeRequest{Region: image.Rect(0, 0, 34, 1)}, pixeldata.ErrInvalidRegion},
		{"negative region", native, pixeldata.PartialDecodeRequest{Region: image.Rect(-1, 0, 2, 2)}, pixeldata.ErrInvalidRegion},
		{"empty region", native, pixeldata.PartialDecodeRequest{Region: image.Rect(2, 2, 2, 5)}, pixeldata.ErrInvalidRegion},
		{"negative reduction", reader, pixeldata.PartialDecodeRequest{ReductionLevel: -1}, pixeldata.ErrInvalidReduction},
		{"reduction too deep", native, pixeldata.PartialDecodeRequest{ReductionLevel: pixeldata.MaxReductionLevel + 1}, pixeldata.ErrInvalidReduction},
		{"negative layers", reader, pixeldata.PartialDecodeRequest{QualityLayers: -1}, pixeldata.ErrInvalidQualityLayers},
		{"frame past the end", native, pixeldata.PartialDecodeRequest{Frame: 2}, pixeldata.ErrInvalidMetadata},
		{"no fallback: reduce native", native, pixeldata.PartialDecodeRequest{ReductionLevel: 1, NoFallback: true}, pixeldata.ErrPartialDecodeUnsupported},
		{"no fallback: layers of native", native, pixeldata.PartialDecodeRequest{QualityLayers: 1, NoFallback: true}, pixeldata.ErrPartialDecodeUnsupported},
		{"no fallback: JPEG-LS reduction", reader, pixeldata.PartialDecodeRequest{ReductionLevel: 2, NoFallback: true}, pixeldata.ErrPartialDecodeUnsupported},
	} {
		if result, err := c.reader.DecodePartial(context.Background(), c.request); !errors.Is(err, c.want) || result.Data != nil {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// What needs no fallback is served with NoFallback.
	if got, err := native.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{Region: image.Rect(1, 1, 4, 4), NoFallback: true}); err != nil || got.Fallback || got.Path != pixeldata.PartialDecodeNativeRows {
		t.Errorf("native region without fallback: %v %v", got.Path, err)
	}
	for _, region := range []image.Rectangle{{}, image.Rect(0, 0, 4, 4)} {
		if got, err := reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{Region: region, NoFallback: true}); err != nil || got.Fallback || got.Path != pixeldata.PartialDecodeCodec {
			t.Errorf("JPEG-LS region %v without fallback: %v %v", region, got.Path, err)
		}
	}
	// All three supported fixtures can emit a region directly.
	rle, err := pixeldata.OpenFrameReader(context.Background(), paths["rle16"], pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
	if err != nil {
		t.Fatal(err)
	}
	defer rle.Close()
	for name, c := range map[string]struct {
		reader *pixeldata.FrameReader
		want   pixeldata.PartialDecodeCapabilities
	}{
		"native": {native, pixeldata.PartialDecodeCapabilities{Region: true}},
		"rle":    {rle, pixeldata.PartialDecodeCapabilities{Region: true}},
		"jls":    {reader, pixeldata.PartialDecodeCapabilities{Region: true}},
	} {
		if got, err := c.reader.PartialDecodeCapabilities(context.Background(), 0); err != nil || got != c.want {
			t.Errorf("%s capabilities %+v, %v", name, got, err)
		}
	}
	if _, err := native.PartialDecodeCapabilities(context.Background(), 5); !errors.Is(err, pixeldata.ErrInvalidMetadata) {
		t.Errorf("capabilities of frame 5: %v", err)
	}
}

// partialCodec serves partial decodes for an RLE codec by decoding whole
// frames and reducing them like the reference, standing in for a codec
// with its own partial decode; err replaces its result.
type partialCodec struct {
	pixeldata.Codec
	err      error
	short    bool
	requests []pixeldata.PartialDecodeRequest
}

func (c *partialCodec) PartialDecodeCapabilities([]byte, pixeldata.Metadata) (pixeldata.PartialDecodeCapabilities, error) {
	return pixeldata.PartialDecodeCapabilities{Region: true, ReductionLevels: 3, QualityLayers: 2}, nil
}

func (c *partialCodec) DecodeFramePartial(_ context.Context, codestream []byte, metadata pixeldata.Metadata, request pixeldata.PartialDecodeRequest) (pixeldata.Frames, error) {
	c.requests = append(c.requests, request)
	if c.err != nil {
		return pixeldata.Frames{}, c.err
	}
	obj := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0028, 0x0002), core.VRUS, binary.LittleEndian.AppendUint16(nil, metadata.SamplesPerPixel)),
		core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("MONOCHROME2 ")),
		core.NewRawElement(core.NewTag(0x0028, 0x0010), core.VRUS, binary.LittleEndian.AppendUint16(nil, metadata.Rows)),
		core.NewRawElement(core.NewTag(0x0028, 0x0011), core.VRUS, binary.LittleEndian.AppendUint16(nil, metadata.Columns)),
		core.NewRawElement(core.NewTag(0x0028, 0x0100), core.VRUS, binary.LittleEndian.AppendUint16(nil, metadata.BitsAllocated)),
		core.NewRawElement(core.NewTag(0x0028, 0x0101), core.VRUS, binary.LittleEndian.AppendUint16(nil, metadata.BitsStored)),
		core.NewRawElement(core.NewTag(0x0028, 0x0102), core.VRUS, binary.LittleEndian.AppendUint16(nil, metadata.HighBit)),
		core.NewRawElement(core.NewTag(0x0028, 0x0103), core.VRUS, binary.LittleEndian.AppendUint16(nil, metadata.PixelRepresentation)),
	}, nil)
	frames, err := c.Codec.Decode(pixeldata.PixelData{Encapsulated: true, Sequence: core.FragmentSequence{Fragments: [][]byte{codestream}}}, obj)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	out := pixeldata.ReducedRegion(request.Region, request.ReductionLevel)
	data := referenceReduce(frames.Data[0], metadata, request.Region, request.ReductionLevel)
	if c.short {
		data = data[1:]
	}
	return pixeldata.Frames{Rows: out.Dy(), Columns: out.Dx(), Data: [][]byte{data}}, nil
}

func TestDecodePartialUsesPartialFrameDecoder(t *testing.T) {
	paths, options := partialTestFiles(t)
	rleCodec, ok := options.DecoderRegistry.GetCodec(transfer.RLELossless.UID)
	if !ok {
		t.Fatal("no RLE codec")
	}
	codec := &partialCodec{Codec: rleCodec}
	registry := pixeldata.NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.RLELossless.UID, codec); err != nil {
		t.Fatal(err)
	}
	frames, metadata := fullDecode(t, paths["rle16"], options.DecoderRegistry)
	reader, err := pixeldata.OpenFrameReader(context.Background(), paths["rle16"], pixeldata.FrameReaderOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got, err := reader.PartialDecodeCapabilities(context.Background(), 1); err != nil || got != (pixeldata.PartialDecodeCapabilities{Region: true, ReductionLevels: 3, QualityLayers: 2}) {
		t.Fatalf("capabilities %+v, %v", got, err)
	}
	request := pixeldata.PartialDecodeRequest{Frame: 1, Region: image.Rect(3, 4, 20, 9), ReductionLevel: 2, QualityLayers: 1, NoFallback: true}
	got, err := reader.DecodePartial(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != pixeldata.PartialDecodeCodec || got.Fallback || got.AveragedReduction || got.QualityLayers != 1 || got.Region != image.Rect(0, 1, 5, 3) {
		t.Fatalf("result %+v", got)
	}
	if !bytes.Equal(got.Data, referenceReduce(frames[1], metadata, request.Region, 2)) || len(codec.requests) != 1 || codec.requests[0] != request {
		t.Fatalf("data or request differ: %+v", codec.requests)
	}
	// The codec's typed refusals are the caller's.
	codec.err = pixeldata.ErrInvalidQualityLayers
	if _, err := reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{QualityLayers: 3}); !errors.Is(err, pixeldata.ErrInvalidQualityLayers) {
		t.Fatalf("layers beyond the codestream's: %v", err)
	}
	// An unsupported request falls back, or is refused without fallback.
	codec.err = pixeldata.ErrPartialDecodeUnsupported
	got, err = reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{Region: image.Rect(1, 1, 3, 3)})
	if err != nil || got.Path != pixeldata.PartialDecodeFullFrame || !got.Fallback || !bytes.Equal(got.Data, crop(frames[0], metadata, image.Rect(1, 1, 3, 3))) {
		t.Fatalf("fallback %v, %v", got.Path, err)
	}
	if _, err := reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{Region: image.Rect(1, 1, 3, 3), NoFallback: true}); !errors.Is(err, pixeldata.ErrPartialDecodeUnsupported) {
		t.Fatalf("no fallback: %v", err)
	}
	// A result of the wrong size is refused.
	codec.err, codec.short = nil, true
	if _, err := reader.DecodePartial(context.Background(), request); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("short result: %v", err)
	}
}

func TestReducedRegion(t *testing.T) {
	for _, c := range []struct {
		region image.Rectangle
		level  int
		want   image.Rectangle
	}{
		{image.Rect(0, 0, 33, 21), 0, image.Rect(0, 0, 33, 21)},
		{image.Rect(0, 0, 33, 21), 1, image.Rect(0, 0, 17, 11)},
		{image.Rect(0, 0, 33, 21), 5, image.Rect(0, 0, 2, 1)},
		{image.Rect(1, 2, 2, 3), 3, image.Rect(0, 0, 1, 1)},
		{image.Rect(9, 7, 17, 8), 3, image.Rect(1, 0, 3, 1)},
	} {
		if got := pixeldata.ReducedRegion(c.region, c.level); got != c.want {
			t.Errorf("ReducedRegion(%v, %d) = %v, want %v", c.region, c.level, got, c.want)
		}
	}
}

// BenchmarkFrameReaderDecodePartial compares a 1024x1024 region at 1/4
// (256x256 out) of a 4096x4096 16-bit frame through DecodePartial with
// decoding the whole frame and reducing it, for native and RLE data.
func BenchmarkFrameReaderDecodePartial(b *testing.B) {
	dir := b.TempDir()
	native := filepath.Join(dir, "native.dcm")
	writeLargeNativeFile(b, native, 4096, 4096, 1)
	options := streamTestRegistries(b)
	file, err := object.OpenFile(native)
	if err != nil {
		b.Fatal(err)
	}
	encoded, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.RLELossless, pixeldata.TranscodeOptions{EncoderRegistry: options.EncoderRegistry, Limits: pixeldata.TranscodeLimits{MaxPixels: 1 << 26}})
	file.Close()
	if err != nil {
		b.Fatal(err)
	}
	var data bytes.Buffer
	if err := object.WriteFile(&data, encoded); err != nil {
		b.Fatal(err)
	}
	rlePath := filepath.Join(dir, "rle.dcm")
	if err := os.WriteFile(rlePath, data.Bytes(), 0o600); err != nil {
		b.Fatal(err)
	}
	region := image.Rect(1024, 1024, 2048, 2048)
	for _, c := range []struct {
		name  string
		path  string
		whole bool
	}{
		{"native/partial", native, false}, {"native/frame+reduce", native, true},
		{"rle/partial", rlePath, false}, {"rle/frame+reduce", rlePath, true},
	} {
		b.Run(c.name, func(b *testing.B) {
			reader, err := pixeldata.OpenFrameReader(context.Background(), c.path, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
			if err != nil {
				b.Fatal(err)
			}
			defer reader.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if c.whole {
					frame, m, err := reader.Frame(context.Background(), 0)
					if err != nil {
						b.Fatal(err)
					}
					_ = referenceReduce(frame, m, region, 2)
				} else if _, err := reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{Region: region, ReductionLevel: 2}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type wholeFrameOnlyCodec struct {
	pixeldata.Codec
}

func (wholeFrameOnlyCodec) PartialDecodeCapabilities([]byte, pixeldata.Metadata) (pixeldata.PartialDecodeCapabilities, error) {
	return pixeldata.PartialDecodeCapabilities{}, nil
}

func (wholeFrameOnlyCodec) DecodeFramePartial(context.Context, []byte, pixeldata.Metadata, pixeldata.PartialDecodeRequest) (pixeldata.Frames, error) {
	return pixeldata.Frames{}, pixeldata.ErrPartialDecodeUnsupported
}

func (wholeFrameOnlyCodec) DecodeFrameRegion(context.Context, []byte, pixeldata.Metadata, image.Rectangle) (pixeldata.Frames, error) {
	return pixeldata.Frames{}, pixeldata.ErrRegionUnsupported
}

func TestPartialDecodeCapabilitiesHonorsExplicitWholeFrameOnly(t *testing.T) {
	paths, options := partialTestFiles(t)
	underlying, ok := options.DecoderRegistry.GetCodec(transfer.RLELossless.UID)
	if !ok {
		t.Fatal("no RLE codec")
	}
	registry := pixeldata.NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.RLELossless.UID, wholeFrameOnlyCodec{underlying}); err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.OpenFrameReader(context.Background(), paths["rle16"], pixeldata.FrameReaderOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	req := pixeldata.PartialDecodeRequest{Frame: 1, Region: image.Rect(1, 1, 4, 4)}
	got, err := reader.DecodePartial(context.Background(), req)
	if err != nil || !got.Fallback || got.Path != pixeldata.PartialDecodeFullFrame {
		t.Fatalf("region must crop a full decoded frame: %+v, %v", got, err)
	}
	if caps, err := reader.PartialDecodeCapabilities(context.Background(), 1); err != nil || caps != (pixeldata.PartialDecodeCapabilities{}) {
		t.Fatalf("whole-frame codec advertised partial capabilities %+v, %v", caps, err)
	}
}
