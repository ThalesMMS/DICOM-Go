package jpeglossless

import (
	"context"
	"fmt"
)

// huffTable is a JPEG Huffman decode table built from the DHT BITS/HUFFVAL
// arrays using the canonical generation in ITU-T T.81 Annex C/F.
type huffTable struct {
	mincode    [17]int
	maxcode    [17]int // -1 when no codes of that length
	valptr     [17]int
	values     []byte
	lookLength [256]uint8
	lookValue  [256]byte
	diffLength [256]uint8
	diffValue  [256]int32
}

func buildHuffTable(counts [17]int, values []byte) *huffTable {
	// HUFFSIZE: code length per value, in order.
	var sizes []int
	for l := 1; l <= 16; l++ {
		for i := 0; i < counts[l]; i++ {
			sizes = append(sizes, l)
		}
	}
	// HUFFCODE: canonical codes.
	codes := make([]int, len(sizes))
	if len(sizes) > 0 {
		code := 0
		si := sizes[0]
		k := 0
		for k < len(sizes) {
			for k < len(sizes) && sizes[k] == si {
				codes[k] = code
				code++
				k++
			}
			code <<= 1
			si++
		}
	}
	t := &huffTable{values: values}
	k := 0
	for l := 1; l <= 16; l++ {
		if counts[l] == 0 {
			t.maxcode[l] = -1
			continue
		}
		t.valptr[l] = k
		t.mincode[l] = codes[k]
		k += counts[l]
		t.maxcode[l] = codes[k-1]
		if l <= 8 {
			for i := t.valptr[l]; i < k; i++ {
				first := codes[i] << uint(8-l)
				for prefix := first; prefix < first+(1<<uint(8-l)); prefix++ {
					t.lookLength[prefix] = uint8(l)
					t.lookValue[prefix] = values[i]
					size := int(values[i])
					if size == 0 || size == 16 {
						t.diffLength[prefix] = uint8(l)
						if size == 16 {
							t.diffValue[prefix] = 32768
						}
					} else if l+size <= 8 {
						t.diffLength[prefix] = uint8(l + size)
						magnitude := (prefix >> uint(8-l-size)) & ((1 << uint(size)) - 1)
						t.diffValue[prefix] = extendDifference(int32(magnitude), size)
					}
				}
			}
		}
	}
	return t
}

// decodeSymbol reads bits until a code matches (T.81 Annex F.2.2.3 DECODE).
func (t *huffTable) decodeSymbol(br *bitReader) (byte, error) {
	if br.nbits < 8 {
		br.refill()
	}
	if br.nbits >= 8 {
		prefix := byte(br.bits >> uint(br.nbits-8))
		if length := t.lookLength[prefix]; length != 0 {
			br.nbits -= int(length)
			return t.lookValue[prefix], nil
		}
	}
	code := 0
	for l := 1; l <= 16; l++ {
		b, err := br.readBit()
		if err != nil {
			return 0, err
		}
		code = (code << 1) | b
		if t.maxcode[l] >= 0 && code <= t.maxcode[l] {
			idx := t.valptr[l] + (code - t.mincode[l])
			if idx < 0 || idx >= len(t.values) {
				return 0, fmt.Errorf("%w: Huffman value index out of range", ErrInvalidStream)
			}
			return t.values[idx], nil
		}
	}
	return 0, fmt.Errorf("%w: undecodable Huffman code", ErrInvalidStream)
}

// decodeDifference consumes a short Huffman code and its magnitude together.
// Long codes and a short entropy tail retain the canonical decoder, including
// its marker and truncation checks. Category 16 has no magnitude bits in T.81.
func (t *huffTable) decodeDifference(br *bitReader) (int32, error) {
	if br.nbits < 24 {
		br.refill()
	}
	if br.nbits >= 8 {
		prefix := byte(br.bits >> uint(br.nbits-8))
		if length := t.diffLength[prefix]; length != 0 {
			br.nbits -= int(length)
			return int32(t.diffValue[prefix]), nil
		}
		length := int(t.lookLength[prefix])
		if length != 0 {
			size := int(t.lookValue[prefix])
			if size == 0 || size == 16 {
				br.nbits -= length
				if size == 16 {
					return 32768, nil
				}
				return 0, nil
			}
			if size < 16 && br.nbits >= length+size {
				br.nbits -= length + size
				value := int32(br.bits >> uint(br.nbits) & ((uint64(1) << uint(size)) - 1))
				return extendDifference(value, size), nil
			}
		}
	}
	symbol, err := t.decodeSymbol(br)
	if err != nil {
		return 0, err
	}
	return receiveExtend(br, int(symbol))
}

// bitReader reads MSB-first bits from a JPEG entropy segment, undoing 0xFF byte
// stuffing. It stops at a marker (returns an error if more bits are requested).
type bitReader struct {
	data  []byte
	pos   int
	bits  uint64
	nbits int
	// lenientFill accepts fill bits that are not all ones and records it in
	// fillQuirk.
	lenientFill bool
	fillQuirk   bool
}

func (br *bitReader) readBit() (int, error) {
	if br.nbits == 0 {
		br.refill()
		if br.nbits == 0 {
			return 0, br.boundaryError()
		}
	}
	br.nbits--
	return int((br.bits >> uint(br.nbits)) & 1), nil
}

func (br *bitReader) readBits(n int) (int, error) {
	if br.nbits < n {
		br.refill()
	}
	if br.nbits < n {
		return 0, br.boundaryError()
	}
	br.nbits -= n
	return int(br.bits >> uint(br.nbits) & ((uint64(1) << uint(n)) - 1)), nil
}

// Prefetch complete entropy bytes into the low nbits bits. Markers are never
// consumed: a short final Huffman symbol may still be valid with fewer than
// eight bits before a restart, next scan or EOI.
func (br *bitReader) refill() {
	for br.nbits <= 56 && br.pos < len(br.data) {
		b := br.data[br.pos]
		if b == 0xff {
			if br.pos+1 >= len(br.data) || br.data[br.pos+1] != 0 {
				return
			}
			br.pos++
		}
		br.pos++
		br.bits = br.bits<<8 | uint64(b)
		br.nbits += 8
	}
}

func (br *bitReader) boundaryError() error {
	if br.pos >= len(br.data) {
		return fmt.Errorf("%w: unexpected end of entropy data", ErrInvalidStream)
	}
	if br.pos+1 >= len(br.data) {
		return fmt.Errorf("%w: dangling 0xFF in entropy data", ErrInvalidStream)
	}
	return fmt.Errorf("%w: marker 0xFF%02X inside entropy data", ErrInvalidStream, br.data[br.pos+1])
}

// alignToRestart validates entropy fill bits and consumes the exact expected
// RST marker in the RST0..RST7 sequence.
func (br *bitReader) alignToRestart(ctx context.Context, want byte) error {
	if err := br.validateFillBits(); err != nil {
		return err
	}
	br.nbits = 0
	if br.pos >= len(br.data) || br.data[br.pos] != 0xff {
		return fmt.Errorf("%w: expected restart marker 0xFF%02X", ErrInvalidStream, want)
	}
	marker, err := skipMarkerFill(ctx, br.data, br.pos)
	if err != nil {
		return err
	}
	if marker >= len(br.data) || br.data[marker] != want {
		return fmt.Errorf("%w: invalid restart marker, want 0xFF%02X", ErrInvalidStream, want)
	}
	br.pos = marker + 1
	return nil
}

func skipMarkerFill(ctx context.Context, data []byte, pos int) (int, error) {
	for pos < len(data) && data[pos] == 0xff {
		pos++
		if pos&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return pos, err
			}
		}
	}
	return pos, nil
}

func (br *bitReader) finishEntropy() (int, error) {
	if err := br.validateFillBits(); err != nil {
		return 0, err
	}
	br.nbits = 0
	if br.pos >= len(br.data) || br.data[br.pos] != 0xff {
		return 0, fmt.Errorf("%w: trailing entropy data or missing marker", ErrInvalidStream)
	}
	return br.pos, nil
}

func (br *bitReader) validateFillBits() error {
	if br.nbits == 0 {
		return nil
	}
	if br.nbits >= 8 {
		return fmt.Errorf("%w: trailing entropy data before marker", ErrInvalidStream)
	}
	mask := uint64((1 << br.nbits) - 1)
	if br.bits&mask != mask {
		if br.lenientFill {
			br.fillQuirk = true
			return nil
		}
		return fmt.Errorf("%w: invalid entropy fill bits", ErrInvalidStream)
	}
	return nil
}
