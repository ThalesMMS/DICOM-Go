package jpeglossless

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/bits"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// Encoder writes JPEG Lossless Process 14 (T.81 Annex H) frames: one
// interleaved scan with the configured predictor (selection value), point
// transform 0 and Huffman tables optimized for each frame and component.
// It is implemented from the standard and safe for concurrent use.
type Encoder struct {
	uid            string
	selectionValue int
	// faultyPredictor6 replaces predictor 6 inside the image; tests use it
	// to reproduce defective encoders.
	faultyPredictor6 func(a, b, c int32) int32
	// cornell reproduces the Cornell encoder bug: category 16 is followed by
	// 16 extension bits like the other categories (T.81 H.1.2.2 has none).
	cornell bool
}

var _ pixeldata.FrameEncoder = (*Encoder)(nil)

// NewSV1Encoder returns the encoder of JPEG Lossless Process 14 SV1
// (1.2.840.10008.1.2.4.70), predictor 1.
func NewSV1Encoder() *Encoder {
	return &Encoder{uid: UIDProcess14SV1, selectionValue: 1}
}

// NewProcess14Encoder returns the encoder of JPEG Lossless Process 14
// (1.2.840.10008.1.2.4.57) with a selection value from 1 through 7.
func NewProcess14Encoder(selectionValue int) (*Encoder, error) {
	if selectionValue < 1 || selectionValue > 7 {
		return nil, unsupportedEncoderMetadata("SelectionValue")
	}
	return &Encoder{uid: UIDProcess14, selectionValue: selectionValue}, nil
}

// RegisterEncoder registers the SV1 encoder for .70 and a predictor 1
// encoder for .57 in registry.
func RegisterEncoder(registry pixeldata.EncoderRegistry) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	if err := registry.RegisterEncoder(UIDProcess14SV1, NewSV1Encoder()); err != nil {
		return err
	}
	process14, _ := NewProcess14Encoder(1)
	return registry.RegisterEncoder(UIDProcess14, process14)
}

// Capabilities describes the native frames Encoder accepts.
func (e *Encoder) Capabilities() pixeldata.EncoderCapabilities {
	return pixeldata.EncoderCapabilities{
		TransferSyntaxUID:          e.transferSyntax(),
		BitsAllocated:              []uint16{8, 16},
		PixelRepresentations:       []uint16{0, 1},
		SamplesPerPixel:            []uint16{1, 3},
		PhotometricInterpretations: []string{"MONOCHROME1", "MONOCHROME2", "RGB", "YBR_FULL"},
		Lossless:                   true,
		SupportsMultiFrame:         true,
		Backend:                    "pure-go",
	}
}

func (e *Encoder) transferSyntax() string {
	if e == nil || e.uid == "" {
		return UIDProcess14SV1
	}
	return e.uid
}

func (e *Encoder) predictor() int {
	if e == nil || e.selectionValue == 0 {
		return 1
	}
	return e.selectionValue
}

// EncodeFrame encodes one little-endian, sample-interleaved native frame.
// Signed samples are coded as their Bits Stored two's complement bits, as the
// decoder returns them.
func (e *Encoder) EncodeFrame(ctx context.Context, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if err := validateEncoderMetadata(metadata); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	width, height := int(metadata.Columns), int(metadata.Rows)
	components := int(metadata.SamplesPerPixel)
	bytesPerSample := int(metadata.BitsAllocated / 8)
	if uint64(len(frame)) != uint64(width)*uint64(height)*uint64(components)*uint64(bytesPerSample) {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: native frame length", pixeldata.ErrPixelDataSizeMismatch)
	}
	precision := int(metadata.BitsStored)
	mask := int32(1)<<precision - 1
	// Differences and their categories, per component.
	predictor := e.predictor()
	defaultPrediction := int32(1) << (precision - 1)
	// Store the signed 16-bit difference and its category in one word. The
	// entropy pass reuses that category without another magnitude calculation.
	differences := make([][]uint32, components)
	frequencies := make([][17]int, components)
	for c := 0; c < components; c++ {
		diffs := make([]uint32, width*height)
		// Predictor 1 needs only the left sample and the preceding row's first
		// sample. Other predictors retain one row, never a second whole frame.
		var aboveRow []int32
		if predictor != 1 {
			aboveRow = make([]int32, width)
		}
		var aboveFirst int32
		for y := 0; y < height; y++ {
			if err := ctx.Err(); err != nil {
				return pixeldata.EncodedFrame{}, err
			}
			var left, aboveLeft int32
			if predictor == 1 {
				left = defaultPrediction
				if y != 0 {
					left = aboveFirst
				}
			}
			offset := (y*width*components + c) * bytesPerSample
			for x := 0; x < width; x++ {
				value := int32(frame[offset])
				if bytesPerSample == 2 {
					value = int32(binary.LittleEndian.Uint16(frame[offset:]))
				}
				value &= mask
				above := aboveFirst
				if aboveRow != nil {
					above = aboveRow[x]
				}
				prediction := left
				if predictor != 1 {
					prediction = defaultPrediction
					switch {
					case y == 0 && x == 0:
					case y == 0:
						prediction = left
					case x == 0:
						prediction = above
					default:
						prediction = predictNeighbors(left, above, aboveLeft, predictor)
						if e.faultyPredictor6 != nil && predictor == 6 {
							prediction = e.faultyPredictor6(left, above, aboveLeft)
						}
					}
				}
				difference := int32(int16(uint16(value - prediction)))
				category := differenceCategory(difference)
				diffs[y*width+x] = uint32(category)<<16 | uint32(uint16(difference))
				frequencies[c][category]++
				if aboveRow != nil {
					aboveRow[x] = value
				}
				if x == 0 {
					aboveFirst = value
				}
				left, aboveLeft = value, above
				offset += components * bytesPerSample
			}
		}
		differences[c] = diffs
		if err := ctx.Err(); err != nil {
			return pixeldata.EncodedFrame{}, err
		}
	}

	tables := make([]huffmanSpec, components)
	for c := range tables {
		tables[c] = optimalHuffmanSpec(frequencies[c])
	}

	out := make([]byte, 0, len(frame)/2+1024)
	out = append(out, 0xff, 0xd8) // SOI
	// SOF3: precision, dimensions and one 1x1 component per sample.
	sof := []byte{byte(precision), byte(height >> 8), byte(height), byte(width >> 8), byte(width), byte(components)}
	for c := 0; c < components; c++ {
		sof = append(sof, byte(c+1), 0x11, 0)
	}
	out = appendSegment(out, 0xc3, sof)
	var dht []byte
	for c, table := range tables {
		dht = append(dht, byte(c)) // class 0 (DC/lossless), table c
		dht = append(dht, table.counts[1:]...)
		dht = append(dht, table.symbols...)
	}
	out = appendSegment(out, 0xc4, dht)
	sos := []byte{byte(components)}
	for c := 0; c < components; c++ {
		sos = append(sos, byte(c+1), byte(c<<4))
	}
	sos = append(sos, byte(predictor), 0, 0) // Ss = predictor, Se = 0, Ah = Al = 0
	out = appendSegment(out, 0xda, sos)

	writer := entropyWriter{out: out}
	codes := make([]huffmanCodes, components)
	for c := range codes {
		codes[c] = tables[c].codes()
	}
	for i := 0; i < width*height; i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return pixeldata.EncodedFrame{}, err
			}
		}
		for c := 0; c < components; c++ {
			packed := differences[c][i]
			difference := int32(int16(packed))
			category := int(packed >> 16)
			code, length := codes[c].code[category], codes[c].length[category]
			if category > 0 && (category < 16 || e.cornell) {
				bits := difference
				if bits < 0 {
					bits--
				}
				code = code<<uint(category) | uint32(bits)&(1<<uint(category)-1)
				length += category
			}
			writer.write(code, length)
		}
	}
	out = writer.flush()
	out = append(out, 0xff, 0xd9) // EOI
	return pixeldata.EncodedFrame{Data: out}, nil
}

func validateEncoderMetadata(metadata pixeldata.Metadata) error {
	if metadata.Rows == 0 {
		return unsupportedEncoderMetadata("Rows")
	}
	if metadata.Columns == 0 {
		return unsupportedEncoderMetadata("Columns")
	}
	if metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16 {
		return unsupportedEncoderMetadata("BitsAllocated")
	}
	if metadata.BitsStored < 2 || metadata.BitsStored > metadata.BitsAllocated {
		return unsupportedEncoderMetadata("BitsStored")
	}
	if metadata.HighBit != metadata.BitsStored-1 {
		return unsupportedEncoderMetadata("HighBit")
	}
	if metadata.PixelRepresentation > 1 {
		return unsupportedEncoderMetadata("PixelRepresentation")
	}
	if metadata.NumberOfFrames < 1 {
		return unsupportedEncoderMetadata("NumberOfFrames")
	}
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	switch metadata.SamplesPerPixel {
	case 1:
		if photometric != "MONOCHROME1" && photometric != "MONOCHROME2" {
			return unsupportedEncoderMetadata("PhotometricInterpretation")
		}
	case 3:
		if photometric != "RGB" && photometric != "YBR_FULL" {
			return unsupportedEncoderMetadata("PhotometricInterpretation")
		}
		if metadata.PixelRepresentation != 0 {
			return unsupportedEncoderMetadata("PixelRepresentation")
		}
		if metadata.PlanarConfigurationPresent && metadata.PlanarConfiguration != 0 {
			return unsupportedEncoderMetadata("PlanarConfiguration")
		}
	default:
		return unsupportedEncoderMetadata("SamplesPerPixel")
	}
	// Each sample costs at most 16 + 15 bits; keep the frame within one
	// fragment.
	worst := uint64(metadata.Rows)*uint64(metadata.Columns)*uint64(metadata.SamplesPerPixel)*8 + 4096
	if worst > 1<<32-2 {
		return fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	return nil
}

func unsupportedEncoderMetadata(field string) error {
	return &pixeldata.UnsupportedEncoderMetadataError{Field: field}
}

// differenceCategory is SSSS (T.81 Table H.2): the bit length of |diff|,
// with 16 for the difference 32768 (-32768 modulo 2^16).
func differenceCategory(difference int32) int {
	if difference == -32768 {
		return 16
	}
	if difference < 0 {
		difference = -difference
	}
	return bits.Len32(uint32(difference))
}

func appendSegment(out []byte, marker byte, payload []byte) []byte {
	out = append(out, 0xff, marker, byte((len(payload)+2)>>8), byte(len(payload)+2))
	return append(out, payload...)
}

// huffmanSpec is a DHT table: counts of codes per length 1..16 and the
// symbols in code order.
type huffmanSpec struct {
	counts  [17]byte
	symbols []byte
}

type huffmanCodes struct {
	code   [17]uint32
	length [17]int
}

// optimalHuffmanSpec builds code lengths from symbol frequencies (T.81
// Annex K.2), reserving the all-ones code with a dummy symbol, and limits
// them to 16 bits (K.3 figure K.3). Unused symbols get no code.
func optimalHuffmanSpec(frequencies [17]int) huffmanSpec {
	const symbols = 17
	freq := make([]int, symbols+1)
	copy(freq, frequencies[:])
	freq[symbols] = 1 // reserved symbol: no code of all ones
	codesize := make([]int, symbols+1)
	others := make([]int, symbols+1)
	for i := range others {
		others[i] = -1
	}
	for {
		// The two least frequent entries, the larger index winning ties.
		v1, v2 := -1, -1
		for i, f := range freq {
			if f == 0 {
				continue
			}
			if v1 < 0 || f <= freq[v1] {
				v1 = i
			}
		}
		for i, f := range freq {
			if f == 0 || i == v1 {
				continue
			}
			if v2 < 0 || f <= freq[v2] {
				v2 = i
			}
		}
		if v2 < 0 {
			break
		}
		freq[v1] += freq[v2]
		freq[v2] = 0
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
	var bits [33]int
	for _, size := range codesize {
		if size > 0 {
			bits[size]++
		}
	}
	// Limit to 16 bits (K.3).
	for i := 32; i > 16; i-- {
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
	// Drop the reserved symbol's code (the longest).
	for i := 16; i > 0; i-- {
		if bits[i] > 0 {
			bits[i]--
			break
		}
	}
	var spec huffmanSpec
	for i := 1; i <= 16; i++ {
		spec.counts[i] = byte(bits[i])
	}
	// Symbols by increasing code size (ties by symbol value).
	order := make([]int, 0, symbols)
	for s := 0; s < symbols; s++ {
		if codesize[s] > 0 {
			order = append(order, s)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return codesize[order[a]] < codesize[order[b]] })
	for _, s := range order {
		spec.symbols = append(spec.symbols, byte(s))
	}
	return spec
}

// codes assigns canonical codes (T.81 Annex C) to the table's symbols.
func (s huffmanSpec) codes() huffmanCodes {
	var out huffmanCodes
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

// entropyWriter packs entropy-coded bits most significant first, stuffing a
// zero byte after every 0xFF (T.81 F.1.2.3) and padding with ones.
type entropyWriter struct {
	out   []byte
	acc   uint64
	count int
}

func (w *entropyWriter) write(value uint32, length int) {
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

func (w *entropyWriter) flush() []byte {
	if w.count > 0 {
		w.write(1<<uint(8-w.count)-1, 8-w.count)
	}
	return w.out
}
