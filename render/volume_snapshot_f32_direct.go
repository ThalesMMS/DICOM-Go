package render

import (
	"context"
	"io"
	"math"
	"unsafe"
)

// writeModalityDirect streams a float32 modality payload as the bytes it
// already holds, a whole slice per write when rows are tightly packed and a
// row per write otherwise. The bytes equal those of the per-sample loop in
// WriteModalityF32Context, which converts one sample at a time and bounds a
// volume upload by the host instead of by the transport.
//
// done is false, with nothing written, when the payload is not little-endian
// float32 in memory or does not hold every row the descriptor names; the
// per-sample loop then converts or reports it as before. The lease holds
// immutable samples: dst consumes each slice before returning and must not
// modify it. Cancellation is observed before every write.
func (p *float32VolumePayload) writeModalityDirect(ctx context.Context, dst io.Writer, descriptor VolumeDescriptor) (done bool, err error) {
	if p == nil || descriptor.ScalarFormat != VolumeScalarF32ModalityLE || !hostLittleEndian() {
		return false, nil
	}
	rows, depth := uint64(descriptor.Dimensions[1]), uint64(descriptor.Dimensions[2])
	rowStride, sliceStride := descriptor.RowStrideBytes, descriptor.SliceStrideBytes
	rowBytes, ok := checkedMul64(uint64(descriptor.Dimensions[0])*uint64(descriptor.Components), 4)
	if !ok || rowBytes == 0 || rows == 0 || depth == 0 || rowStride%4 != 0 || sliceStride%4 != 0 {
		return false, nil
	}
	// The last byte read is the end of the last row of the last slice.
	lastSlice, ok := checkedMul64(depth-1, sliceStride)
	if !ok {
		return false, nil
	}
	lastRow, ok := checkedMul64(rows-1, rowStride)
	if !ok {
		return false, nil
	}
	end, ok := checkedAdd64(lastSlice, lastRow)
	if ok {
		end, ok = checkedAdd64(end, rowBytes)
	}
	if !ok || end > p.byteLen() || end > uint64(math.MaxInt) {
		return false, nil
	}
	run, runStride, runs := rowBytes, rowStride, rows
	if rowStride == rowBytes {
		// Tightly packed rows: one slice is one contiguous run.
		run, runStride, runs = rowBytes*rows, sliceStride, 1
	}
	base := unsafe.Pointer(unsafe.SliceData(p.data))
	for z := uint64(0); z < depth; z++ {
		for index := uint64(0); index < runs; index++ {
			if err := ctx.Err(); err != nil {
				return true, err
			}
			offset := z*sliceStride + index*runStride
			if err := writeExact(dst, unsafe.Slice((*byte)(unsafe.Add(base, int(offset))), int(run))); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}
