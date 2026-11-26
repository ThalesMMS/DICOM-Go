package display

import (
	"encoding/binary"
	"errors"
	"fmt"
	mathbits "math/bits"
)

var (
	// ErrInvalidLUTDescriptor reports a LUT Descriptor (0028,3002) that does
	// not hold exactly three values, declares bits per entry outside 8–16, or
	// declares a negative entry count.
	ErrInvalidLUTDescriptor = errors.New("dicom/display: invalid LUT descriptor")
	// ErrInvalidLUTData reports LUT Data (0028,3006) that does not match its
	// descriptor: a length that fits no encoding of the declared entries, or
	// entries above the declared bits per entry.
	ErrInvalidLUTData = errors.New("dicom/display: LUT data does not match descriptor")
	// ErrLegacyLUTEncoding accompanies ErrInvalidLUTDescriptor or
	// ErrInvalidLUTData when the LUT is in a legacy encoding that
	// LUTOptions.Legacy reads.
	ErrLegacyLUTEncoding = errors.New("dicom/display: legacy LUT encoding (read with LUTOptions.Legacy)")
)

// LUTOptions controls how LUTs are read from a data set. The zero value reads
// the encodings of PS3.3 C.11.1.1.1, C.11.2.1.1, C.11.6.1.1 and C.7.6.3.1.5:
// 8-bit entries packed two per word (the first in the low byte), 9–16-bit
// entries one per word, and 8-bit entries one per word with a zero high byte,
// which the standard notes some implementations write and which is detected
// from the data length (LUTAdjustmentOneEntryPerWord).
type LUTOptions struct {
	// Legacy also reads the legacy encodings DCMTK reads (dcm2pnm with
	// +Mc, --check-lut-depth), each listed in LUT.Adjustments: LUT Data
	// longer or shorter than the entry count, 8-bit entries packed under
	// another bits per entry, bits per entry outside 8–16 (inferred from the
	// largest entry), a 16-bit descriptor whose entries are 8-bit values
	// (high byte zero or a copy of the low byte; read as 8 bits), an 8-bit
	// descriptor whose entries are 16-bit values (read as 16 bits), and
	// entries above the bits per entry (masked). Without it these are
	// refused with ErrLegacyLUTEncoding.
	Legacy bool
}

func lutOptions(opts []LUTOptions) LUTOptions {
	if len(opts) > 0 {
		return opts[0]
	}
	return LUTOptions{}
}

// LUTAdjustment names a non-conformant LUT encoding that was read, and how.
type LUTAdjustment string

const (
	// LUTAdjustmentOneEntryPerWord: 8-bit entries stored one per 16-bit
	// word instead of packed.
	LUTAdjustmentOneEntryPerWord LUTAdjustment = "8-bit entries stored one per 16-bit word"
	// LUTAdjustmentDataLength: the data holds more or fewer entries than the
	// descriptor declares; the entries present were used.
	LUTAdjustmentDataLength LUTAdjustment = "LUT Data length does not match the number of entries; the entries present were used"
	// LUTAdjustmentPacked: 8-bit entries packed two per word under a
	// descriptor declaring more bits; read as 8 bits.
	LUTAdjustmentPacked LUTAdjustment = "8-bit entries packed two per word under another bits per entry; read as 8 bits"
	// LUTAdjustmentBitsInferred: bits per entry outside 8–16; the depth of
	// the largest entry (at least 8) was used.
	LUTAdjustmentBitsInferred LUTAdjustment = "bits per entry outside 8-16; inferred from the largest entry"
	// LUTAdjustmentEightBitValues: a 16-bit descriptor whose entries all
	// have a zero high byte or one equal to the low byte; read as 8 bits.
	LUTAdjustmentEightBitValues LUTAdjustment = "16 bits per entry declared for 8-bit values; read as 8 bits"
	// LUTAdjustmentSixteenBitValues: an 8-bit descriptor whose entries use
	// the high byte; read as 16 bits.
	LUTAdjustmentSixteenBitValues LUTAdjustment = "8 bits per entry declared for 16-bit values; read as 16 bits"
	// LUTAdjustmentMasked: entries above the bits per entry were masked to
	// its low bits.
	LUTAdjustmentMasked LUTAdjustment = "entries above the bits per entry were masked to their low bits"
)

// LUT models a DICOM lookup table built from a LUT Descriptor (0028,3002) and
// LUT Data (0028,3006). It backs the Modality LUT (PS3.3 C.11.1), the VOI LUT
// (PS3.3 C.11.2), the Presentation LUT (PS3.3 C.11.6) and the palette color
// LUTs (PS3.3 C.7.6.3.1.5).
//
// A LUT Descriptor holds three values: the number of entries, the first stored
// pixel value mapped, and the number of bits per entry. Per the standard a
// number-of-entries value of 0 means 2^16 (65536) entries.
type LUT struct {
	// FirstMapped is the input value mapped by entry 0 (LUT Descriptor value 2).
	// Inputs at or below FirstMapped map to the first entry; inputs at or above
	// FirstMapped+len(Entries)-1 map to the last entry.
	FirstMapped int
	// BitsPerEntry is LUT Descriptor value 3, 8 to 16 (8 or 16 in image
	// IODs); the output range is 0 to 2^BitsPerEntry-1. For a LUT read in a
	// legacy encoding it is the depth the entries were read with.
	BitsPerEntry int
	// Entries holds the output values, one per declared entry.
	Entries []uint16
	// Adjustments lists the non-conformant encodings accepted while reading
	// the LUT, in order; empty for a conformant LUT.
	Adjustments []LUTAdjustment
}

// NewLUT validates descriptor and entries and returns a LUT. descriptor must
// hold exactly [numberOfEntries, firstMapped, bitsPerEntry]; firstMapped is
// taken verbatim (callers sign-interpret it before calling, per the relevant
// Pixel Representation). A numberOfEntries of 0 is treated as 65536, and every
// entry must fit in bitsPerEntry (8 to 16).
func NewLUT(descriptor []int, entries []uint16) (*LUT, error) {
	if len(descriptor) != 3 {
		return nil, fmt.Errorf("%w: got %d descriptor values, want 3", ErrInvalidLUTDescriptor, len(descriptor))
	}
	numberOfEntries := descriptor[0]
	if numberOfEntries == 0 {
		numberOfEntries = 1 << 16
	}
	if numberOfEntries < 0 {
		return nil, fmt.Errorf("%w: number of entries %d is negative", ErrInvalidLUTDescriptor, numberOfEntries)
	}
	bitsPerEntry := descriptor[2]
	if bitsPerEntry < 8 || bitsPerEntry > 16 {
		return nil, fmt.Errorf("%w: bits per entry %d, want 8 to 16", ErrInvalidLUTDescriptor, bitsPerEntry)
	}
	if len(entries) != numberOfEntries {
		return nil, fmt.Errorf("%w: got %d entries, want %d", ErrInvalidLUTData, len(entries), numberOfEntries)
	}
	if largest := maxEntry(entries); int(largest) > 1<<bitsPerEntry-1 {
		return nil, fmt.Errorf("%w: entry %d exceeds %d bits per entry", ErrInvalidLUTData, largest, bitsPerEntry)
	}
	return &LUT{
		FirstMapped:  descriptor[1],
		BitsPerEntry: bitsPerEntry,
		Entries:      entries,
	}, nil
}

// lutFromData decodes LUT Data for a descriptor and builds the LUT with the
// adjustments decodeLUTData made.
func lutFromData(raw []byte, order binary.ByteOrder, numberOfEntries, firstMapped, bitsPerEntry int, opts LUTOptions) (*LUT, error) {
	count := numberOfEntries
	if count == 0 {
		count = 1 << 16
	}
	entries, bitsPerEntry, adjustments, err := decodeLUTData(raw, order, count, bitsPerEntry, opts)
	if err != nil {
		return nil, err
	}
	return &LUT{FirstMapped: firstMapped, BitsPerEntry: bitsPerEntry, Entries: entries, Adjustments: adjustments}, nil
}

// decodeLUTData reads count entries of bitsPerEntry bits from LUT Data (or
// palette color LUT data) and returns them with the depth they were read
// with. See LUTOptions for the encodings read.
func decodeLUTData(raw []byte, order binary.ByteOrder, count, bitsPerEntry int, opts LUTOptions) ([]uint16, int, []LUTAdjustment, error) {
	if order == nil {
		order = binary.LittleEndian
	}
	if len(raw) == 0 {
		return nil, 0, nil, fmt.Errorf("%w: no LUT data", ErrInvalidLUTData)
	}
	legacy := func(base error, format string, args ...any) error {
		return fmt.Errorf("%w: %w: %s", base, ErrLegacyLUTEncoding, fmt.Sprintf(format, args...))
	}
	var adjustments []LUTAdjustment
	if (bitsPerEntry < 8 || bitsPerEntry > 16) && !opts.Legacy {
		return nil, 0, nil, legacy(ErrInvalidLUTDescriptor, "bits per entry %d, want 8 to 16", bitsPerEntry)
	}

	if len(raw)%2 != 0 {
		raw = append(raw[:len(raw):len(raw)], 0)
	}
	words := make([]uint16, len(raw)/2)
	for i := range words {
		words[i] = order.Uint16(raw[2*i:])
	}

	switch {
	case len(words) == (count+1)/2 && (count > 1 || bitsPerEntry == 8):
		// 8 bits allocated: two entries per word, the first in the low byte.
		if bitsPerEntry != 8 {
			if !opts.Legacy {
				return nil, 0, nil, legacy(ErrInvalidLUTData, "%d words hold %d packed 8-bit entries, but bits per entry is %d", len(words), count, bitsPerEntry)
			}
			adjustments = append(adjustments, LUTAdjustmentPacked)
		}
		entries := make([]uint16, count)
		for i := range entries {
			entries[i] = words[i/2] >> (8 * (i % 2)) & 0xFF
		}
		return entries, 8, adjustments, nil
	case len(words) == count:
	default:
		if !opts.Legacy {
			return nil, 0, nil, legacy(ErrInvalidLUTData, "%d data words for %d entries of %d bits", len(words), count, bitsPerEntry)
		}
		adjustments = append(adjustments, LUTAdjustmentDataLength)
	}

	return wordLUTDepth(words, bitsPerEntry, adjustments, opts, true)
}

// wordLUTDepth settles the depth of entries stored one per word: the
// descriptor's bits per entry when the entries fit it, otherwise (with
// opts.Legacy) the depth LUTOptions describes. oneEntryPerWord reports 8-bit
// entries as LUTAdjustmentOneEntryPerWord, since word storage is not their
// standard encoding in LUT Data (it is in segmented palettes).
func wordLUTDepth(entries []uint16, bitsPerEntry int, adjustments []LUTAdjustment, opts LUTOptions, oneEntryPerWord bool) ([]uint16, int, []LUTAdjustment, error) {
	largest, eightBit := maxEntry(entries), eightBitValues(entries)
	switch {
	case bitsPerEntry < 8 || bitsPerEntry > 16:
		if !opts.Legacy {
			return nil, 0, nil, fmt.Errorf("%w: %w: bits per entry %d, want 8 to 16", ErrInvalidLUTDescriptor, ErrLegacyLUTEncoding, bitsPerEntry)
		}
		bitsPerEntry = min(max(mathbits.Len16(largest), 8), 16)
		adjustments = append(adjustments, LUTAdjustmentBitsInferred)
	case opts.Legacy && bitsPerEntry == 16 && eightBit:
		adjustments = append(adjustments, LUTAdjustmentEightBitValues)
		return maskEntries(entries, 8), 8, adjustments, nil
	case opts.Legacy && bitsPerEntry == 8 && !eightBit:
		bitsPerEntry = 16
		adjustments = append(adjustments, LUTAdjustmentSixteenBitValues)
	case bitsPerEntry == 8 && oneEntryPerWord:
		adjustments = append(adjustments, LUTAdjustmentOneEntryPerWord)
	}
	if limit := uint16(1<<bitsPerEntry - 1); largest > limit {
		if !opts.Legacy {
			return nil, 0, nil, fmt.Errorf("%w: %w: entry %d exceeds %d bits per entry", ErrInvalidLUTData, ErrLegacyLUTEncoding, largest, bitsPerEntry)
		}
		adjustments = append(adjustments, LUTAdjustmentMasked)
		entries = maskEntries(entries, bitsPerEntry)
	}
	return entries, bitsPerEntry, adjustments, nil
}

func maxEntry(entries []uint16) uint16 {
	var largest uint16
	for _, entry := range entries {
		largest = max(largest, entry)
	}
	return largest
}

// eightBitValues reports whether every entry's high byte is zero or a copy of
// its low byte, the pattern of 8-bit values in 16-bit words.
func eightBitValues(entries []uint16) bool {
	for _, entry := range entries {
		if high := entry >> 8; high != 0 && high != entry&0xFF {
			return false
		}
	}
	return true
}

func maskEntries(entries []uint16, bitsPerEntry int) []uint16 {
	mask := uint16(1<<bitsPerEntry - 1)
	out := make([]uint16, len(entries))
	for i, entry := range entries {
		out[i] = entry & mask
	}
	return out
}

// EncodeLUTData encodes entries as little-endian LUT Data (0028,3006) for
// bitsPerEntry: 8-bit entries packed two per word with the first in the low
// byte (padded with a zero byte when the count is odd), 9–16-bit entries one
// per word.
func EncodeLUTData(entries []uint16, bitsPerEntry int) []byte {
	if bitsPerEntry == 8 {
		out := make([]byte, len(entries)+len(entries)%2)
		for i, entry := range entries {
			out[i] = uint8(entry)
		}
		return out
	}
	out := make([]byte, 2*len(entries))
	for i, entry := range entries {
		binary.LittleEndian.PutUint16(out[2*i:], entry)
	}
	return out
}

// Lookup maps value through the LUT, clamping to the first/last entry for
// inputs outside the mapped range.
func (l *LUT) Lookup(value int) uint16 {
	return l.lookupInt64(int64(value))
}

func (l *LUT) lookupInt64(value int64) uint16 {
	if len(l.Entries) == 0 {
		return 0
	}
	idx := value - int64(l.FirstMapped)
	if idx < 0 {
		idx = 0
	}
	if idx >= int64(len(l.Entries)) {
		idx = int64(len(l.Entries) - 1)
	}
	return l.Entries[idx]
}

// bitsOrDefault is BitsPerEntry, or 16 for a LUT built without a valid one.
func (l *LUT) bitsOrDefault() int {
	if l.BitsPerEntry < 1 || l.BitsPerEntry > 16 {
		return 16
	}
	return l.BitsPerEntry
}

// outputMax is the top of the LUT's output range, 2^BitsPerEntry-1 (PS3.3
// C.11.2.1.1, C.11.6.1.1).
func (l *LUT) outputMax() uint16 {
	return uint16(1<<l.bitsOrDefault() - 1)
}

// entryByte scales an entry to 8 bits by dropping the bits below the top
// eight (entry >> (BitsPerEntry-8)), as palette and presentation outputs are
// reduced to 8 bits.
func (l *LUT) entryByte(entry uint16) uint8 {
	entry = min(entry, l.outputMax())
	if shift := l.bitsOrDefault() - 8; shift > 0 {
		entry >>= shift
	}
	return uint8(entry)
}

// entry16 scales an entry to the full 16-bit range (an 8-bit value v becomes
// v*257, a 16-bit value is kept).
func (l *LUT) entry16(entry uint16) uint16 {
	top := uint32(l.outputMax())
	value := min(uint32(entry), top)
	return uint16((value*65535 + top/2) / top)
}
