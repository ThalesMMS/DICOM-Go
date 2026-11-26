package packedbits

import "testing"

func bitAt(b []byte, i int64) byte { return b[i/8] >> (i % 8) & 1 }

func TestBytesRoundsOnlyTheTotal(t *testing.T) {
	for _, tc := range []struct{ bits, want int64 }{{0, 0}, {1, 1}, {8, 1}, {9, 2}, {18, 3}, {75, 10}} {
		if got := Bytes(tc.bits); got != tc.want {
			t.Fatalf("Bytes(%d) = %d, want %d", tc.bits, got, tc.want)
		}
	}
}

func TestCopyMatchesBitByBitCopy(t *testing.T) {
	src := []byte{0b10110100, 0b01101011, 0b11100010, 0b00011101, 0b10101010}
	for srcBit := int64(0); srcBit < 16; srcBit++ {
		for dstBit := int64(0); dstBit < 9; dstBit++ {
			for count := int64(0); srcBit+count <= int64(len(src))*8 && dstBit+count <= 40; count++ {
				dst := []byte{0xff, 0x00, 0xff, 0x00, 0xff}
				want := append([]byte(nil), dst...)
				for i := int64(0); i < count; i++ {
					d := dstBit + i
					want[d/8] = want[d/8]&^(1<<(d%8)) | bitAt(src, srcBit+i)<<(d%8)
				}
				Copy(dst, dstBit, src, srcBit, count)
				for i := range dst {
					if dst[i] != want[i] {
						t.Fatalf("Copy(dstBit=%d, srcBit=%d, count=%d) = %08b, want %08b", dstBit, srcBit, count, dst, want)
					}
				}
			}
		}
	}
}
