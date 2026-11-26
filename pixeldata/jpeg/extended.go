package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

const (
	extendedProcess4Precision       = 12
	extendedProcess4MaxDecodedBytes = uint64(512 << 20)
)

var extendedZigZag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

var extendedCos = func() [8][8]float64 {
	var table [8][8]float64
	for frequency := 0; frequency < 8; frequency++ {
		for sample := 0; sample < 8; sample++ {
			table[frequency][sample] = math.Cos(float64((2*sample+1)*frequency) * math.Pi / 16)
		}
	}
	return table
}()

type extendedHuffmanTable struct {
	counts  [17]int
	symbols []byte
	valid   bool
}

type extendedProcess4Decoder struct {
	data []byte
	pos  int
	ctx  context.Context

	width             int
	height            int
	precision         int
	components        []extendedComponent
	hmax, vmax        int
	packedMono        []byte // Final little-endian raster for a strict mono SOF1 scan.
	paddedRasterBytes uint64
	reservedBytes     uint64 // Compressed input and other frames in the request.
	// adobeTransform is the APP14 Adobe color transform (-1 when absent);
	// jfif reports an APP0 JFIF marker.
	adobeTransform int
	jfif           bool

	// tolerateScanParameters accepts non-sequential Ss/Se/Ah/Al fields
	// (DecodeRecovery.JPEGSequentialScanQuirks); scanParametersIgnored
	// reports that it was needed.
	tolerateScanParameters bool
	scanParametersIgnored  bool

	quant        [4][64]int
	quantDefined [4]bool
	huffman      [2][4]extendedHuffmanTable
	restart      int
	seenDRI      bool
	seenSOF      bool
	seenSOS      bool
}

// extendedComponent is one frame component and its decoded sample plane,
// padded to whole MCUs.
type extendedComponent struct {
	id              byte
	h, v            int
	quantID         byte
	blocksPerLine   int
	blocksPerColumn int
	samples         []uint16
	dcPredictor     int
	scanned         bool
}

func validateExtendedProcess4Request(metadata pixeldata.Metadata, fragments [][]byte) error {
	frames := uint64(metadata.NumberOfFrames)
	if metadata.NumberOfFrames <= 0 || len(fragments) == 0 {
		return fmt.Errorf("%w: invalid JPEG Extended frame count", ErrInvalidFragment)
	}
	pixelsPerFrame := uint64(metadata.Rows) * uint64(metadata.Columns)
	if pixelsPerFrame > ^uint64(0)/8 {
		return fmt.Errorf("%w: JPEG Extended output size overflow", ErrInvalidFragment)
	}
	samples := uint64(max(metadata.SamplesPerPixel, 1))
	outputPerFrame := pixelsPerFrame * 2 * samples
	if outputPerFrame > ^uint64(0)/frames {
		return fmt.Errorf("%w: JPEG Extended request output overflow", ErrInvalidFragment)
	}
	total := outputPerFrame * frames

	wordBytes := uint64(32<<(^uint(0)>>63)) / 8
	// Account for both the retained input fragment headers and the output frame
	// headers. Each []byte occupies three machine words.
	if wordBytes == 0 || frames > ^uint64(0)/(6*wordBytes) {
		return fmt.Errorf("%w: JPEG Extended request overhead overflow", ErrInvalidFragment)
	}
	var ok bool
	total, ok = addExtendedRequestBytes(total, frames*6*wordBytes)
	if !ok {
		return fmt.Errorf("%w: JPEG Extended request overhead overflow", ErrInvalidFragment)
	}
	for _, fragment := range fragments {
		total, ok = addExtendedRequestBytes(total, uint64(len(fragment)))
		if !ok {
			return fmt.Errorf("%w: JPEG Extended request size overflow", ErrInvalidFragment)
		}
		if total > extendedProcess4MaxDecodedBytes {
			return fmt.Errorf("%w: JPEG Extended request exceeds resource limit", ErrInvalidFragment)
		}
	}
	if total > extendedProcess4MaxDecodedBytes {
		return fmt.Errorf("%w: JPEG Extended request exceeds resource limit", ErrInvalidFragment)
	}
	return nil
}

func addExtendedRequestBytes(total, addition uint64) (uint64, bool) {
	if total > ^uint64(0)-addition {
		return 0, false
	}
	return total + addition, true
}

func jpegFrameSOFMarker(ctx context.Context, stream []byte) (byte, error) {
	if len(stream) < 4 || stream[0] != 0xff || stream[1] != 0xd8 {
		return 0, fmt.Errorf("missing JPEG SOI")
	}
	for pos := 2; pos < len(stream); {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if stream[pos] != 0xff {
			return 0, fmt.Errorf("expected JPEG marker at byte %d", pos)
		}
		for pos < len(stream) && stream[pos] == 0xff {
			pos++
			if pos&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
			}
		}
		if pos >= len(stream) || stream[pos] == 0 {
			return 0, fmt.Errorf("truncated or stuffed JPEG marker")
		}
		marker := stream[pos]
		pos++
		switch marker {
		case 0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
			return marker, nil
		case 0xd8, 0xd9, 0xda:
			return 0, fmt.Errorf("JPEG frame has no SOF marker")
		case 0xd0, 0xd1, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7:
			return 0, fmt.Errorf("JPEG restart marker before SOF")
		}
		if pos+2 > len(stream) {
			return 0, fmt.Errorf("truncated JPEG segment length")
		}
		length := int(binary.BigEndian.Uint16(stream[pos:]))
		if length < 2 || pos+length > len(stream) {
			return 0, fmt.Errorf("invalid JPEG segment length")
		}
		pos += length
	}
	return 0, fmt.Errorf("JPEG frame has no SOF marker")
}

func decodeExtendedProcess4(stream []byte, metadata pixeldata.Metadata) ([]byte, error) {
	out, _, _, err := decodeExtendedProcess4Options(context.Background(), stream, metadata, false, 0)
	return out, err
}

// decodeExtendedProcess4Options decodes with the scan-parameter tolerance
// and also reports whether it was used.
func decodeExtendedProcess4Options(ctx context.Context, stream []byte, metadata pixeldata.Metadata, tolerateScanParameters bool, reservedBytes uint64) ([]byte, bool, bool, error) {
	d := extendedProcess4Decoder{
		data: stream, pos: 2, ctx: ctx, adobeTransform: -1,
		tolerateScanParameters: tolerateScanParameters,
		reservedBytes:          max(reservedBytes, uint64(len(stream))),
	}
	out, ycbcr, err := d.decode(metadata)
	return out, ycbcr, d.scanParametersIgnored, err
}

// decode decodes one 12-bit SOF1 frame to interleaved little-endian samples
// and reports whether a three-component codestream is YCbCr by the libjpeg
// rule (codestreamIsYCbCr).
func (d *extendedProcess4Decoder) decode(metadata pixeldata.Metadata) ([]byte, bool, error) {
	stream := d.data
	if uint64(len(stream)) > extendedProcess4MaxDecodedBytes {
		return nil, false, fmt.Errorf("JPEG Extended compressed frame exceeds resource limit")
	}
	if len(stream) < 4 || stream[0] != 0xff || stream[1] != 0xd8 {
		return nil, false, fmt.Errorf("missing JPEG SOI")
	}
	for {
		if err := d.checkContext(); err != nil {
			return nil, false, err
		}
		marker, err := d.nextMarker()
		if err != nil {
			return nil, false, err
		}
		switch marker {
		case 0xc1: // SOF1: extended sequential DCT, Huffman coding.
			segment, err := d.readSegment()
			if err != nil {
				return nil, false, err
			}
			if err := d.parseSOF1(segment, metadata); err != nil {
				return nil, false, err
			}
		case 0xc4: // DHT
			segment, err := d.readSegment()
			if err != nil {
				return nil, false, err
			}
			if err := d.parseDHT(segment); err != nil {
				return nil, false, err
			}
		case 0xdb: // DQT
			segment, err := d.readSegment()
			if err != nil {
				return nil, false, err
			}
			if err := d.parseDQT(segment); err != nil {
				return nil, false, err
			}
		case 0xdd: // DRI
			if d.seenDRI || d.allComponentsScanned() {
				return nil, false, fmt.Errorf("duplicate or out-of-order JPEG DRI")
			}
			segment, err := d.readSegment()
			if err != nil {
				return nil, false, err
			}
			if len(segment) != 2 {
				return nil, false, fmt.Errorf("invalid JPEG DRI length")
			}
			d.restart = int(binary.BigEndian.Uint16(segment))
			d.seenDRI = true
		case 0xda: // SOS
			segment, err := d.readSegment()
			if err != nil {
				return nil, false, err
			}
			if err := d.decodeSOS(segment); err != nil {
				return nil, false, err
			}
		case 0xd9: // EOI
			if !d.seenSOF || !d.seenSOS {
				return nil, false, fmt.Errorf("incomplete JPEG Extended Process 4 frame")
			}
			for _, component := range d.components {
				if !component.scanned {
					return nil, false, fmt.Errorf("incomplete JPEG Extended Process 4 frame: component %d not scanned", component.id)
				}
			}
			trailing := d.data[d.pos:]
			// DICOM requires even-length Items and permits the final compressed
			// fragment to carry one padding byte; deployed writers do not agree
			// on that byte's value.
			if len(trailing) > 1 {
				return nil, false, fmt.Errorf("trailing data after JPEG EOI")
			}
			out := d.output()
			if err := d.checkContext(); err != nil {
				return nil, false, err
			}
			return out, d.codestreamIsYCbCr(), nil
		case 0xe0, 0xee:
			segment, err := d.readSegment()
			if err != nil {
				return nil, false, err
			}
			if marker == 0xe0 && len(segment) >= 5 && string(segment[:5]) == "JFIF\x00" {
				d.jfif = true
			}
			if marker == 0xee && len(segment) >= 12 && string(segment[:5]) == "Adobe" {
				d.adobeTransform = int(segment[11])
			}
		case 0xe1, 0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7,
			0xe8, 0xe9, 0xea, 0xeb, 0xec, 0xed, 0xef, 0xfe:
			if _, err := d.readSegment(); err != nil {
				return nil, false, err
			}
		case 0xd0, 0xd1, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7:
			return nil, false, fmt.Errorf("JPEG restart marker outside entropy data")
		default:
			return nil, false, fmt.Errorf("unsupported JPEG marker 0xff%02x", marker)
		}
	}
}

func (d *extendedProcess4Decoder) checkContext() error {
	if d.ctx != nil {
		return d.ctx.Err()
	}
	return nil
}

func (d *extendedProcess4Decoder) admitRasterPeak(rasterBytes uint64) error {
	limit := min(extendedProcess4MaxDecodedBytes, uint64(int(^uint(0)>>1)))
	if rasterBytes > limit || d.reservedBytes > limit-rasterBytes {
		return fmt.Errorf("JPEG Extended decoded frame exceeds resource limit")
	}
	return nil
}

func (d *extendedProcess4Decoder) nextMarker() (byte, error) {
	if d.pos >= len(d.data) || d.data[d.pos] != 0xff {
		return 0, fmt.Errorf("expected JPEG marker at byte %d", d.pos)
	}
	for d.pos < len(d.data) && d.data[d.pos] == 0xff {
		d.pos++
		if d.pos&4095 == 0 {
			if err := d.checkContext(); err != nil {
				return 0, err
			}
		}
	}
	if d.pos >= len(d.data) || d.data[d.pos] == 0 {
		return 0, fmt.Errorf("truncated or stuffed JPEG marker")
	}
	marker := d.data[d.pos]
	d.pos++
	return marker, nil
}

func (d *extendedProcess4Decoder) readSegment() ([]byte, error) {
	if d.pos+2 > len(d.data) {
		return nil, fmt.Errorf("truncated JPEG segment length")
	}
	length := int(binary.BigEndian.Uint16(d.data[d.pos:]))
	if length < 2 || d.pos+length > len(d.data) {
		return nil, fmt.Errorf("invalid JPEG segment length")
	}
	segment := d.data[d.pos+2 : d.pos+length]
	d.pos += length
	return segment, nil
}

func (d *extendedProcess4Decoder) parseSOF1(segment []byte, metadata pixeldata.Metadata) error {
	if d.seenSOF || d.seenSOS {
		return fmt.Errorf("duplicate or out-of-order JPEG SOF1")
	}
	if len(segment) < 6 {
		return fmt.Errorf("truncated JPEG SOF1")
	}
	count := int(segment[5])
	if (count != 1 && count != 3) || len(segment) != 6+3*count {
		return fmt.Errorf("JPEG Extended Process 4 requires one or three components")
	}
	if metadata.SamplesPerPixel != 0 && count != int(metadata.SamplesPerPixel) {
		return fmt.Errorf("JPEG Extended components=%d, SamplesPerPixel=%d", count, metadata.SamplesPerPixel)
	}
	precision := int(segment[0])
	height := int(binary.BigEndian.Uint16(segment[1:3]))
	width := int(binary.BigEndian.Uint16(segment[3:5]))
	if precision != extendedProcess4Precision {
		return fmt.Errorf("JPEG Extended Process 4 precision=%d, want 12", precision)
	}
	if height == 0 || width == 0 {
		return fmt.Errorf("invalid JPEG Extended dimensions")
	}
	if height != int(metadata.Rows) || width != int(metadata.Columns) {
		return fmt.Errorf("%w: JPEG=%dx%d metadata=%dx%d", ErrImageSizeMismatch, width, height, metadata.Columns, metadata.Rows)
	}
	d.components = make([]extendedComponent, count)
	for i := range d.components {
		c := segment[6+3*i:]
		h, v := int(c[1]>>4), int(c[1]&0x0f)
		if h < 1 || h > 4 || v < 1 || v > 4 || c[2] > 3 {
			return fmt.Errorf("unsupported JPEG Extended component parameters")
		}
		if count == 1 && (h != 1 || v != 1) {
			return fmt.Errorf("unsupported JPEG Extended component parameters")
		}
		for _, previous := range d.components[:i] {
			if previous.id == c[0] {
				return fmt.Errorf("duplicate JPEG component identifier %d", c[0])
			}
		}
		d.components[i] = extendedComponent{id: c[0], h: h, v: v, quantID: c[2]}
		d.hmax, d.vmax = max(d.hmax, h), max(d.vmax, v)
	}
	mcusX := (width + 8*d.hmax - 1) / (8 * d.hmax)
	mcusY := (height + 8*d.vmax - 1) / (8 * d.vmax)
	var planeBytes, chromaRowBytes uint64
	for i := range d.components {
		c := &d.components[i]
		c.blocksPerLine, c.blocksPerColumn = mcusX*c.h, mcusY*c.v
		planeBytes += uint64(c.blocksPerLine) * uint64(c.blocksPerColumn) * 128
		if d.hmax == 2*c.h && d.vmax == 2*c.v {
			wordBytes := uint64(32<<(^uint(0)>>63)) / 8
			chromaRowBytes = uint64((width+1)/2) * wordBytes
		}
	}
	packedBytes := uint64(width) * uint64(height) * 2 * uint64(count)
	decodedBytes := packedBytes + planeBytes
	if decodedBytes > extendedProcess4MaxDecodedBytes || decodedBytes > uint64(int(^uint(0)>>1)) {
		return fmt.Errorf("JPEG Extended decoded frame exceeds resource limit")
	}
	// Preserve frame admission for the compatibility path. For a single
	// component, defer raster allocation until the scan parameters are known.
	if count != 1 {
		// Padded component planes coexist with all full-resolution planes.
		// The latter coexist with the packed output or a chroma row buffer.
		if err := d.admitRasterPeak(planeBytes + packedBytes + max(packedBytes, chromaRowBytes)); err != nil {
			return err
		}
		for i := range d.components {
			c := &d.components[i]
			c.samples = make([]uint16, c.blocksPerLine*8*c.blocksPerColumn*8)
		}
	}
	d.precision = precision
	d.height = height
	d.width = width
	d.paddedRasterBytes = planeBytes
	d.seenSOF = true
	return nil
}

func (d *extendedProcess4Decoder) allComponentsScanned() bool {
	for _, component := range d.components {
		if !component.scanned {
			return false
		}
	}
	return len(d.components) != 0
}

func (d *extendedProcess4Decoder) parseDQT(segment []byte) error {
	if d.allComponentsScanned() {
		return fmt.Errorf("JPEG quantization table after final scan")
	}
	for offset := 0; offset < len(segment); {
		info := segment[offset]
		offset++
		precision := info >> 4
		tableID := info & 0x0f
		if precision > 1 || tableID > 3 {
			return fmt.Errorf("invalid JPEG quantization table selector")
		}
		valueBytes := 1
		if precision == 1 {
			valueBytes = 2
		}
		if len(segment)-offset < 64*valueBytes {
			return fmt.Errorf("truncated JPEG quantization table")
		}
		for zig := 0; zig < 64; zig++ {
			value := int(segment[offset])
			if valueBytes == 2 {
				value = int(binary.BigEndian.Uint16(segment[offset:]))
			}
			if value == 0 {
				return fmt.Errorf("zero JPEG quantization value")
			}
			d.quant[tableID][extendedZigZag[zig]] = value
			offset += valueBytes
		}
		d.quantDefined[tableID] = true
	}
	if len(segment) == 0 {
		return fmt.Errorf("empty JPEG quantization segment")
	}
	return nil
}

func (d *extendedProcess4Decoder) parseDHT(segment []byte) error {
	if d.allComponentsScanned() {
		return fmt.Errorf("JPEG Huffman table after final scan")
	}
	for offset := 0; offset < len(segment); {
		if len(segment)-offset < 17 {
			return fmt.Errorf("truncated JPEG Huffman table")
		}
		info := segment[offset]
		offset++
		class := info >> 4
		tableID := info & 0x0f
		if class > 1 || tableID > 3 {
			return fmt.Errorf("invalid JPEG Huffman table selector")
		}
		var table extendedHuffmanTable
		symbolCount := 0
		availableCodes := 1
		for length := 1; length <= 16; length++ {
			count := int(segment[offset])
			offset++
			table.counts[length] = count
			symbolCount += count
			availableCodes = availableCodes*2 - count
			if availableCodes < 0 {
				return fmt.Errorf("oversubscribed JPEG Huffman table")
			}
		}
		// JPEG reserves the all-ones code for entropy padding, so a complete
		// canonical code tree is invalid even when it is not oversubscribed.
		if availableCodes == 0 {
			return fmt.Errorf("complete JPEG Huffman code tree")
		}
		if symbolCount == 0 || symbolCount > 256 || len(segment)-offset < symbolCount {
			return fmt.Errorf("invalid JPEG Huffman symbol count")
		}
		table.symbols = append([]byte(nil), segment[offset:offset+symbolCount]...)
		if class == 0 {
			for _, symbol := range table.symbols {
				if symbol > 15 {
					return fmt.Errorf("invalid JPEG DC Huffman symbol %d", symbol)
				}
			}
		}
		table.valid = true
		offset += symbolCount
		d.huffman[class][tableID] = table
	}
	if len(segment) == 0 {
		return fmt.Errorf("empty JPEG Huffman segment")
	}
	return nil
}

func (d *extendedProcess4Decoder) decodeSOS(segment []byte) error {
	if !d.seenSOF {
		return fmt.Errorf("duplicate or out-of-order JPEG SOS")
	}
	if len(segment) < 1 {
		return fmt.Errorf("unsupported JPEG Extended sequential scan")
	}
	count := int(segment[0])
	if count < 1 || count > len(d.components) || len(segment) != 1+2*count+3 {
		return fmt.Errorf("unsupported JPEG Extended sequential scan")
	}
	tail := segment[1+2*count:]
	if tail[0] != 0 || tail[1] != 63 || tail[2] != 0 {
		if !d.tolerateScanParameters {
			return fmt.Errorf("unsupported JPEG Extended sequential scan")
		}
		d.scanParametersIgnored = true
	}
	scan := make([]*extendedComponent, count)
	tables := make([][2]extendedHuffmanTable, count)
	blocksPerMCU := 0
	for i := range scan {
		id := segment[1+2*i]
		for j := range d.components {
			if d.components[j].id == id {
				scan[i] = &d.components[j]
			}
		}
		if scan[i] == nil || scan[i].scanned {
			return fmt.Errorf("unsupported JPEG Extended sequential scan")
		}
		for _, previous := range scan[:i] {
			if previous == scan[i] {
				return fmt.Errorf("unsupported JPEG Extended sequential scan")
			}
		}
		dcID, acID := segment[2+2*i]>>4, segment[2+2*i]&0x0f
		if dcID > 3 || acID > 3 || !d.huffman[0][dcID].valid || !d.huffman[1][acID].valid {
			return fmt.Errorf("undefined JPEG Huffman table")
		}
		if !d.quantDefined[scan[i].quantID] {
			return fmt.Errorf("undefined JPEG quantization table")
		}
		tables[i] = [2]extendedHuffmanTable{d.huffman[0][dcID], d.huffman[1][acID]}
		blocksPerMCU += scan[i].h * scan[i].v
	}
	if count > 1 && blocksPerMCU > 10 {
		return fmt.Errorf("JPEG MCU has %d blocks, more than 10", blocksPerMCU)
	}
	entropy, end, err := d.entropySegment()
	if err != nil {
		return err
	}
	if err := d.checkContext(); err != nil {
		return err
	}
	if len(d.components) == 1 {
		packedBytes := uint64(d.width) * uint64(d.height) * 2
		if d.scanParametersIgnored {
			if err := d.admitRasterPeak(d.paddedRasterBytes + packedBytes*2); err != nil {
				return err
			}
			c := scan[0]
			c.samples = make([]uint16, c.blocksPerLine*8*c.blocksPerColumn*8)
		} else {
			if err := d.admitRasterPeak(packedBytes); err != nil {
				return err
			}
			d.packedMono = make([]byte, packedBytes)
		}
	}
	if err := d.decodeScan(entropy, scan, tables); err != nil {
		return err
	}
	for _, component := range scan {
		component.scanned = true
	}
	d.seenSOS = true
	d.seenDRI = false
	d.pos = end
	return nil
}

// decodeScan decodes a sequential scan: one component in its own block
// order, or several interleaved in MCUs (T.81 A.2).
func (d *extendedProcess4Decoder) decodeScan(entropy []byte, scan []*extendedComponent, tables [][2]extendedHuffmanTable) error {
	reader := extendedBitReader{data: entropy}
	var mcusX, mcusY int
	if len(scan) == 1 {
		c := scan[0]
		componentWidth := (d.width*c.h + d.hmax - 1) / d.hmax
		componentHeight := (d.height*c.v + d.vmax - 1) / d.vmax
		mcusX, mcusY = (componentWidth+7)/8, (componentHeight+7)/8
	} else {
		mcusX = (d.width + 8*d.hmax - 1) / (8 * d.hmax)
		mcusY = (d.height + 8*d.vmax - 1) / (8 * d.vmax)
	}
	for _, c := range scan {
		c.dcPredictor = 0
	}
	restartIndex := byte(0)
	for mcu := 0; mcu < mcusX*mcusY; mcu++ {
		if mcu&63 == 0 {
			if err := d.checkContext(); err != nil {
				return err
			}
		}
		if d.restart > 0 && mcu > 0 && mcu%d.restart == 0 {
			if err := reader.consumeRestart(0xd0 + restartIndex); err != nil {
				return err
			}
			restartIndex = (restartIndex + 1) & 7
			for _, c := range scan {
				c.dcPredictor = 0
			}
		}
		mx, my := mcu%mcusX, mcu/mcusX
		for i, c := range scan {
			if len(scan) == 1 {
				if err := d.decodeBlock(&reader, c, tables[i], mx, my, mcu); err != nil {
					return err
				}
				continue
			}
			for v := 0; v < c.v; v++ {
				for h := 0; h < c.h; h++ {
					if err := d.decodeBlock(&reader, c, tables[i], mx*c.h+h, my*c.v+v, mcu); err != nil {
						return err
					}
				}
			}
		}
	}
	return reader.finish()
}

func (d *extendedProcess4Decoder) decodeBlock(reader *extendedBitReader, c *extendedComponent, tables [2]extendedHuffmanTable, blockX, blockY, mcu int) error {
	var coefficients [64]float64
	quant := &d.quant[c.quantID]
	category, err := reader.decodeHuffman(tables[0])
	if err != nil {
		return fmt.Errorf("decode JPEG DC block %d: %w", mcu, err)
	}
	if category > 15 {
		return fmt.Errorf("invalid JPEG DC coefficient category %d", category)
	}
	difference, err := reader.receiveExtend(int(category))
	if err != nil {
		return err
	}
	if difference > 0 && c.dcPredictor > math.MaxInt32-difference || difference < 0 && c.dcPredictor < math.MinInt32-difference {
		return fmt.Errorf("JPEG DC predictor overflow")
	}
	c.dcPredictor += difference
	coefficients[0] = float64(c.dcPredictor) * float64(quant[0])

	for zig := 1; zig < 64; {
		symbol, err := reader.decodeHuffman(tables[1])
		if err != nil {
			return fmt.Errorf("decode JPEG AC block %d: %w", mcu, err)
		}
		run := int(symbol >> 4)
		size := int(symbol & 0x0f)
		if size == 0 {
			if run == 0 {
				break
			}
			if run != 15 || zig+16 > 64 {
				return fmt.Errorf("invalid JPEG zero run")
			}
			zig += 16
			continue
		}
		zig += run
		if zig >= 64 {
			return fmt.Errorf("JPEG AC run exceeds block")
		}
		value, err := reader.receiveExtend(size)
		if err != nil {
			return err
		}
		natural := extendedZigZag[zig]
		coefficients[natural] = float64(value) * float64(quant[natural])
		zig++
	}
	if blockX < c.blocksPerLine && blockY < c.blocksPerColumn {
		d.writeIDCTBlock(c, blockX, blockY, coefficients)
	}
	return nil
}

func (d *extendedProcess4Decoder) writeIDCTBlock(c *extendedComponent, blockX, blockY int, coefficients [64]float64) {
	center := 1 << (d.precision - 1)
	maximum := (1 << d.precision) - 1
	x0, y0 := blockX*8, blockY*8
	width, height := 8, 8
	if d.packedMono != nil {
		width, height = min(width, d.width-x0), min(height, d.height-y0)
	}
	var scratch [8]uint16
	if onlyDC := func() bool {
		for i := 1; i < 64; i++ {
			if coefficients[i] != 0 {
				return false
			}
		}
		return true
	}(); onlyDC {
		value := roundExtendedIDCTSample(coefficients[0]/8, center, maximum)
		for y := 0; y < height; y++ {
			row := d.idctRow(c, x0, y0+y, scratch[:width])
			for x := range row {
				row[x] = value
			}
			d.packMonoRow(x0, y0+y, row)
		}
		return
	}

	var intermediate [64]float64
	for v := 0; v < 8; v++ {
		for x := 0; x < width; x++ {
			sum := coefficients[v*8] / math.Sqrt2
			for u := 1; u < 8; u++ {
				sum += coefficients[v*8+u] * extendedCos[u][x]
			}
			intermediate[v*8+x] = sum
		}
	}
	for y := 0; y < height; y++ {
		row := d.idctRow(c, x0, y0+y, scratch[:width])
		for x := range row {
			sum := intermediate[x] / math.Sqrt2
			for v := 1; v < 8; v++ {
				sum += intermediate[v*8+x] * extendedCos[v][y]
			}
			row[x] = roundExtendedIDCTSample(sum/4, center, maximum)
		}
		d.packMonoRow(x0, y0+y, row)
	}
}

func (d *extendedProcess4Decoder) idctRow(c *extendedComponent, x, y int, scratch []uint16) []uint16 {
	if d.packedMono != nil {
		return scratch
	}
	start := y*c.blocksPerLine*8 + x
	return c.samples[start : start+len(scratch)]
}

func (d *extendedProcess4Decoder) packMonoRow(x, y int, samples []uint16) {
	if d.packedMono == nil {
		return
	}
	start := (y*d.width + x) * 2
	dst := d.packedMono[start : start+2*len(samples)]
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(dst[i*2:], sample)
	}
}

// output interleaves the component planes at full resolution as
// little-endian 16-bit samples. Subsampled chroma is interpolated with the
// triangle ("fancy") filter libjpeg-family decoders apply by default for 2:1
// ratios (h2v1, h2v2, h1v2); other ratios are replicated.
func (d *extendedProcess4Decoder) output() []byte {
	if d.packedMono != nil {
		return d.packedMono
	}
	count := len(d.components)
	planes := make([][]uint16, count)
	for i := range d.components {
		planes[i] = d.upsample(&d.components[i])
	}
	out := make([]byte, d.width*d.height*2*count)
	for p := 0; p < d.width*d.height; p++ {
		for i := range planes {
			binary.LittleEndian.PutUint16(out[(p*count+i)*2:], planes[i][p])
		}
	}
	return out
}

// upsample returns a component at full resolution (width x height).
func (d *extendedProcess4Decoder) upsample(c *extendedComponent) []uint16 {
	stride := c.blocksPerLine * 8
	// The component's own sample grid (T.81 A.1.1).
	cw := (d.width*c.h + d.hmax - 1) / d.hmax
	ch := (d.height*c.v + d.vmax - 1) / d.vmax
	at := func(x, y int) int {
		x = min(max(x, 0), cw-1)
		y = min(max(y, 0), ch-1)
		return int(c.samples[y*stride+x])
	}
	out := make([]uint16, d.width*d.height)
	rx, ry := d.hmax/c.h, d.vmax/c.v
	exact := d.hmax%c.h == 0 && d.vmax%c.v == 0
	switch {
	case rx == 1 && ry == 1 && exact:
		for y := 0; y < d.height; y++ {
			copy(out[y*d.width:(y+1)*d.width], c.samples[y*stride:y*stride+d.width])
		}
	case rx == 2 && ry == 1 && exact:
		for y := 0; y < d.height; y++ {
			for x := 0; x < d.width; x++ {
				i := x / 2
				value := 3 * at(i, y)
				if x%2 == 0 {
					if i == 0 {
						value = at(i, y)
					} else {
						value = (value + at(i-1, y) + 1) >> 2
					}
				} else if i == cw-1 {
					value = at(i, y)
				} else {
					value = (value + at(i+1, y) + 2) >> 2
				}
				out[y*d.width+x] = uint16(value)
			}
		}
	case rx == 1 && ry == 2 && exact:
		for y := 0; y < d.height; y++ {
			r, far, bias := y/2, y/2-1, 1
			if y%2 == 1 {
				far, bias = y/2+1, 2
			}
			for x := 0; x < d.width; x++ {
				out[y*d.width+x] = uint16((3*at(x, r) + at(x, far) + bias) >> 2)
			}
		}
	case rx == 2 && ry == 2 && exact:
		colsum := make([]int, cw)
		for y := 0; y < d.height; y++ {
			r, far := y/2, y/2-1
			if y%2 == 1 {
				far = y/2 + 1
			}
			for i := range colsum {
				colsum[i] = 3*at(i, r) + at(i, far)
			}
			for x := 0; x < d.width; x++ {
				i := x / 2
				var value int
				switch {
				case x%2 == 0 && i == 0:
					value = (colsum[0]*4 + 8) >> 4
				case x%2 == 0:
					value = (3*colsum[i] + colsum[i-1] + 8) >> 4
				case i == cw-1:
					value = (colsum[i]*4 + 7) >> 4
				default:
					value = (3*colsum[i] + colsum[i+1] + 7) >> 4
				}
				out[y*d.width+x] = uint16(value)
			}
		}
	default:
		for y := 0; y < d.height; y++ {
			for x := 0; x < d.width; x++ {
				out[y*d.width+x] = c.samples[(y*c.v/d.vmax)*stride+x*c.h/d.hmax]
			}
		}
	}
	return out
}

// codestreamIsYCbCr applies the libjpeg rule to a three-component frame: a
// JFIF marker means YCbCr, then the Adobe transform decides, then identifiers
// 'R', 'G', 'B' mean RGB; anything else is YCbCr.
func (d *extendedProcess4Decoder) codestreamIsYCbCr() bool {
	if len(d.components) != 3 {
		return false
	}
	if d.jfif {
		return true
	}
	if d.adobeTransform >= 0 {
		return d.adobeTransform != 0
	}
	return !(d.components[0].id == 'R' && d.components[1].id == 'G' && d.components[2].id == 'B')
}

func (d *extendedProcess4Decoder) entropySegment() ([]byte, int, error) {
	start := d.pos
	for offset := start; offset+1 < len(d.data); {
		if err := d.checkContext(); err != nil {
			return nil, 0, err
		}
		end := offset + min(4096, len(d.data)-1-offset)
		next := bytes.IndexByte(d.data[offset:end], 0xff)
		if next < 0 {
			offset = end
			continue
		}
		i := offset + next
		j := i + 1
		for j < len(d.data) && d.data[j] == 0xff {
			j++
			if j&4095 == 0 {
				if err := d.checkContext(); err != nil {
					return nil, 0, err
				}
			}
		}
		if j >= len(d.data) {
			return nil, 0, fmt.Errorf("truncated JPEG entropy marker")
		}
		marker := d.data[j]
		if marker == 0 {
			offset = j + 1
			continue
		}
		if marker >= 0xd0 && marker <= 0xd7 {
			offset = j + 1
			continue
		}
		return d.data[start:i], i, nil
	}
	return nil, 0, fmt.Errorf("unterminated JPEG entropy data")
}

// Clamp in floating point before any host-sized conversion. Malformed yet
// bounded coefficients can exceed a 32-bit int after dequantization/IDCT.
func roundExtendedIDCTSample(value float64, center, maximum int) uint16 {
	value = math.Round(value) + float64(center)
	if value <= 0 {
		return 0
	}
	if value >= float64(maximum) {
		return uint16(maximum)
	}
	return uint16(value)
}

type extendedBitReader struct {
	data  []byte
	pos   int
	value byte
	bits  int
}

func (r *extendedBitReader) readBit() (int, error) {
	if r.bits == 0 {
		value, err := r.readEntropyByte()
		if err != nil {
			return 0, err
		}
		r.value = value
		r.bits = 8
	}
	r.bits--
	return int(r.value>>r.bits) & 1, nil
}

func (r *extendedBitReader) readEntropyByte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, fmt.Errorf("unexpected end of JPEG entropy data")
	}
	value := r.data[r.pos]
	r.pos++
	if value != 0xff {
		return value, nil
	}
	if r.pos >= len(r.data) || r.data[r.pos] != 0 {
		return 0, fmt.Errorf("unexpected JPEG marker in entropy data")
	}
	r.pos++
	return 0xff, nil
}

func (r *extendedBitReader) readBits(count int) (int, error) {
	value := 0
	for i := 0; i < count; i++ {
		bit, err := r.readBit()
		if err != nil {
			return 0, err
		}
		value = value<<1 | bit
	}
	return value, nil
}

func (r *extendedBitReader) receiveExtend(count int) (int, error) {
	if count == 0 {
		return 0, nil
	}
	if count < 0 || count > 16 {
		return 0, fmt.Errorf("invalid JPEG coefficient category %d", count)
	}
	value, err := r.readBits(count)
	if err != nil {
		return 0, err
	}
	threshold := 1 << (count - 1)
	if value < threshold {
		value -= (1 << count) - 1
	}
	return value, nil
}

func (r *extendedBitReader) decodeHuffman(table extendedHuffmanTable) (byte, error) {
	code := 0
	firstCode := 0
	symbolOffset := 0
	for length := 1; length <= 16; length++ {
		bit, err := r.readBit()
		if err != nil {
			return 0, err
		}
		code = code<<1 | bit
		count := table.counts[length]
		if code >= firstCode && code-firstCode < count {
			index := symbolOffset + code - firstCode
			if index < 0 || index >= len(table.symbols) {
				return 0, fmt.Errorf("invalid JPEG Huffman symbol index")
			}
			return table.symbols[index], nil
		}
		symbolOffset += count
		firstCode = (firstCode + count) << 1
	}
	return 0, fmt.Errorf("invalid JPEG Huffman code")
}

func (r *extendedBitReader) consumeRestart(want byte) error {
	if err := r.validatePadding(); err != nil {
		return err
	}
	r.bits = 0
	if r.pos >= len(r.data) || r.data[r.pos] != 0xff {
		return fmt.Errorf("missing JPEG restart marker 0xff%02x", want)
	}
	for r.pos < len(r.data) && r.data[r.pos] == 0xff {
		r.pos++
	}
	if r.pos >= len(r.data) || r.data[r.pos] != want {
		return fmt.Errorf("invalid JPEG restart marker, want 0xff%02x", want)
	}
	r.pos++
	return nil
}

func (r *extendedBitReader) validatePadding() error {
	if r.bits == 0 {
		return nil
	}
	mask := byte((1 << r.bits) - 1)
	if r.value&mask != mask {
		return fmt.Errorf("invalid JPEG entropy padding")
	}
	return nil
}

func (r *extendedBitReader) finish() error {
	if err := r.validatePadding(); err != nil {
		return err
	}
	if r.pos != len(r.data) {
		return fmt.Errorf("trailing JPEG entropy data")
	}
	return nil
}
