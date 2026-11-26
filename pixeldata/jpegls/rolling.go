package jpegls

import (
	"context"
	"encoding/binary"
	"fmt"
	"image"
)

// A scan needs its current and preceding rows plus the preceding row's
// virtual-left sample (the first sample two rows back). Keep three rows to reuse
// the established T.87 kernels, emitting only the requested rectangle into
// the packed native output. Validate the entropy of every row and its tail
// padding, even when there are no more pixels to emit.
func decodeNativeScan(ctx context.Context, entropy []byte, header frameHeader, preset presetCodingParameters, native []byte, region image.Rectangle, firstComponent, bytesPerSample int) error {
	width, height := header.columns, header.rows
	components := 1
	if header.ilv != 0 {
		components = header.components
	}
	var storage [3][]int
	planes := storage[:components]
	for c := range planes {
		planes[c] = getSampleBuffer(&sampleRows, 3*width)
		defer putSampleBuffer(&sampleRows, planes[c])
	}
	r := &bitReader{buf: entropy}
	st := newLocoStateForPreset(preset)
	var runIndices [3]int
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := minInt(y, 2)
		if header.ilv == 2 {
			if err := decodeSampleLine(ctx, r, st, planes, width, row); err != nil {
				return fmt.Errorf("row %d: %w", y, err)
			}
		} else {
			for c, plane := range planes {
				if header.ilv == 1 {
					st.runIdx = runIndices[c]
				}
				if err := decodeLine(ctx, r, st, plane, width, row); err != nil {
					return fmt.Errorf("row %d component %d: %w", y, firstComponent+c, err)
				}
				runIndices[c] = st.runIdx
			}
		}
		for c, plane := range planes {
			if y >= region.Min.Y && y < region.Max.Y {
				// The kernels clamp reconstructed samples to the coding range,
				// including those kept only as prediction history.
				start := row*width + region.Min.X
				samples := plane[start : start+region.Dx()]
				offset := ((y-region.Min.Y)*region.Dx()*header.components + firstComponent + c) * bytesPerSample
				if err := emitNativeRow(ctx, samples, native[offset:], header.components, bytesPerSample, preset.maxval); err != nil {
					return err
				}
			}
			if row == 2 {
				copy(plane[:2*width], plane[width:3*width])
			}
		}
	}
	return validateEntropyPadding(r)
}

func emitNativeRow(ctx context.Context, samples []int, native []byte, components, bytesPerSample, maxval int) error {
	if bytesPerSample == 1 {
		for x, sample := range samples {
			if x&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if sample < 0 || sample > maxval {
				return fmt.Errorf("%w: reconstructed sample", ErrInvalidCodestream)
			}
			native[x*components] = byte(sample)
		}
		return nil
	}
	for x, sample := range samples {
		if x&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if sample < 0 || sample > maxval {
			return fmt.Errorf("%w: reconstructed sample", ErrInvalidCodestream)
		}
		binary.LittleEndian.PutUint16(native[x*components*2:], uint16(sample))
	}
	return nil
}
