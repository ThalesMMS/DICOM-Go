package jpeg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// extendedLuminanceQuantization is T.81 Table K.1 (luminance) in natural
// (row-major) order: the example table the quality setting scales.
var extendedLuminanceQuantization = [64]int{
	16, 11, 10, 16, 24, 40, 51, 61,
	12, 12, 14, 19, 26, 58, 60, 55,
	14, 13, 16, 24, 40, 57, 69, 56,
	14, 17, 22, 29, 51, 87, 80, 62,
	18, 22, 37, 56, 68, 109, 103, 77,
	24, 35, 55, 64, 81, 104, 113, 92,
	49, 64, 78, 87, 103, 121, 120, 101,
	72, 92, 95, 98, 112, 100, 103, 99,
}

// ExtendedEncoder is an immutable, concurrency-safe SOF1 encoder written
// from ITU-T T.81. The options constructor defaults to the unsigned mono8/12
// attribute profiles of PS3.5 Table 8.2.1-1. Color and the historical reduced
// precision mono path are separately selected compatibility profiles.
type ExtendedEncoder struct {
	quality int
	options ExtendedEncoderOptions
}

const (
	defaultExtendedWorkingBytes = int64(256) << 20
	defaultExtendedEncodedBytes = int64(128) << 20
)

var (
	ErrInvalidExtendedEncoderLimits = errors.New("dicom: invalid JPEG Extended encoder limits")
	ErrExtendedEncoderResourceLimit = errors.New("dicom: JPEG Extended encoder resource limit exceeded")
)

// ExtendedEncoderOptions selects an explicit SOF1 encoder profile. Quality
// must be 1..100. Zero byte limits use finite defaults; negative limits fail.
// MaxWorkingBytes bounds borrowed input plus the coefficient/output capacity
// and a fixed table allowance; it is not a process RSS measurement.
type ExtendedEncoderOptions struct {
	Quality int
	// AllowColorCompatibility enables unsigned interleaved RGB8/RGB12 and
	// YBR_FULL12, 4:4:4, preserving the supplied components. These color .51
	// outputs are outside PS3.5 Table 8.2.1-1 and report that profile.
	AllowColorCompatibility bool
	// AllowLegacyReducedPrecision enables historical mono9..11 at JPEG
	// precision12 with reduced BitsStored. The frame reports that profile.
	AllowLegacyReducedPrecision bool
	MaxWorkingBytes             int64
	MaxEncodedFrameBytes        int64
}

var _ pixeldata.FrameEncoder = (*ExtendedEncoder)(nil)

// NewExtendedEncoder preserves the historical mono9..12 constructor and
// additionally accepts standard mono8. Mono9..11 reports a compatibility
// profile. Use NewExtendedEncoderWithOptions for a strict mono8/12 default.
func NewExtendedEncoder(quality int) (*ExtendedEncoder, error) {
	return NewExtendedEncoderWithOptions(ExtendedEncoderOptions{Quality: quality, AllowLegacyReducedPrecision: true})
}

// NewExtendedEncoderWithOptions constructs an explicit encoder without
// implicit color transforms, subsampling, signed samples or reduced color.
func NewExtendedEncoderWithOptions(options ExtendedEncoderOptions) (*ExtendedEncoder, error) {
	if options.Quality < MinQuality || options.Quality > MaxQuality {
		return nil, ErrInvalidQuality
	}
	if options.MaxWorkingBytes < 0 || options.MaxEncodedFrameBytes < 0 {
		return nil, ErrInvalidExtendedEncoderLimits
	}
	if options.MaxWorkingBytes == 0 {
		options.MaxWorkingBytes = defaultExtendedWorkingBytes
	}
	if options.MaxEncodedFrameBytes == 0 {
		options.MaxEncodedFrameBytes = defaultExtendedEncodedBytes
	}
	return &ExtendedEncoder{quality: options.Quality, options: options}, nil
}

// RegisterExtendedEncoder registers the historical constructor explicitly.
func RegisterExtendedEncoder(registry pixeldata.EncoderRegistry, quality int) error {
	return RegisterExtendedEncoderWithOptions(registry, ExtendedEncoderOptions{Quality: quality, AllowLegacyReducedPrecision: true})
}

// RegisterExtendedEncoderWithOptions registers the selected strict or
// compatibility profile. Registration never authorizes a lossy transcode;
// TranscodeOptions.AllowLossy remains separately required.
func RegisterExtendedEncoderWithOptions(registry pixeldata.EncoderRegistry, options ExtendedEncoderOptions) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	encoder, err := NewExtendedEncoderWithOptions(options)
	if err != nil {
		return err
	}
	return registry.RegisterEncoder(UIDExtended, encoder)
}

// Quality reports the configured quality.
func (e *ExtendedEncoder) Quality() int {
	if e == nil {
		return 0
	}
	return e.quality
}

// Capabilities reports only the selected input and compatibility profiles.
// Precise combinations of BitsStored and photometric are checked per frame.
func (e *ExtendedEncoder) Capabilities() pixeldata.EncoderCapabilities {
	caps := pixeldata.EncoderCapabilities{
		TransferSyntaxUID:          UIDExtended,
		BitsAllocated:              []uint16{8, 16},
		PixelRepresentations:       []uint16{0},
		SamplesPerPixel:            []uint16{1},
		PhotometricInterpretations: []string{"MONOCHROME1", "MONOCHROME2"},
		Lossless:                   false, LossyMethod: jpegLossyMethod,
		SupportsMultiFrame: true, Backend: "pure-go",
	}
	if e != nil && e.options.AllowColorCompatibility {
		caps.SamplesPerPixel = append(caps.SamplesPerPixel, 3)
		caps.PhotometricInterpretations = append(caps.PhotometricInterpretations, "RGB", "YBR_FULL")
		caps.CompatibilityProfiles = append(caps.CompatibilityProfiles, pixeldata.EncoderCompatibilityJPEGExtendedColor)
	}
	if e != nil && e.options.AllowLegacyReducedPrecision {
		caps.CompatibilityProfiles = append(caps.CompatibilityProfiles, pixeldata.EncoderCompatibilityJPEGExtendedReducedPrecision)
	}
	return caps
}

// EncodeFrame encodes one borrowed, canonical little-endian native frame.
// The frame owns its output; bits above HighBit are ignored.
func (e *ExtendedEncoder) EncodeFrame(ctx context.Context, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if e == nil || e.quality < MinQuality || e.quality > MaxQuality {
		return pixeldata.EncodedFrame{}, ErrInvalidQuality
	}
	profile, precision, err := e.validateInput(metadata)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	frameLength := uint64(metadata.Rows) * uint64(metadata.Columns) * uint64(metadata.SamplesPerPixel) * uint64(metadata.BytesPerSample())
	if uint64(len(frame)) != frameLength {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: native frame length", pixeldata.ErrPixelDataSizeMismatch)
	}
	width, height, components := int(metadata.Columns), int(metadata.Rows), int(metadata.SamplesPerPixel)
	blocksX, blocksY := (width+7)/8, (height+7)/8
	blockCount := uint64(blocksX) * uint64(blocksY) * uint64(components)
	// At precision12: DC <= 16 Huffman + 15 magnitude bits; each of 63
	// ACs <= 16 + 14 bits. Including zero runs, flush and byte stuffing,
	// 512 bytes per block is conservative; 4096 covers all headers.
	encodedCapacity := blockCount*512 + 4096
	workingBytes := frameLength + blockCount*128 + encodedCapacity + 65536
	maxInt := uint64(^uint(0) >> 1)
	if encodedCapacity > uint64(e.options.MaxEncodedFrameBytes) || encodedCapacity > maxInt {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: encoded_frame_bytes", ErrExtendedEncoderResourceLimit)
	}
	if workingBytes > uint64(e.options.MaxWorkingBytes) || blockCount*64 > maxInt {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: working_bytes", ErrExtendedEncoderResourceLimit)
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	blocks := make([]int16, int(blockCount)*64)
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	quant := extendedQuantizationTable(e.quality)
	if precision == 8 {
		quant = baselineQuantization(extendedLuminanceQuantization, e.quality)
	}
	mask := uint16(1)<<metadata.BitsStored - 1
	center := 1 << (precision - 1)
	bytesPerSample := metadata.BytesPerSample()
	var dcFrequencies, acFrequencies [256]int
	var predictors [3]int
	for by := 0; by < blocksY; by++ {
		for bx := 0; bx < blocksX; bx++ {
			if bx%128 == 0 {
				if err := ctx.Err(); err != nil {
					return pixeldata.EncodedFrame{}, err
				}
			}
			for c := 0; c < components; c++ {
				var block [64]float64
				for y := 0; y < 8; y++ {
					row := min(by*8+y, height-1) * width
					for x := 0; x < 8; x++ {
						offset := ((row+min(bx*8+x, width-1))*components + c) * bytesPerSample
						value := uint16(frame[offset])
						if bytesPerSample == 2 {
							value = binary.LittleEndian.Uint16(frame[offset:])
						}
						block[y*8+x] = float64(int(value&mask) - center)
					}
				}
				coefficients := extendedForwardDCT(&block)
				index := ((by*blocksX+bx)*components + c) * 64
				zigzag := blocks[index:][:64]
				for zig := 0; zig < 64; zig++ {
					natural := extendedZigZag[zig]
					zigzag[zig] = int16(math.Round(coefficients[natural] / float64(quant[natural])))
				}
				difference := int(zigzag[0]) - predictors[c]
				predictors[c] = int(zigzag[0])
				dcFrequencies[extendedMagnitudeCategory(difference)]++
				forEachExtendedACSymbol(zigzag, func(symbol byte, _ int) { acFrequencies[symbol]++ })
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	dcTable := extendedOptimalHuffmanTable(dcFrequencies)
	acTable := extendedOptimalHuffmanTable(acFrequencies)
	// Capacity is the admitted worst case, so entropy appends never reallocate.
	out := make([]byte, 0, int(encodedCapacity))
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	out = append(out, 0xff, 0xd8)
	dqt := []byte{0}
	for _, value := range quant {
		if value > 255 {
			dqt[0] = 0x10
			break
		}
	}
	for zig := 0; zig < 64; zig++ {
		value := quant[extendedZigZag[zig]]
		if dqt[0] == 0 {
			dqt = append(dqt, byte(value))
		} else {
			dqt = binary.BigEndian.AppendUint16(dqt, uint16(value))
		}
	}
	out = appendExtendedSegment(out, 0xdb, dqt)
	sof := []byte{byte(precision), byte(height >> 8), byte(height), byte(width >> 8), byte(width), byte(components)}
	sos := []byte{byte(components)}
	ids := [3]byte{1, 2, 3}
	if components == 3 && normalizedEncoderPhotometric(metadata.PhotometricInterpretation) == "RGB" {
		ids = [3]byte{'R', 'G', 'B'}
	}
	for c := 0; c < components; c++ {
		sof = append(sof, ids[c], 0x11, 0)
		sos = append(sos, ids[c], 0)
	}
	sos = append(sos, 0, 63, 0)
	out = appendExtendedSegment(out, 0xc1, sof)
	dht := append([]byte{0}, dcTable.counts[1:]...)
	dht = append(dht, dcTable.symbols...)
	dht = append(dht, 0x10)
	dht = append(dht, acTable.counts[1:]...)
	dht = append(dht, acTable.symbols...)
	out = appendExtendedSegment(out, 0xc4, dht)
	out = appendExtendedSegment(out, 0xda, sos)
	dcCodes, acCodes := dcTable.codes(), acTable.codes()
	writer := extendedBitWriter{out: out}
	predictors = [3]int{}
	for i := 0; i < int(blockCount); i++ {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return pixeldata.EncodedFrame{}, err
			}
		}
		c := i % components
		zigzag := blocks[i*64:][:64]
		difference := int(zigzag[0]) - predictors[c]
		predictors[c] = int(zigzag[0])
		category := extendedMagnitudeCategory(difference)
		writer.write(dcCodes.code[category], dcCodes.length[category])
		writer.writeAdditionalBits(difference, category)
		forEachExtendedACSymbol(zigzag, func(symbol byte, value int) {
			writer.write(acCodes.code[symbol], acCodes.length[symbol])
			writer.writeAdditionalBits(value, int(symbol&0x0f))
		})
	}
	out = append(writer.flush(), 0xff, 0xd9)
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if uint64(len(out)) > encodedCapacity {
		return pixeldata.EncodedFrame{}, pixeldata.ErrEncoderOutputInvalid
	}
	result := pixeldata.EncodedFrame{Data: out, CompatibilityProfile: profile}
	if components == 3 {
		planar := uint16(0)
		result.PlanarConfiguration = &planar
		result.PhotometricInterpretation = normalizedEncoderPhotometric(metadata.PhotometricInterpretation)
	}
	return result, nil
}

func normalizedEncoderPhotometric(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func (e *ExtendedEncoder) validateInput(m pixeldata.Metadata) (pixeldata.EncoderCompatibilityProfile, int, error) {
	fail := func(field string) (pixeldata.EncoderCompatibilityProfile, int, error) {
		return "", 0, unsupportedJPEGEncoderMetadata(field)
	}
	if m.Rows == 0 {
		return fail("Rows")
	}
	if m.Columns == 0 {
		return fail("Columns")
	}
	if m.NumberOfFrames < 1 {
		return fail("NumberOfFrames")
	}
	if m.PixelRepresentation != 0 {
		return fail("PixelRepresentation")
	}
	precision := 12
	if m.BitsAllocated == 8 {
		precision = 8
		if m.BitsStored != 8 {
			return fail("BitsStored")
		}
	} else if m.BitsAllocated != 16 {
		return fail("BitsAllocated")
	} else if m.BitsStored < 9 || m.BitsStored > 12 {
		return fail("BitsStored")
	}
	if m.HighBit != m.BitsStored-1 {
		return fail("HighBit")
	}
	pi := normalizedEncoderPhotometric(m.PhotometricInterpretation)
	if m.SamplesPerPixel == 1 {
		if pi != "MONOCHROME1" && pi != "MONOCHROME2" {
			return fail("PhotometricInterpretation")
		}
		if m.PlanarConfigurationPresent {
			return fail("PlanarConfiguration")
		}
		if precision == 12 && m.BitsStored != 12 {
			if !e.options.AllowLegacyReducedPrecision {
				return fail("BitsStored")
			}
			return pixeldata.EncoderCompatibilityJPEGExtendedReducedPrecision, precision, nil
		}
		return "", precision, nil
	}
	if m.SamplesPerPixel != 3 {
		return fail("SamplesPerPixel")
	}
	if !e.options.AllowColorCompatibility {
		return fail("ColorCompatibility")
	}
	if pi != "RGB" && !(precision == 12 && pi == "YBR_FULL") {
		return fail("PhotometricInterpretation")
	}
	if m.BitsStored != uint16(precision) {
		return fail("BitsStored")
	}
	if !m.PlanarConfigurationPresent || m.PlanarConfiguration != 0 {
		return fail("PlanarConfiguration")
	}
	return pixeldata.EncoderCompatibilityJPEGExtendedColor, precision, nil
}

// extendedQuantizationTable scales Table K.1 by quality and limits the values
// to the 16-bit range precision 12 allows (T.81 B.2.4.1).
func extendedQuantizationTable(quality int) [64]int {
	scale := 200 - 2*quality
	if quality < 50 {
		scale = 5000 / quality
	}
	var table [64]int
	for i, base := range extendedLuminanceQuantization {
		table[i] = min(max((base*scale+50)/100, 1), 32767)
	}
	return table
}

// extendedForwardDCT is the 8x8 FDCT of T.81 A.3.3,
// F(u,v) = 1/4 C(u) C(v) sum_x sum_y s(x,y) cos((2x+1)u pi/16) cos((2y+1)v pi/16),
// computed separably. Input and output are natural order, row index y or v.
func extendedForwardDCT(block *[64]float64) [64]float64 {
	var rows, out [64]float64
	for y := 0; y < 8; y++ {
		for u := 0; u < 8; u++ {
			sum := 0.0
			for x := 0; x < 8; x++ {
				sum += block[y*8+x] * extendedCos[u][x]
			}
			rows[y*8+u] = sum
		}
	}
	for v := 0; v < 8; v++ {
		for u := 0; u < 8; u++ {
			sum := 0.0
			for y := 0; y < 8; y++ {
				sum += rows[y*8+u] * extendedCos[v][y]
			}
			scale := 0.25
			if u == 0 {
				scale /= math.Sqrt2
			}
			if v == 0 {
				scale /= math.Sqrt2
			}
			out[v*8+u] = sum * scale
		}
	}
	return out
}

// forEachExtendedACSymbol walks the AC coefficients of a zig-zag block as
// T.81 Figure F.2 codes them: RRRRSSSS symbols with their values, ZRL (0xF0)
// for sixteen zeros and EOB (0x00) after the last nonzero coefficient.
func forEachExtendedACSymbol(zigzag []int16, emit func(symbol byte, value int)) {
	run := 0
	for k := 1; k < 64; k++ {
		value := int(zigzag[k])
		if value == 0 {
			run++
			continue
		}
		for run > 15 {
			emit(0xf0, 0)
			run -= 16
		}
		emit(byte(run<<4|extendedMagnitudeCategory(value)), value)
		run = 0
	}
	if run > 0 {
		emit(0x00, 0)
	}
}

// extendedMagnitudeCategory is SSSS (T.81 Tables F.1 and F.2): the bit length
// of |value|.
func extendedMagnitudeCategory(value int) int {
	if value < 0 {
		value = -value
	}
	category := 0
	for value > 0 {
		category++
		value >>= 1
	}
	return category
}

func appendExtendedSegment(out []byte, marker byte, payload []byte) []byte {
	out = append(out, 0xff, marker, byte((len(payload)+2)>>8), byte(len(payload)+2))
	return append(out, payload...)
}

// extendedHuffmanSpec is a DHT table: the number of codes of each length 1..16
// (BITS) and the symbols in code order (HUFFVAL).
type extendedHuffmanSpec struct {
	counts  [17]byte
	symbols []byte
}

type extendedHuffmanCodes struct {
	code   [256]uint32
	length [256]int
}

// extendedOptimalHuffmanTable builds a Huffman table from symbol frequencies
// by T.81 Annex K.2: code sizes (Figure K.1) with a reserved one-count symbol
// so that no code is all ones, BITS (Figure K.2) limited to 16 bits (Figure
// K.3) and HUFFVAL sorted by code size (Figure K.4). Unused symbols get no
// code.
func extendedOptimalHuffmanTable(frequencies [256]int) extendedHuffmanSpec {
	const reserved = 256
	var freq [257]int
	copy(freq[:], frequencies[:])
	freq[reserved] = 1
	var codesize [257]int
	var others [257]int
	for i := range others {
		others[i] = -1
	}
	for {
		// V1 and V2 are the two least nonzero frequencies, the larger symbol
		// winning ties.
		v1, v2 := -1, -1
		for v, f := range freq {
			if f > 0 && (v1 < 0 || f <= freq[v1]) {
				v1 = v
			}
		}
		for v, f := range freq {
			if f > 0 && v != v1 && (v2 < 0 || f <= freq[v2]) {
				v2 = v
			}
		}
		if v2 < 0 {
			break
		}
		freq[v1] += freq[v2]
		freq[v2] = 0
		// Every symbol of both branches gets one bit longer; V2's chain is
		// appended to V1's.
		codesize[v1]++
		for others[v1] >= 0 {
			v1 = others[v1]
			codesize[v1]++
		}
		others[v1] = v2
		codesize[v2]++
		for others[v2] >= 0 {
			v2 = others[v2]
			codesize[v2]++
		}
	}
	// The unlimited tree has at most 257 leaves (256 symbols and the
	// reserved symbol), so a leaf can reach depth 256 before length limiting.
	var bits [257]int
	for _, size := range codesize {
		if size > 0 {
			bits[size]++
		}
	}
	for i := len(bits) - 1; i > 16; i-- {
		for bits[i] > 0 {
			j := i - 2
			for bits[j] == 0 {
				j--
			}
			bits[i] -= 2
			bits[i-1]++
			bits[j+1] += 2
			bits[j]--
		}
	}
	// The reserved symbol sorts last among the longest codes; drop it.
	for i := 16; i > 0; i-- {
		if bits[i] > 0 {
			bits[i]--
			break
		}
	}
	var spec extendedHuffmanSpec
	for i := 1; i <= 16; i++ {
		spec.counts[i] = byte(bits[i])
	}
	for size := 1; size < len(bits); size++ {
		for symbol := 0; symbol < reserved; symbol++ {
			if codesize[symbol] == size {
				spec.symbols = append(spec.symbols, byte(symbol))
			}
		}
	}
	return spec
}

// codes assigns the canonical codes of T.81 Annex C.
func (s extendedHuffmanSpec) codes() extendedHuffmanCodes {
	var out extendedHuffmanCodes
	code := uint32(0)
	k := 0
	for length := 1; length <= 16; length++ {
		for i := 0; i < int(s.counts[length]); i++ {
			symbol := s.symbols[k]
			out.code[symbol] = code
			out.length[symbol] = length
			code++
			k++
		}
		code <<= 1
	}
	return out
}

// extendedBitWriter packs entropy-coded bits most significant first,
// stuffing a zero byte after every 0xFF (T.81 F.1.2.3) and padding the final
// byte with ones (F.1.2.3).
type extendedBitWriter struct {
	out   []byte
	acc   uint64
	count int
}

func (w *extendedBitWriter) write(value uint32, length int) {
	if length == 0 {
		return
	}
	w.acc = w.acc<<uint(length) | uint64(value&(1<<uint(length)-1))
	w.count += length
	for w.count >= 8 {
		b := byte(w.acc >> uint(w.count-8))
		w.out = append(w.out, b)
		if b == 0xff {
			w.out = append(w.out, 0)
		}
		w.count -= 8
	}
}

// writeAdditionalBits appends the SSSS low-order bits of value, of value-1
// when it is negative (T.81 F.1.2.1 and F.1.2.2).
func (w *extendedBitWriter) writeAdditionalBits(value, category int) {
	if category == 0 {
		return
	}
	if value < 0 {
		value--
	}
	w.write(uint32(value)&(1<<uint(category)-1), category)
}

func (w *extendedBitWriter) flush() []byte {
	if w.count > 0 {
		w.write(1<<uint(8-w.count)-1, 8-w.count)
	}
	return w.out
}
