package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func extendedEncoderMetadata(rows, columns, bitsStored uint16) pixeldata.Metadata {
	return pixeldata.Metadata{Rows: rows, Columns: columns, SamplesPerPixel: 1, PhotometricInterpretation: "MONOCHROME2", NumberOfFrames: 1, BitsAllocated: 16, BitsStored: bitsStored, HighBit: bitsStored - 1}
}

// extendedPhantomFrame is a synthetic CT-like slice: a bright disc with a
// soft gradient, two inserts with sharp edges, fine texture and
// deterministic pseudo-random noise, scaled to Bits Stored. The bits above
// High Bit carry junk the encoder must ignore.
func extendedPhantomFrame(rows, columns int, bitsStored uint16, seed uint32) []byte {
	top := float64(int(1)<<bitsStored - 1)
	out := make([]byte, rows*columns*2)
	state := seed*2654435761 + 1
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			dx, dy := float64(x)/float64(columns)-0.5, float64(y)/float64(rows)-0.5
			v := 0.05
			if dx*dx+dy*dy < 0.18 {
				v = 0.45 + 0.25*dx + 0.1*math.Sin(float64(x)*0.21)*math.Cos(float64(y)*0.17)
			}
			if (dx-0.15)*(dx-0.15)+dy*dy < 0.006 {
				v = 0.92
			}
			if dx < -0.1 && dx > -0.25 && dy > -0.05 && dy < 0.12 {
				v = 0.2
			}
			state = state*1664525 + 1013904223
			v += (float64(state>>16&0xff)/255 - 0.5) * 0.01
			value := uint16(math.Max(0, math.Min(top, math.Round(v*top))))
			binary.LittleEndian.PutUint16(out[2*(y*columns+x):], value|0xa000&^(uint16(1)<<bitsStored-1))
		}
	}
	return out
}

func extendedSamples(frame []byte, bitsStored uint16) []int {
	out := make([]int, len(frame)/2)
	for i := range out {
		out[i] = int(binary.LittleEndian.Uint16(frame[2*i:]) & (uint16(1)<<bitsStored - 1))
	}
	return out
}

// extendedErrors reports the PSNR (peak 2^bits-1) and the largest absolute
// difference between two sample sets.
func extendedErrors(got, want []int, bitsStored uint16) (psnr float64, worst int) {
	var squared float64
	for i := range want {
		d := got[i] - want[i]
		squared += float64(d * d)
		worst = max(worst, abs(d))
	}
	if squared == 0 {
		return math.Inf(1), 0
	}
	peak := float64(int(1)<<bitsStored - 1)
	return 10 * math.Log10(peak*peak/(squared/float64(len(want)))), worst
}

func nativeExtendedObject(rows, columns, bitsStored uint16, frames int, pixels []byte) *object.Object {
	elements := []core.Element{
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.1032.1"),
		dicomtest.Uint16Element(tagRows, core.VRUS, nil, rows),
		dicomtest.Uint16Element(tagColumns, core.VRUS, nil, columns),
		dicomtest.Uint16Element(tagSamplesPerPixel, core.VRUS, nil, 1),
		dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, "MONOCHROME2"),
		dicomtest.Uint16Element(tagBitsAllocated, core.VRUS, nil, 16),
		dicomtest.Uint16Element(tagBitsStored, core.VRUS, nil, bitsStored),
		dicomtest.Uint16Element(tagHighBit, core.VRUS, nil, bitsStored-1),
		dicomtest.Uint16Element(tagPixelRepresentation, core.VRUS, nil, 0),
		core.NewRawElement(core.TagPixelData, core.VROW, pixels),
	}
	if frames > 1 {
		elements = append(elements, dicomtest.NewStringElement(tagNumberOfFrames, core.VRIS, strconv.Itoa(frames)))
	}
	return object.FromElements(elements, nil)
}

func TestExtendedEncoderCapabilitiesRegistrationAndQuality(t *testing.T) {
	for _, quality := range []int{0, 101} {
		if _, err := NewExtendedEncoder(quality); !errors.Is(err, ErrInvalidQuality) {
			t.Fatalf("NewExtendedEncoder(%d) error = %v, want ErrInvalidQuality", quality, err)
		}
	}
	encoder, err := NewExtendedEncoder(DefaultQuality)
	if err != nil {
		t.Fatal(err)
	}
	if encoder.Quality() != DefaultQuality {
		t.Fatalf("Quality = %d, want %d", encoder.Quality(), DefaultQuality)
	}
	caps := encoder.Capabilities()
	if caps.TransferSyntaxUID != transfer.JPEGExtended.UID || caps.Lossless || caps.LossyMethod != jpegLossyMethod || !caps.SupportsMultiFrame || caps.Backend != "pure-go" {
		t.Fatalf("capabilities = %#v", caps)
	}
	if len(caps.BitsAllocated) != 2 || caps.BitsAllocated[0] != 8 || caps.BitsAllocated[1] != 16 || len(caps.SamplesPerPixel) != 1 || caps.SamplesPerPixel[0] != 1 {
		t.Fatalf("capabilities = %#v", caps)
	}
	if err := RegisterExtendedEncoder(nil, DefaultQuality); !errors.Is(err, pixeldata.ErrEncoderRegistryNil) {
		t.Fatalf("RegisterExtendedEncoder(nil) error = %v", err)
	}
	registry := pixeldata.NewMemoryEncoderRegistry()
	if err := RegisterExtendedEncoder(registry, 0); !errors.Is(err, ErrInvalidQuality) {
		t.Fatalf("RegisterExtendedEncoder quality 0 error = %v", err)
	}
	if err := RegisterExtendedEncoder(registry, DefaultQuality); err != nil {
		t.Fatal(err)
	}
	if err := pixeldata.CheckEncoderAvailability(registry, UIDExtended); err != nil {
		t.Fatal(err)
	}
	// Registration is per syntax: .50 stays unregistered.
	if _, ok := registry.GetEncoder(UID); ok {
		t.Fatal("RegisterExtendedEncoder registered JPEG Baseline")
	}
	var zero *ExtendedEncoder
	if zero.Quality() != 0 {
		t.Fatal("nil encoder quality")
	}
	if _, err := zero.EncodeFrame(context.Background(), nil, extendedEncoderMetadata(1, 1, 12)); !errors.Is(err, ErrInvalidQuality) {
		t.Fatalf("nil encoder error = %v", err)
	}
}

// TestExtendedEncoderRoundTripsThroughPureGoDecoder encodes the phantom at
// each Bits Stored and quality, requires a Process 4 codestream (SOF1,
// precision 12) and decodes it with the pure-Go Process 4 decoder within a
// PSNR floor and maximum error per quality, limited to Bits Stored as the
// codec does. Quality 100 quantizes by 1. The quantization step does not
// depend on Bits Stored, so the error bound is in levels and the PSNR floor
// (for 12 bits) drops by 6.1 dB per bit fewer.
func TestExtendedEncoderRoundTripsThroughPureGoDecoder(t *testing.T) {
	type bound struct {
		quality int
		psnr    float64
		worst   int
	}
	bounds := []bound{{1, 20, 1400}, {25, 38, 200}, {50, 44, 100}, {75, 49, 50}, {90, 56, 24}, {100, 78, 2}}
	for _, bitsStored := range []uint16{12, 11, 10, 9} {
		for _, size := range [][2]uint16{{64, 64}, {45, 61}, {1, 1}, {9, 17}} {
			rows, columns := size[0], size[1]
			frame := extendedPhantomFrame(int(rows), int(columns), bitsStored, uint32(rows)+uint32(bitsStored))
			metadata := extendedEncoderMetadata(rows, columns, bitsStored)
			want := extendedSamples(frame, bitsStored)
			for _, b := range bounds {
				t.Run(fmt.Sprintf("bits%d-%dx%d-q%d", bitsStored, columns, rows, b.quality), func(t *testing.T) {
					encoder, err := NewExtendedEncoder(b.quality)
					if err != nil {
						t.Fatal(err)
					}
					encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
					if err != nil {
						t.Fatal(err)
					}
					if encoded.PhotometricInterpretation != "" || encoded.PlanarConfiguration != nil {
						t.Fatalf("monochrome metadata changed: %#v", encoded)
					}
					sof := jpegSegment(t, encoded.Data, 0xc1)
					if encoded.Data[sof] != 12 || bytes.Contains(encoded.Data, []byte{0xff, 0xc0}) {
						t.Fatal("codestream is not SOF1 precision 12")
					}
					decoded, err := decodeExtendedProcess4(encoded.Data, metadata)
					if err != nil {
						t.Fatal(err)
					}
					limitStoredBits16(decoded, metadata)
					got := extendedSamples(decoded, 16)
					for _, v := range got {
						if v >= 1<<bitsStored {
							t.Fatalf("decoded sample %d exceeds Bits Stored %d", v, bitsStored)
						}
					}
					psnr, worst := extendedErrors(got, want, bitsStored)
					floor := b.psnr - 6.1*float64(12-bitsStored)
					if psnr < floor || worst > b.worst {
						t.Fatalf("PSNR %.1f dB (floor %.1f), max error %d (limit %d)", psnr, floor, worst, b.worst)
					}
				})
			}
		}
	}
}

// TestExtendedEncoderExtremesUseEveryDCCategory codes flat blocks at 0 and
// 2^12-1 in alternation, so the DC differences reach category 15, and
// requires an exact reconstruction at quality 100; quality 1 also needs
// 16-bit quantization values (DQT Pq=1).
func TestExtendedEncoderExtremesUseEveryDCCategory(t *testing.T) {
	const rows, columns = 16, 32
	frame := make([]byte, rows*columns*2)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			if (x/8+y/8)%2 == 1 {
				binary.LittleEndian.PutUint16(frame[2*(y*columns+x):], 4095)
			}
		}
	}
	metadata := extendedEncoderMetadata(rows, columns, 12)
	want := extendedSamples(frame, 12)
	for _, tc := range []struct {
		quality int
		pq      byte
		worst   int
	}{{100, 0, 0}, {1, 1, 4095}} {
		encoder, _ := NewExtendedEncoder(tc.quality)
		encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
		if err != nil {
			t.Fatal(err)
		}
		dqt := jpegSegment(t, encoded.Data, 0xdb)
		if pq := encoded.Data[dqt] >> 4; pq != tc.pq {
			t.Fatalf("quality %d: DQT Pq=%d, want %d", tc.quality, pq, tc.pq)
		}
		decoded, err := decodeExtendedProcess4(encoded.Data, metadata)
		if err != nil {
			t.Fatalf("quality %d: %v", tc.quality, err)
		}
		if _, worst := extendedErrors(extendedSamples(decoded, 12), want, 12); worst > tc.worst {
			t.Fatalf("quality %d: max error %d, want <= %d", tc.quality, worst, tc.worst)
		}
	}
}

func TestExtendedEncoderTranscodeRecordsLossyCompression(t *testing.T) {
	const rows, columns, frames = 40, 52, 3
	var pixels []byte
	for f := 0; f < frames; f++ {
		pixels = append(pixels, extendedPhantomFrame(rows, columns, 12, uint32(f))...)
	}
	source := nativeExtendedObject(rows, columns, 12, frames, pixels)
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := RegisterExtendedEncoder(encoders, 90); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGExtended, pixeldata.TranscodeOptions{EncoderRegistry: encoders}); !errors.Is(err, pixeldata.ErrTranscodeLossyDisallowed) {
		t.Fatalf("transcode without AllowLossy error = %v, want ErrTranscodeLossyDisallowed", err)
	}
	compressed, report, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGExtended, pixeldata.TranscodeOptions{EncoderRegistry: encoders, AllowLossy: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Lossy {
		t.Fatalf("report = %#v", report)
	}
	if got, _ := compressed.GetString(core.NewTag(0x0028, 0x2110)); got != "01" {
		t.Fatalf("LossyImageCompression = %q, want 01", got)
	}
	if got, _ := compressed.GetString(core.NewTag(0x0028, 0x2114)); got != jpegLossyMethod {
		t.Fatalf("LossyImageCompressionMethod = %q", got)
	}
	ratio, _ := compressed.GetString(core.NewTag(0x0028, 0x2112))
	if value, err := strconv.ParseFloat(ratio, 64); err != nil || value <= 1 {
		t.Fatalf("LossyImageCompressionRatio = %q", ratio)
	}
	pixel, err := pixeldata.ExtractView(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if !pixel.Encapsulated || len(pixel.Sequence.Fragments) != frames {
		t.Fatalf("encapsulated pixel data = %d fragments", len(pixel.Sequence.Fragments))
	}
	metadata, err := pixeldata.ExtractMetadata(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.BitsAllocated != 16 || metadata.BitsStored != 12 || metadata.PhotometricInterpretation != "MONOCHROME2" {
		t.Fatalf("encoded metadata = %#v", metadata)
	}

	decoders := pixeldata.NewMemoryRegistry()
	if err := Register(decoders); err != nil {
		t.Fatal(err)
	}
	native, _, err := pixeldata.TranscodeDataSet(context.Background(), compressed, transfer.JPEGExtended, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{DecoderRegistry: decoders})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := pixeldata.ExtractNativeFramesView(native)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Data) != frames {
		t.Fatalf("decoded %d frames", len(decoded.Data))
	}
	for f := range decoded.Data {
		want := extendedSamples(pixels[f*rows*columns*2:(f+1)*rows*columns*2], 12)
		if psnr, worst := extendedErrors(extendedSamples(decoded.Data[f], 12), want, 12); psnr < 60 || worst > 64 {
			t.Fatalf("frame %d: PSNR %.1f dB, max error %d", f, psnr, worst)
		}
	}
}

func TestExtendedEncoderRejectsIncompatibleMetadataBeforeEncoding(t *testing.T) {
	encoder, err := NewExtendedEncoder(DefaultQuality)
	if err != nil {
		t.Fatal(err)
	}
	valid := extendedEncoderMetadata(1, 1, 12)
	tests := []struct {
		name   string
		mutate func(*pixeldata.Metadata)
	}{
		{name: "rows", mutate: func(m *pixeldata.Metadata) { m.Rows = 0 }},
		{name: "columns", mutate: func(m *pixeldata.Metadata) { m.Columns = 0 }},
		{name: "32 allocated", mutate: func(m *pixeldata.Metadata) { m.BitsAllocated = 32 }},
		{name: "8 stored", mutate: func(m *pixeldata.Metadata) { m.BitsStored, m.HighBit = 8, 7 }},
		{name: "13 stored", mutate: func(m *pixeldata.Metadata) { m.BitsStored, m.HighBit = 13, 12 }},
		{name: "16 stored", mutate: func(m *pixeldata.Metadata) { m.BitsStored, m.HighBit = 16, 15 }},
		{name: "high bit", mutate: func(m *pixeldata.Metadata) { m.HighBit = 15 }},
		{name: "signed", mutate: func(m *pixeldata.Metadata) { m.PixelRepresentation = 1 }},
		{name: "frames", mutate: func(m *pixeldata.Metadata) { m.NumberOfFrames = 0 }},
		{name: "color", mutate: func(m *pixeldata.Metadata) { m.SamplesPerPixel, m.PhotometricInterpretation = 3, "RGB" }},
		{name: "photometric", mutate: func(m *pixeldata.Metadata) { m.PhotometricInterpretation = "PALETTE COLOR" }},
		{name: "planar configuration", mutate: func(m *pixeldata.Metadata) { m.PlanarConfigurationPresent = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := valid
			test.mutate(&metadata)
			if _, err := encoder.EncodeFrame(context.Background(), []byte{0, 0}, metadata); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
				t.Fatalf("EncodeFrame error = %v, want ErrUnsupportedEncoderMetadata", err)
			}
		})
	}
	if _, err := encoder.EncodeFrame(context.Background(), []byte{0}, valid); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("short frame error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := encoder.EncodeFrame(ctx, []byte{0, 0}, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled EncodeFrame error = %v", err)
	}
	// The transcoder refuses the layouts its capabilities exclude.
	source := nativeExtendedObject(2, 2, 16, 1, make([]byte, 8))
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := RegisterExtendedEncoder(encoders, DefaultQuality); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGExtended, pixeldata.TranscodeOptions{EncoderRegistry: encoders, AllowLossy: true}); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
		t.Fatalf("16 stored bits transcode error = %v", err)
	}
}

func TestExtendedEncoderQualityAffectsOutputAndConcurrentUseIsSafe(t *testing.T) {
	const rows, columns = 64, 64
	frame := extendedPhantomFrame(rows, columns, 12, 7)
	metadata := extendedEncoderMetadata(rows, columns, 12)
	previous := math.MaxInt
	for _, quality := range []int{100, 90, 75, 50, 10} {
		encoder, _ := NewExtendedEncoder(quality)
		encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded.Data) >= previous {
			t.Fatalf("quality %d output %d bytes, not below %d", quality, len(encoded.Data), previous)
		}
		previous = len(encoded.Data)
	}

	encoder, _ := NewExtendedEncoder(DefaultQuality)
	want, err := encoder.EncodeFrame(context.Background(), frame, metadata)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := encoder.EncodeFrame(context.Background(), frame, metadata)
			if err == nil && !bytes.Equal(got.Data, want.Data) {
				err = errors.New("concurrent encoding differs")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestExtendedOptimalHuffmanTableLimitsLengthsAndKeepsAllOnesFree feeds
// Fibonacci and super-Fibonacci frequencies, and single-symbol statistics;
// every table must be canonical,
// within 16 bits, not a complete tree and accepted by the decoder's DHT
// parser.
func TestExtendedOptimalHuffmanTableLimitsLengthsAndKeepsAllOnesFree(t *testing.T) {
	var fibonacci [256]int
	a, b := 1, 1
	for i := 0; i < 40; i++ {
		fibonacci[i*5] = a
		a, b = b, a+b
	}
	// Ordinary Fibonacci frequencies reach only depth 21 with the reserved
	// one-count leaf. Adding one to each recurrence forces an unlimited tree
	// of depth 40, exercising both storage and HUFFVAL ordering above depth 32.
	// These 40 symbols are legal Baseline AC run/category combinations.
	var superFibonacci [256]int
	a, b = 1, 1
	for i := 0; i < 40; i++ {
		symbol := (i/10)<<4 | (i%10 + 1)
		superFibonacci[symbol] = a
		a, b = b, a+b+1
	}
	var single, all [256]int
	single[0] = 1000
	for i := range all {
		all[i] = i + 1
	}
	for name, frequencies := range map[string][256]int{"fibonacci": fibonacci, "super-fibonacci-depth-40": superFibonacci, "single": single, "all": all} {
		spec := extendedOptimalHuffmanTable(frequencies)
		used := 0
		for _, f := range frequencies {
			if f > 0 {
				used++
			}
		}
		if len(spec.symbols) != used {
			t.Fatalf("%s: %d symbols, want %d", name, len(spec.symbols), used)
		}
		total, kraft := 0, 0.0
		for length := 1; length <= 16; length++ {
			total += int(spec.counts[length])
			kraft += float64(spec.counts[length]) / float64(uint(1)<<length)
		}
		if total != used || kraft >= 1 {
			t.Fatalf("%s: %d codes, Kraft sum %f", name, total, kraft)
		}
		codes := spec.codes()
		for _, symbol := range spec.symbols {
			length := codes.length[symbol]
			if length < 1 || length > 16 || codes.code[symbol] == 1<<length-1 {
				t.Fatalf("%s: symbol %d code %b/%d", name, symbol, codes.code[symbol], length)
			}
		}
		segment := append([]byte{0x10}, spec.counts[1:]...)
		segment = append(segment, spec.symbols...)
		var decoder extendedProcess4Decoder
		if err := decoder.parseDHT(segment); err != nil {
			t.Fatalf("%s: decoder rejects table: %v", name, err)
		}
	}
}

func BenchmarkExtendedEncoder512(b *testing.B) {
	frame := extendedPhantomFrame(512, 512, 12, 1)
	metadata := extendedEncoderMetadata(512, 512, 12)
	encoder, _ := NewExtendedEncoder(DefaultQuality)
	b.SetBytes(int64(len(frame)))
	for i := 0; i < b.N; i++ {
		if _, err := encoder.EncodeFrame(context.Background(), frame, metadata); err != nil {
			b.Fatal(err)
		}
	}
}
