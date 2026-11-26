package pixeldata

import "github.com/ThalesMMS/dicom-go/internal/packedbits"

func packedFrameBits(m Metadata) int64 {
	return int64(m.Rows) * int64(m.Columns) * int64(m.SamplesPerPixel)
}

func packedBytes(bits int64) int64 {
	return packedbits.Bytes(bits)
}

// nativePackedByteCount gives the byte count of concatenated one-bit
// frames, before adding an even-length Value Field pad byte.
func nativePackedByteCount(m Metadata) (int64, bool) {
	bits, ok := checkedDecompressProduct(packedFrameBits(m), int64(m.NumberOfFrames))
	return packedBytes(bits), ok
}

// copyPackedBits copies an LSB-first range between nonoverlapping buffers.
// It preserves destination bits outside the range.
func copyPackedBits(dst []byte, dstBit int64, src []byte, srcBit int64, count int64) {
	packedbits.Copy(dst, dstBit, src, srcBit, count)
}
