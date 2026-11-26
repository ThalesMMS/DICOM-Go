package display

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestDecodeStoredIntoMasksSignExtendsAndChecksRange(t *testing.T) {
	pixels := make([]byte, 6)
	// 12 stored bits, garbage in the high nibble: 0xF800 is -2048 after
	// masking and sign extension; 0x07FF is 2047.
	binary.LittleEndian.PutUint16(pixels[0:], 0xF800)
	binary.LittleEndian.PutUint16(pixels[2:], 0x07FF)
	binary.LittleEndian.PutUint16(pixels[4:], 0x3001)
	frame := Frame{Rows: 1, Columns: 3, Pixels: pixels, Format: PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11, Signed: true}}
	signed := make([]int16, 3)
	if err := DecodeStoredInto(signed, frame); err != nil || signed[0] != -2048 || signed[1] != 2047 || signed[2] != 1 {
		t.Fatalf("signed = %v, %v", signed, err)
	}
	unsignedFrame := frame
	unsignedFrame.Format = PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15}
	unsigned := make([]uint16, 3)
	if err := DecodeStoredInto(unsigned, unsignedFrame); err != nil || unsigned[0] != 0xF800 {
		t.Fatalf("unsigned = %v, %v", unsigned, err)
	}
	// Unsigned 16-bit samples above 32767 do not fit int16.
	if err := DecodeStoredInto(signed, unsignedFrame); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("unsigned into int16 error = %v", err)
	}
	eightBit := frame
	eightBit.Format.BitsAllocated = 8
	if err := DecodeStoredInto(signed, eightBit); !errors.Is(err, ErrUnsupportedBitsAllocated) {
		t.Fatalf("8-bit error = %v", err)
	}
}

// DecodeModalityLevels must reproduce DecodeModalityFloat32 bit for bit
// (#1612): levels[index[i]] is the i-th modality value for every format it
// accepts, and it declines ranges a uint16 index or the pixel count cannot
// carry.
func TestDecodeModalityLevelsMatchesFloat32Decode(t *testing.T) {
	type tc struct {
		name     string
		format   PixelFormat
		modality ModalityLUT
		value    func(i int) uint64
		wantOK   bool
	}
	lut := &LUT{FirstMapped: -20, BitsPerEntry: 16, Entries: []uint16{5, 900, 40, 65535, 7}}
	cases := []tc{
		{"unsigned 12-bit fractional rescale", PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11}, ModalityLUT{Slope: 0.37, Intercept: -1024.25}, func(i int) uint64 { return uint64(i*29) % 4096 }, true},
		{"signed 12-bit with garbage high bits", PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11, Signed: true}, ModalityLUT{Slope: 1, Intercept: -1024}, func(i int) uint64 { return 0xF000 | uint64(i*13)%4096 }, true},
		{"signed 16-bit big endian", PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, Signed: true, ByteOrder: binary.BigEndian}, ModalityLUT{Slope: 2, Intercept: 7}, func(i int) uint64 { return uint64(uint16(int16(i*101%3000 - 1500))) }, true},
		{"8-bit zero slope", PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, ModalityLUT{}, func(i int) uint64 { return uint64(i % 256) }, true},
		{"signed 16-bit modality LUT", PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, Signed: true}, ModalityLUT{LUT: lut}, func(i int) uint64 { return uint64(uint16(int16(i%40 - 25))) }, true},
		{"range wider than the frame", PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15}, ModalityLUT{Slope: 1}, func(i int) uint64 { return uint64(i*977) % 65536 }, false},
	}
	const rows, cols = 64, 96
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bytesPerSample := c.format.BitsAllocated / 8
			order := c.format.ByteOrder
			if order == nil {
				order = binary.LittleEndian
			}
			pixels := make([]byte, rows*cols*bytesPerSample)
			for i := 0; i < rows*cols; i++ {
				if bytesPerSample == 1 {
					pixels[i] = byte(c.value(i))
				} else {
					order.PutUint16(pixels[2*i:], uint16(c.value(i)))
				}
			}
			frame := Frame{Rows: rows, Columns: cols, Pixels: pixels, Format: c.format, Modality: c.modality}
			want, err := DecodeModalityFloat32(frame)
			if err != nil {
				t.Fatal(err)
			}
			index, levels, ok, err := DecodeModalityLevels(frame)
			if err != nil || ok != c.wantOK {
				t.Fatalf("ok=%v err=%v, want ok=%v", ok, err, c.wantOK)
			}
			if !ok {
				return
			}
			if len(index) != len(want) {
				t.Fatalf("index has %d values, want %d", len(index), len(want))
			}
			for i, level := range index {
				if math.Float32bits(levels[level]) != math.Float32bits(want[i]) {
					t.Fatalf("pixel %d: level value %v, want %v", i, levels[level], want[i])
				}
			}
		})
	}
	if _, _, ok, err := DecodeModalityLevels(Frame{Rows: 1, Columns: 1, Pixels: make([]byte, 4), Format: PixelFormat{BitsAllocated: 32, BitsStored: 32, HighBit: 31}}); ok || err != nil {
		t.Fatalf("32-bit frame ok=%v err=%v, want declined", ok, err)
	}
}
