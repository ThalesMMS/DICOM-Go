// Package packedbits holds the bit arithmetic for native Pixel Data with Bits
// Allocated 1. PS3.5 8.1.1 stores such frames back to back in one LSB-first
// bit stream: a frame starts where the previous one ended, so only the total
// bit count of the value is rounded up to a whole byte.
package packedbits

// Bytes returns the number of bytes that hold bits packed bits.
func Bytes(bits int64) int64 {
	size := bits / 8
	if bits%8 != 0 {
		size++
	}
	return size
}

// Copy copies count LSB-first bits from src, starting at bit srcBit, to dst,
// starting at bit dstBit. The buffers must not overlap. Destination bits
// outside the range are preserved.
func Copy(dst []byte, dstBit int64, src []byte, srcBit int64, count int64) {
	if dstBit%8 == 0 && srcBit%8 == 0 && count >= 8 {
		wholeBytes := count / 8
		copy(dst[dstBit/8:dstBit/8+wholeBytes], src[srcBit/8:srcBit/8+wholeBytes])
		copiedBits := wholeBytes * 8
		dstBit += copiedBits
		srcBit += copiedBits
		count -= copiedBits
	}
	for count > 0 {
		dstShift, srcShift := dstBit%8, srcBit%8
		width := min(8-dstShift, count)
		value := src[srcBit/8] >> srcShift
		if srcShift+width > 8 {
			value |= src[srcBit/8+1] << (8 - srcShift)
		}
		mask := byte((1<<width)-1) << dstShift
		dst[dstBit/8] = dst[dstBit/8]&^mask | (value<<dstShift)&mask
		dstBit += width
		srcBit += width
		count -= width
	}
}
