package pixeldata_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// Exhaust the encoded words, including every combination of unused bits.
// The arithmetic oracle extracts the field by division and remainder, then
// interprets the upper half of its code space as negative.
func TestStoredSignedWidthsAgainstIntegerOracle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		container int
		order     binary.ByteOrder
	}{
		{"byte", 8, binary.LittleEndian},
		{"little-endian-word", 16, binary.LittleEndian},
		{"big-endian-word", 16, binary.BigEndian},
	} {
		for bits := 1; bits <= tc.container; bits++ {
			t.Run(fmt.Sprintf("%s/%d-stored", tc.name, bits), func(t *testing.T) {
				count := 1 << tc.container
				src := make([]byte, count*tc.container/8)
				for word := 0; word < count; word++ {
					if tc.container == 8 {
						src[word] = byte(word)
					} else {
						tc.order.PutUint16(src[2*word:], uint16(word))
					}
				}
				dst := make([]int16, count+1)
				const sentinel = 0x5555
				dst[count] = sentinel
				rangeSize := 1 << bits
				for _, lowBit := range []int{0, tc.container - bits} {
					format := pixeldata.SampleFormat{
						BitsAllocated: tc.container,
						BitsStored:    bits,
						HighBit:       lowBit + bits - 1,
						Signed:        true,
						ByteOrder:     tc.order,
					}
					n, err := pixeldata.ReadStoredSamples(ctx, dst, src, format)
					if err != nil || n != count || dst[count] != sentinel {
						t.Fatalf("low bit %d: count = %d, tail = %#x, error = %v", lowBit, n, dst[count], err)
					}
					for word, got := range dst[:count] {
						want := word / (1 << lowBit) % rangeSize
						if want >= rangeSize/2 {
							want -= rangeSize
						}
						if int(got) != want {
							t.Fatalf("low bit %d, word %#x: sample = %d, want %d", lowBit, word, got, want)
						}
					}
					if bits == tc.container {
						break // Full-width fields have only one alignment.
					}
				}
			})
		}
	}
}
