package brotli

// EncodeStored returns a valid Brotli stream holding data in uncompressed
// meta-blocks of at most 65536 bytes. It performs no compression; it lets
// writers emit Brotli-framed payloads (such as the JPEG XL jbrd box) without
// an entropy coder.
func EncodeStored(data []byte) []byte {
	out := make([]byte, 0, len(data)+len(data)/65536*4+8)
	var acc uint64
	var n uint
	put := func(v uint64, bits uint) {
		acc |= v << n
		n += bits
		for n >= 8 {
			out = append(out, byte(acc))
			acc >>= 8
			n -= 8
		}
	}
	flushByte := func() {
		if n > 0 {
			out = append(out, byte(acc))
			acc, n = 0, 0
		}
	}
	put(0, 1) // WBITS = 16
	for len(data) > 0 {
		chunk := min(len(data), 65536)
		put(0, 1)                // ISLAST = 0
		put(0, 2)                // MNIBBLES = 4
		put(uint64(chunk-1), 16) // MLEN - 1
		put(1, 1)                // ISUNCOMPRESSED
		flushByte()              // zero padding to the byte boundary
		out = append(out, data[:chunk]...)
		data = data[chunk:]
	}
	put(1, 1) // ISLAST
	put(1, 1) // ISLASTEMPTY
	flushByte()
	return out
}
