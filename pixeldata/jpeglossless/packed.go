package jpeglossless

import "encoding/binary"

// decodePackedSV1 predicts from the last sample, or from the preceding
// row's first sample at a row boundary. Nothing else needs to be retained
// in a separate reconstruction plane. The packed output remains private
// until the shared parser validates scan padding, EOI, and trailing bytes.
func (d *losslessDecoder) decodePackedSV1(reader *bitReader, table *huffTable, initial, mask int32, pointTransform uint) error {
	sampleBytes := int(d.metadata.BitsAllocated / 8)
	stride := d.frame.width * sampleBytes
	out := make([]byte, stride*d.frame.height)
	decodeRun := decodePackedRun8
	if sampleBytes == 2 {
		decodeRun = decodePackedRun16
	}
	aboveFirst := initial
	marker := byte(0xd0)
	remaining := d.restartInterval
	for y := 0; y < d.frame.height; y++ {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		row := out[y*stride : (y+1)*stride]
		prediction := aboveFirst
		for x := 0; x < d.frame.width; {
			end := d.frame.width
			if d.restartInterval > 0 {
				if remaining == 0 {
					if err := reader.alignToRestart(d.ctx, marker); err != nil {
						return err
					}
					marker = 0xd0 + ((marker - 0xd0 + 1) & 7)
					remaining = d.restartInterval
					prediction = initial
				}
				end = min(end, x+remaining)
				remaining -= end - x
			}
			var err error
			prediction, err = decodeRun(reader, table, row[x*sampleBytes:end*sampleBytes], prediction, mask, pointTransform)
			if err != nil {
				return err
			}
			x = end
		}
		if sampleBytes == 1 {
			aboveFirst = int32(row[0]) >> pointTransform
		} else {
			aboveFirst = int32(binary.LittleEndian.Uint16(row)) >> pointTransform
		}
	}
	d.image.packedMono = out
	return nil
}

func decodePackedRun8(reader *bitReader, table *huffTable, dst []byte, prediction, mask int32, shift uint) (int32, error) {
	for x := range dst {
		difference, err := table.decodeDifference(reader)
		if err != nil {
			return 0, err
		}
		prediction = (prediction + difference) & mask
		dst[x] = byte(prediction << shift)
	}
	return prediction, nil
}

func decodePackedRun16(reader *bitReader, table *huffTable, dst []byte, prediction, mask int32, shift uint) (int32, error) {
	for offset := 0; offset < len(dst); offset += 2 {
		difference, err := table.decodeDifference(reader)
		if err != nil {
			return 0, err
		}
		prediction = (prediction + difference) & mask
		binary.LittleEndian.PutUint16(dst[offset:], uint16(prediction<<shift))
	}
	return prediction, nil
}
