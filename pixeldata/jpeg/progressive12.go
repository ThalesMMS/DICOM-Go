package jpeg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// ErrProgressiveResourceLimit identifies admission or work limits of the
// qualified legacy 12-bit progressive decoder.
var ErrProgressiveResourceLimit = errors.New("dicom: JPEG progressive resource limit")

const (
	progressive12MaxBytes       = uint64(512 << 20)
	progressive12MaxScans       = 256
	progressive12MaxBlockVisits = uint64(8 << 20)
)

// This decoder is derived from T.81 B, C, E and G, using the in-tree
// sequential decoder's marker/table/entropy and IDCT primitives. Progressive
// coefficients stay quantized until all qualified scans have completed.
type progressive12Decoder struct {
	extendedProcess4Decoder
	ctx          context.Context
	spectralOnly bool
	coefficients [][][64]int32
	approx       [3][64]int8 // -1: unseen; otherwise the last scan's Al.
	quantUsed    [4]bool
	scans        int
	work         *progressive12WorkBudget
}

func progressive12RequestBytes(m pixeldata.Metadata, input, joined uint64) (uint64, error) {
	if m.NumberOfFrames <= 0 {
		return 0, fmt.Errorf("%w: invalid frame count", ErrInvalidFragment)
	}
	pixels := uint64(m.Rows) * uint64(m.Columns)
	count := uint64(m.SamplesPerPixel)
	frameBytes := pixels * count * 2
	frames := uint64(m.NumberOfFrames)
	if frameBytes == 0 || frames > progressive12MaxBytes/frameBytes {
		return 0, ErrProgressiveResourceLimit
	}
	// Retained output, source, joined input, frame/plan headers, and bounded
	// marker/table/component scratch. Arithmetic precedes every allocation.
	word := uint64(32<<(^uint(0)>>63)) / 8
	base := frameBytes * frames
	for _, addition := range []uint64{input, joined, frames * 6 * word, 16384} {
		if addition > progressive12MaxBytes || base > progressive12MaxBytes-addition {
			return 0, ErrProgressiveResourceLimit
		}
		base += addition
	}
	blocks := ((uint64(m.Columns) + 7) / 8) * ((uint64(m.Rows) + 7) / 8) * count
	// Each retained block needs 64 int32 coefficients and 64 uint16 samples.
	scratch := blocks * 384
	if scratch > progressive12MaxBytes || base > progressive12MaxBytes-scratch || base+scratch > uint64(int(^uint(0)>>1)) {
		return 0, ErrProgressiveResourceLimit
	}
	return base + scratch, nil
}

type progressive12WorkBudget struct{ remaining uint64 }

func decodeProgressive12(ctx context.Context, stream []byte, m pixeldata.Metadata, spectralOnly bool) ([]byte, error) {
	return decodeProgressive12WithBudget(ctx, stream, m, spectralOnly, &progressive12WorkBudget{remaining: progressive12MaxBlockVisits})
}

func decodeProgressive12WithBudget(ctx context.Context, stream []byte, m pixeldata.Metadata, spectralOnly bool, work *progressive12WorkBudget) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d := progressive12Decoder{extendedProcess4Decoder: extendedProcess4Decoder{data: stream, pos: 2, adobeTransform: -1}, ctx: ctx, spectralOnly: spectralOnly, work: work}
	for i := range d.approx {
		for k := range d.approx[i] {
			d.approx[i][k] = -1
		}
	}
	if _, err := progressive12RequestBytes(m, uint64(len(stream)), 0); err != nil {
		return nil, err
	}
	if len(stream) < 4 || stream[0] != 0xff || stream[1] != 0xd8 {
		return nil, fmt.Errorf("missing JPEG SOI")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		marker, err := d.nextProgressiveMarker()
		if err != nil {
			return nil, err
		}
		if marker == 0xd9 {
			return d.finishProgression()
		}
		if marker != 0xc2 && marker != 0xc4 && marker != 0xdb && marker != 0xdd && marker != 0xda && marker != 0xfe && (marker < 0xe0 || marker > 0xef) {
			return nil, fmt.Errorf("unsupported progressive JPEG marker 0xff%02x", marker)
		}
		segment, err := d.readSegment()
		if err != nil {
			return nil, err
		}
		switch marker {
		case 0xc2:
			err = d.parseSOF2(segment, m)
		case 0xc4:
			err = d.parseDHT(segment)
		case 0xdb:
			previous := d.quant
			err = d.parseDQT(segment)
			for i, used := range d.quantUsed {
				if used && previous[i] != d.quant[i] {
					err = fmt.Errorf("quantization table changed during progression")
				}
			}
		case 0xdd:
			if len(segment) != 2 {
				err = fmt.Errorf("invalid JPEG DRI length")
			} else {
				d.restart = int(binary.BigEndian.Uint16(segment))
			}
		case 0xda:
			err = d.decodeProgressiveScan(segment)
		case 0xe0:
			if d.seenSOS {
				return nil, fmt.Errorf("color marker after initial scan")
			}
			if len(segment) >= 5 && string(segment[:5]) == "JFIF\x00" {
				d.jfif = true
			}
		case 0xee:
			if d.seenSOS {
				return nil, fmt.Errorf("color marker after initial scan")
			}
			if len(segment) >= 12 && string(segment[:5]) == "Adobe" {
				if d.adobeTransform >= 0 && d.adobeTransform != int(segment[11]) {
					return nil, fmt.Errorf("conflicting Adobe color markers")
				}
				d.adobeTransform = int(segment[11])
				if d.adobeTransform > 1 {
					err = fmt.Errorf("unsupported Adobe color transform")
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
}

func (d *progressive12Decoder) nextProgressiveMarker() (byte, error) {
	if d.pos >= len(d.data) || d.data[d.pos] != 0xff {
		return 0, fmt.Errorf("expected JPEG marker at byte %d", d.pos)
	}
	for d.pos < len(d.data) && d.data[d.pos] == 0xff {
		if d.pos&1023 == 0 {
			if err := d.ctx.Err(); err != nil {
				return 0, err
			}
		}
		d.pos++
	}
	if d.pos >= len(d.data) || d.data[d.pos] == 0 {
		return 0, fmt.Errorf("truncated or stuffed JPEG marker")
	}
	marker := d.data[d.pos]
	d.pos++
	return marker, nil
}

func (d *progressive12Decoder) consumeProgressiveRestart(r *extendedBitReader, want byte) error {
	if err := r.validatePadding(); err != nil {
		return err
	}
	r.bits = 0
	if r.pos >= len(r.data) || r.data[r.pos] != 0xff {
		return fmt.Errorf("missing JPEG restart marker")
	}
	for r.pos < len(r.data) && r.data[r.pos] == 0xff {
		if r.pos&1023 == 0 {
			if err := d.ctx.Err(); err != nil {
				return err
			}
		}
		r.pos++
	}
	if r.pos >= len(r.data) || r.data[r.pos] != want {
		return fmt.Errorf("incorrect JPEG restart marker, want %02x", want)
	}
	r.pos++
	return nil
}

func (d *progressive12Decoder) parseSOF2(segment []byte, m pixeldata.Metadata) error {
	if d.seenSOF || d.seenSOS || len(segment) < 6 {
		return fmt.Errorf("duplicate, out-of-order or truncated SOF2")
	}
	count := int(segment[5])
	if (count != 1 && count != 3) || count != int(m.SamplesPerPixel) || len(segment) != 6+3*count {
		return fmt.Errorf("invalid progressive component count")
	}
	if segment[0] != 12 {
		return fmt.Errorf("%w: SOF2 precision=%d, want 12", ErrUnsupportedBitsStored, segment[0])
	}
	width, height := int(binary.BigEndian.Uint16(segment[3:5])), int(binary.BigEndian.Uint16(segment[1:3]))
	if width == 0 || height == 0 || width != int(m.Columns) || height != int(m.Rows) {
		return ErrImageSizeMismatch
	}
	d.components = make([]extendedComponent, count)
	for i := range d.components {
		p := segment[6+3*i:]
		if p[1] != 0x11 || p[2] > 3 {
			return fmt.Errorf("unqualified progressive sampling or quantization selector")
		}
		for _, c := range d.components[:i] {
			if c.id == p[0] {
				return fmt.Errorf("duplicate component ID")
			}
		}
		d.components[i] = extendedComponent{id: p[0], h: 1, v: 1, quantID: p[2], blocksPerLine: (width + 7) / 8, blocksPerColumn: (height + 7) / 8}
	}
	d.coefficients = make([][][64]int32, count)
	for i := range d.components {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		c := &d.components[i]
		blocks := c.blocksPerLine * c.blocksPerColumn
		d.coefficients[i] = make([][64]int32, blocks)
		if err := d.ctx.Err(); err != nil {
			return err
		}
		c.samples = make([]uint16, blocks*64)
	}
	d.width, d.height, d.precision, d.hmax, d.vmax, d.seenSOF = width, height, 12, 1, 1, true
	return nil
}

func (d *progressive12Decoder) entropy() ([]byte, int, error) {
	start := d.pos
	nextCheck := start
	for i := start; i+1 < len(d.data); {
		if i >= nextCheck {
			nextCheck = i + 4096
			if err := d.ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		if d.data[i] != 0xff {
			i++
			continue
		}
		j := i + 1
		for j < len(d.data) && d.data[j] == 0xff {
			if j&4095 == 0 {
				if err := d.ctx.Err(); err != nil {
					return nil, 0, err
				}
			}
			j++
		}
		if j == len(d.data) {
			return nil, 0, fmt.Errorf("truncated entropy marker")
		}
		if d.data[j] == 0 || (d.data[j] >= 0xd0 && d.data[j] <= 0xd7) {
			i = j + 1
			continue
		}
		return d.data[start:i], i, nil
	}
	return nil, 0, fmt.Errorf("unterminated entropy data")
}

func (d *progressive12Decoder) decodeProgressiveScan(segment []byte) error {
	if !d.seenSOF || len(segment) < 6 {
		return fmt.Errorf("out-of-order or truncated progressive SOS")
	}
	if len(d.components) == 3 && d.codestreamIsYCbCr() {
		return fmt.Errorf("%w: only raw RGB progressive12 is qualified", pixeldata.ErrUnsupportedPhotometricInterpretation)
	}
	count := int(segment[0])
	if count < 1 || count > len(d.components) || len(segment) != 4+2*count {
		return fmt.Errorf("invalid progressive scan components")
	}
	tail := segment[1+2*count:]
	ss, se, ah, al := int(tail[0]), int(tail[1]), int(tail[2]>>4), int(tail[2]&15)
	if ss > se || se > 63 || ah > 13 || al > 13 || (ss == 0 && se != 0) || (ss != 0 && count != 1) || (ah != 0 && ah != al+1) {
		return fmt.Errorf("invalid progressive spectral/approximation parameters")
	}
	if d.spectralOnly && (ah != 0 || al != 0) {
		return fmt.Errorf("successive approximation under Spectral Selection syntax")
	}
	var indices [3]int
	var tables [3]extendedHuffmanTable
	lastIndex := -1
	for i := 0; i < count; i++ {
		index := -1
		for j, c := range d.components {
			if c.id == segment[1+2*i] {
				index = j
			}
		}
		if index < 0 || index <= lastIndex {
			return fmt.Errorf("unknown, duplicate or unordered scan component")
		}
		lastIndex, indices[i] = index, index
		c := &d.components[index]
		dc, ac := int(segment[2+2*i]>>4), int(segment[2+2*i]&15)
		if dc > 3 || ac > 3 || !d.quantDefined[c.quantID] || (ss == 0 && ac != 0) || (ss != 0 && dc != 0) {
			return fmt.Errorf("invalid or undefined progressive table selector")
		}
		if ss != 0 && d.approx[index][0] < 0 {
			return fmt.Errorf("AC scan precedes first DC scan")
		}
		for k := ss; k <= se; k++ {
			previous := d.approx[index][k]
			if (ah == 0 && previous != -1) || (ah != 0 && previous != int8(ah)) {
				return fmt.Errorf("duplicate initial band or incompatible refinement")
			}
		}
		if ss == 0 {
			tables[i] = d.huffman[0][dc]
		} else {
			tables[i] = d.huffman[1][ac]
		}
		if (ss != 0 || ah == 0) && !tables[i].valid {
			return fmt.Errorf("undefined progressive Huffman table")
		}
	}
	blocks := len(d.coefficients[indices[0]])
	visits := uint64(blocks) * uint64(count)
	if d.work == nil {
		d.work = &progressive12WorkBudget{remaining: progressive12MaxBlockVisits}
	}
	if d.scans >= progressive12MaxScans || visits > d.work.remaining {
		return ErrProgressiveResourceLimit
	}
	d.scans++
	d.work.remaining -= visits
	entropy, end, err := d.entropy()
	if err != nil {
		return err
	}
	r := extendedBitReader{data: entropy}
	var predictors [3]int64
	eobRun := 0
	restartIndex := byte(0)
	for block := 0; block < blocks; block++ {
		if block&255 == 0 {
			if err := d.ctx.Err(); err != nil {
				return err
			}
		}
		if d.restart > 0 && block > 0 && block%d.restart == 0 {
			if eobRun != 0 {
				return fmt.Errorf("EOB run crosses restart boundary")
			}
			if err := d.consumeProgressiveRestart(&r, 0xd0+restartIndex); err != nil {
				return err
			}
			restartIndex = (restartIndex + 1) & 7
			predictors = [3]int64{}
		}
		for i := 0; i < count; i++ {
			coeff := &d.coefficients[indices[i]][block]
			if ss == 0 {
				if ah == 0 {
					category, err := r.decodeHuffman(tables[i])
					if err != nil || category > 15 {
						return fmt.Errorf("invalid progressive DC category: %v", err)
					}
					difference, err := r.receiveExtend(int(category))
					if err != nil {
						return err
					}
					predictors[i] += int64(difference)
					value := predictors[i] << al
					if value < math.MinInt32 || value > math.MaxInt32 {
						return fmt.Errorf("progressive DC coefficient overflow")
					}
					coeff[0] = int32(value)
				} else {
					bit, err := r.readBit()
					if err != nil {
						return err
					}
					// DC point transform uses arithmetic shift: append the bit
					// to its signed representation, rather than its magnitude.
					coeff[0] |= int32(bit) << al
				}
			} else if ah == 0 {
				if err := progressiveACInitial(&r, tables[i], coeff, ss, se, al, &eobRun); err != nil {
					return err
				}
			} else if err := progressiveACRefine(&r, tables[i], coeff, ss, se, al, &eobRun); err != nil {
				return err
			}
		}
	}
	if eobRun != 0 {
		return fmt.Errorf("EOB run exceeds scan")
	}
	if err := r.finish(); err != nil {
		return err
	}
	for i := 0; i < count; i++ {
		index := indices[i]
		for k := ss; k <= se; k++ {
			d.approx[index][k] = int8(al)
		}
		d.quantUsed[d.components[index].quantID] = true
	}
	d.seenSOS, d.pos = true, end
	return nil
}

func progressiveACInitial(r *extendedBitReader, table extendedHuffmanTable, coeff *[64]int32, ss, se, al int, eob *int) error {
	if *eob > 0 {
		*eob--
		return nil
	}
	for k := ss; k <= se; {
		symbol, err := r.decodeHuffman(table)
		if err != nil {
			return err
		}
		run, size := int(symbol>>4), int(symbol&15)
		if size == 0 {
			if run == 15 {
				if k+16 > se+1 {
					return fmt.Errorf("progressive zero run exceeds band")
				}
				k += 16
				continue
			}
			extra, err := r.readBits(run)
			if err != nil {
				return err
			}
			*eob = (1 << run) + extra - 1
			return nil
		}
		if size > 14 || k+run > se {
			return fmt.Errorf("invalid progressive AC category or run")
		}
		value, err := r.receiveExtend(size)
		if err != nil {
			return err
		}
		k += run
		coeff[extendedZigZag[k]] = int32(value) << al
		k++
	}
	return nil
}

func progressiveACRefine(r *extendedBitReader, table extendedHuffmanTable, coeff *[64]int32, ss, se, al int, eob *int) error {
	step := int32(1) << al
	correct := func(k int) error {
		value := &coeff[extendedZigZag[k]]
		bit, err := r.readBit()
		if err != nil {
			return err
		}
		if bit != 0 {
			if *value > 0 {
				*value += step
			} else {
				*value -= step
			}
		}
		return nil
	}
	k := ss
	if *eob == 0 {
		for k <= se {
			symbol, err := r.decodeHuffman(table)
			if err != nil {
				return err
			}
			run, size := int(symbol>>4), int(symbol&15)
			var newValue int32
			if size == 0 && run != 15 {
				extra, err := r.readBits(run)
				if err != nil {
					return err
				}
				*eob = (1 << run) + extra
				break
			}
			if size != 0 {
				if size != 1 {
					return fmt.Errorf("invalid refinement category")
				}
				bit, err := r.readBit()
				if err != nil {
					return err
				}
				newValue = step
				if bit == 0 {
					newValue = -step
				}
			} else {
				run = 16 // ZRL counts only coefficients with zero history.
			}
			placed := false
			for k <= se {
				if coeff[extendedZigZag[k]] != 0 {
					if err := correct(k); err != nil {
						return err
					}
				} else if run != 0 {
					run--
				} else {
					coeff[extendedZigZag[k]] = newValue
					placed = true
					k++
					break
				}
				k++
				if newValue == 0 && run == 0 {
					placed = true
					break
				}
			}
			if !placed {
				return fmt.Errorf("refinement run exceeds band")
			}
		}
	}
	if *eob > 0 {
		for ; k <= se; k++ {
			if coeff[extendedZigZag[k]] != 0 {
				if err := correct(k); err != nil {
					return err
				}
			}
		}
		*eob--
	}
	return nil
}

func (d *progressive12Decoder) finishProgression() ([]byte, error) {
	if !d.seenSOF || !d.seenSOS || len(d.data)-d.pos > 1 {
		return nil, fmt.Errorf("incomplete progressive frame or trailing data")
	}
	if len(d.components) == 3 && d.codestreamIsYCbCr() {
		return nil, fmt.Errorf("%w: only raw RGB progressive12 is qualified", pixeldata.ErrUnsupportedPhotometricInterpretation)
	}
	for i, c := range d.components {
		for _, al := range d.approx[i] {
			if al != 0 {
				return nil, fmt.Errorf("incomplete progressive coefficient coverage/refinement")
			}
		}
		for b, quantized := range d.coefficients[i] {
			if b&255 == 0 {
				if err := d.ctx.Err(); err != nil {
					return nil, err
				}
			}
			var coefficients [64]float64
			for k, v := range quantized {
				coefficients[k] = float64(v) * float64(d.quant[c.quantID][k])
			}
			d.writeIDCTBlock(&d.components[i], b%c.blocksPerLine, b/c.blocksPerLine, coefficients)
		}
	}
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]byte, d.width*d.height*len(d.components)*2)
	for y := 0; y < d.height; y++ {
		if err := d.ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < d.width; x++ {
			for i, c := range d.components {
				v := c.samples[y*c.blocksPerLine*8+x]
				binary.LittleEndian.PutUint16(out[((y*d.width+x)*len(d.components)+i)*2:], v)
			}
		}
	}
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
