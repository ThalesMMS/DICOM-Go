package pixeldata_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestStoredSampleWriterRoundTripsAndPreservesOtherBits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		format    pixeldata.SampleFormat
		value     float64
		before    []byte
		after     []byte
		wantValue int64
	}{
		{
			name:   "unsigned 8-bit ties away from zero",
			format: pixeldata.SampleFormat{BitsAllocated: 8, BitsStored: 6, HighBit: 6},
			value:  12.5, before: []byte{0xff, 0xa5}, after: []byte{0x9b, 0xa5}, wantValue: 13,
		},
		{
			name:   "signed 8-bit ties away from zero",
			format: pixeldata.SampleFormat{BitsAllocated: 8, BitsStored: 5, HighBit: 6, Signed: true},
			value:  -2.5, before: []byte{0xff}, after: []byte{0xf7}, wantValue: -3,
		},
		{
			name:   "unsigned default little-endian shifted field clamps at zero",
			format: pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 13},
			value:  -42, before: []byte{0xff, 0xff, 0xa5}, after: []byte{0x03, 0xc0, 0xa5},
		},
		{
			name:   "unsigned big-endian shifted field clamps before integer conversion",
			format: pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 13, ByteOrder: binary.BigEndian},
			value:  math.MaxFloat64, before: []byte{0x80, 0x01}, after: []byte{0xbf, 0xfd}, wantValue: 4095,
		},
		{
			name:   "signed little-endian clamps below the minimum",
			format: pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11, Signed: true, ByteOrder: binary.LittleEndian},
			value:  -math.MaxFloat64, before: []byte{0xff, 0x5f}, after: []byte{0x00, 0x58}, wantValue: -2048,
		},
		{
			name:   "signed big-endian full container",
			format: pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, Signed: true, ByteOrder: binary.BigEndian},
			value:  -32767.5, before: []byte{0x00, 0x01}, after: []byte{0x80, 0x00}, wantValue: -32768,
		},
		{
			name:   "unsigned 32-bit includes the full range",
			format: pixeldata.SampleFormat{BitsAllocated: 32, BitsStored: 32, HighBit: 31, ByteOrder: binary.BigEndian},
			value:  1 << 32, before: []byte{0, 0, 0, 0}, after: []byte{0xff, 0xff, 0xff, 0xff}, wantValue: 1<<32 - 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer, err := tc.format.Writer(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := tc.format.Reader()
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Write(tc.before); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(tc.before, tc.after) {
				t.Fatalf("bytes = % x, want % x", tc.before, tc.after)
			}
			if got := reader.Value(tc.before); got != tc.wantValue {
				t.Fatalf("decoded = %d, want %d", got, tc.wantValue)
			}
		})
	}
}

func TestStoredSampleWriterRejectsInvalidInputWithoutWriting(t *testing.T) {
	for _, format := range []pixeldata.SampleFormat{
		{BitsAllocated: 1, BitsStored: 1, HighBit: 0},
		{BitsAllocated: 16, BitsStored: 0, HighBit: 15},
		{BitsAllocated: 16, BitsStored: 12, HighBit: 10},
	} {
		if _, err := format.Writer(1); !errors.Is(err, pixeldata.ErrInvalidSampleFormat) {
			t.Errorf("format %+v: %v, want ErrInvalidSampleFormat", format, err)
		}
	}

	format := pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := format.Writer(value); !errors.Is(err, pixeldata.ErrInvalidStoredSampleValue) {
			t.Errorf("value %v: %v, want ErrInvalidStoredSampleValue", value, err)
		}
	}
	writer, err := format.Writer(1)
	if err != nil {
		t.Fatal(err)
	}
	short := []byte{0xa5}
	if err := writer.Write(short); !errors.Is(err, pixeldata.ErrSampleBufferTooShort) || short[0] != 0xa5 {
		t.Fatalf("short destination = % x, error = %v", short, err)
	}
	var zero pixeldata.StoredSampleWriter
	sample := []byte{0xa5, 0x5a}
	if err := zero.Write(sample); !errors.Is(err, pixeldata.ErrInvalidSampleFormat) || !bytes.Equal(sample, []byte{0xa5, 0x5a}) {
		t.Fatalf("zero writer destination = % x, error = %v", sample, err)
	}
}
