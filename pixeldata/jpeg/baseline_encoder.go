package jpeg

import (
	"context"
	"fmt"
	"image/color"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// T.81 Table K.2, natural order. Table K.1 and the entropy primitives are
// shared with the local Extended encoder; no reference encoder is embedded.
var baselineChrominanceQuantization = [64]int{
	17, 18, 24, 47, 99, 99, 99, 99,
	18, 21, 26, 66, 99, 99, 99, 99,
	24, 26, 56, 99, 99, 99, 99, 99,
	47, 66, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
}

func baselineQuantization(base [64]int, quality int) [64]int {
	scale := 200 - 2*quality
	if quality < 50 {
		scale = 5000 / quality
	}
	var table [64]int
	for i, value := range base {
		// Baseline permits only 8-bit quantization entries (T.81 B.2.4.1).
		table[i] = min(max((value*scale+50)/100, 1), 255)
	}
	return table
}

func (e *Encoder) encodeBaseline(ctx context.Context, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	width, height := int(metadata.Columns), int(metadata.Rows)
	h, v, components := 1, 1, int(metadata.SamplesPerPixel)
	if components == 3 {
		switch e.subsampling {
		case Subsampling420:
			h, v = 2, 2
		case Subsampling422:
			h = 2
		}
	}
	mcusX, mcusY := (width+8*h-1)/(8*h), (height+8*v-1)/(8*v)
	blocksPerMCU := h * v
	if components == 3 {
		blocksPerMCU += 2
	}
	quant := [2][64]int{
		baselineQuantization(extendedLuminanceQuantization, e.quality),
		baselineQuantization(baselineChrominanceQuantization, e.quality),
	}
	var inverseQuant [2][64]float64
	for table := range quant {
		for coefficient, value := range quant[table] {
			inverseQuant[table][coefficient] = 1 / float64(value)
		}
	}
	// Coefficients are private, in MCU/component order, and released after
	// this frame. One FDCT pass suffices for both frequency gathering and
	// coding, including independent DC predictors for Cb and Cr.
	coefficientCount := uint64(mcusX) * uint64(mcusY) * uint64(blocksPerMCU) * 64
	if coefficientCount > uint64(int(^uint(0)>>1))/2 {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: JPEG coefficient buffer size", pixeldata.ErrEncoderOutputInvalid)
	}
	blocks := make([]int16, int(coefficientCount))
	var dcFrequency, acFrequency [2][256]int
	var predictor [3]int
	var planes [3][256]float64
	index := 0
	for my := 0; my < mcusY; my++ {
		if err := ctx.Err(); err != nil {
			return pixeldata.EncodedFrame{}, err
		}
		for mx := 0; mx < mcusX; mx++ {
			if mx%128 == 0 {
				if err := ctx.Err(); err != nil {
					return pixeldata.EncodedFrame{}, err
				}
			}
			if components == 3 {
				// Convert source pixels once, then average chroma.
				baselineColorMCU(frame, width, height, mx, my, h, v, &planes)
			}
			for c := 0; c < components; c++ {
				bh, bv, table := h, v, 0
				if c > 0 {
					bh, bv, table = 1, 1, 1
				}
				for by := 0; by < bv; by++ {
					for bx := 0; bx < bh; bx++ {
						var block [64]float64
						if components == 1 {
							block = baselineSampleBlock(frame, width, height, mx, my)
						} else {
							stride := 8 * bh
							for y := 0; y < 8; y++ {
								copy(block[y*8:y*8+8], planes[c][(by*8+y)*stride+bx*8:][:8])
							}
						}
						coefficients := baselineForwardDCT(&block)
						zigzag := blocks[index : index+64]
						index += 64
						for zig, natural := range extendedZigZag {
							zigzag[zig] = int16(math.Round(coefficients[natural] * inverseQuant[table][natural]))
						}
						delta := int(zigzag[0]) - predictor[c]
						predictor[c] = int(zigzag[0])
						dcFrequency[table][extendedMagnitudeCategory(delta)]++
						forEachExtendedACSymbol(zigzag, func(symbol byte, _ int) { acFrequency[table][symbol]++ })
					}
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	tables := 1
	if components == 3 {
		tables = 2
	}
	var dc, ac [2]extendedHuffmanSpec
	var dcCodes, acCodes [2]extendedHuffmanCodes
	for i := 0; i < tables; i++ {
		dc[i], ac[i] = extendedOptimalHuffmanTable(dcFrequency[i]), extendedOptimalHuffmanTable(acFrequency[i])
		dcCodes[i], acCodes[i] = dc[i].codes(), ac[i].codes()
	}
	out := make([]byte, 0, len(frame)/4+512)
	out = append(out, 0xff, 0xd8)
	var dqt, dht []byte
	for i := 0; i < tables; i++ {
		dqt = append(dqt, byte(i))
		for _, natural := range extendedZigZag {
			dqt = append(dqt, byte(quant[i][natural]))
		}
		dht = append(dht, byte(i))
		dht = append(dht, dc[i].counts[1:]...)
		dht = append(dht, dc[i].symbols...)
		dht = append(dht, byte(0x10|i))
		dht = append(dht, ac[i].counts[1:]...)
		dht = append(dht, ac[i].symbols...)
	}
	out = appendExtendedSegment(out, 0xdb, dqt)
	sof := []byte{8, byte(height >> 8), byte(height), byte(width >> 8), byte(width), byte(components), 1, byte(h<<4 | v), 0}
	sos := []byte{byte(components), 1, 0}
	if components == 3 {
		sof = append(sof, 2, 0x11, 1, 3, 0x11, 1)
		sos = append(sos, 2, 0x11, 3, 0x11)
	}
	sos = append(sos, 0, 63, 0)
	out = appendExtendedSegment(out, 0xc0, sof)
	out = appendExtendedSegment(out, 0xc4, dht)
	out = appendExtendedSegment(out, 0xda, sos)
	writer := extendedBitWriter{out: out}
	predictor = [3]int{}
	index = 0
	for mcu := 0; mcu < mcusX*mcusY; mcu++ {
		if mcu%128 == 0 {
			if err := ctx.Err(); err != nil {
				return pixeldata.EncodedFrame{}, err
			}
		}
		for c := 0; c < components; c++ {
			count, table := h*v, 0
			if c > 0 {
				count, table = 1, 1
			}
			for b := 0; b < count; b++ {
				zigzag := blocks[index : index+64]
				index += 64
				delta := int(zigzag[0]) - predictor[c]
				predictor[c] = int(zigzag[0])
				category := extendedMagnitudeCategory(delta)
				writer.write(dcCodes[table].code[category], dcCodes[table].length[category])
				writer.writeAdditionalBits(delta, category)
				forEachExtendedACSymbol(zigzag, func(symbol byte, value int) {
					writer.write(acCodes[table].code[symbol], acCodes[table].length[symbol])
					writer.writeAdditionalBits(value, int(symbol&15))
				})
			}
		}
	}
	out = append(writer.flush(), 0xff, 0xd9)
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	encoded := pixeldata.EncodedFrame{Data: out}
	if components == 3 {
		planar := uint16(0)
		encoded.PhotometricInterpretation, encoded.PlanarConfiguration = "YBR_FULL_422", &planar
	}
	return encoded, nil
}

// Edge extension precedes chroma averaging, including odd dimensions. Chroma
// samples are centered over each 2x1/2x2 group; they never use partial-range YBR.
func baselineSampleBlock(frame []byte, width, height, mx, my int) [64]float64 {
	var block [64]float64
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			offset := min(my*8+y, height-1)*width + min(mx*8+x, width-1)
			block[y*8+x] = float64(int(frame[offset]) - 128)
		}
	}
	return block
}

func baselineColorMCU(frame []byte, width, height, mx, my, h, v int, planes *[3][256]float64) {
	*planes = [3][256]float64{}
	stride := 8 * h
	for y := 0; y < 8*v; y++ {
		row := min(my*8*v+y, height-1) * width
		for x := 0; x < stride; x++ {
			offset := (row + min(mx*stride+x, width-1)) * 3
			yy, cb, cr := color.RGBToYCbCr(frame[offset], frame[offset+1], frame[offset+2])
			planes[0][y*stride+x] = float64(int(yy) - 128)
			chroma := (y/v)*8 + x/h
			planes[1][chroma] += float64(cb)
			planes[2][chroma] += float64(cr)
		}
	}
	for c := 1; c < 3; c++ {
		for i := 0; i < 64; i++ {
			planes[c][i] = float64((int(planes[c][i])+h*v/2)/(h*v) - 128)
		}
	}
}

// Separable T.81 A.3.3 FDCT. Symmetric sample pairs reduce the even
// frequencies to four-point transforms. Odd frequencies use the shared local
// Extended encoder's cosine constants. Scaling is orthonormal.
func baselineForwardDCT(block *[64]float64) [64]float64 {
	var rows, out [64]float64
	for y := 0; y < 8; y++ {
		row := baselineDCT1D((*[8]float64)(block[y*8 : y*8+8]))
		copy(rows[y*8:y*8+8], row[:])
	}
	for u := 0; u < 8; u++ {
		column := [8]float64{rows[u], rows[8+u], rows[16+u], rows[24+u], rows[32+u], rows[40+u], rows[48+u], rows[56+u]}
		transformed := baselineDCT1D(&column)
		for v, value := range transformed {
			out[v*8+u] = 0.25 * value
		}
	}
	return out
}

func baselineDCT1D(s *[8]float64) [8]float64 {
	a, b, c, d := s[0]+s[7], s[1]+s[6], s[2]+s[5], s[3]+s[4]
	e, f, g, h := s[0]-s[7], s[1]-s[6], s[2]-s[5], s[3]-s[4]
	c1, c2, c3, c5, c6, c7 := extendedCos[1][0], extendedCos[2][0], extendedCos[3][0], extendedCos[5][0], extendedCos[6][0], extendedCos[7][0]
	return [8]float64{
		(a + b + c + d) / math.Sqrt2,
		e*c1 + f*c3 + g*c5 + h*c7,
		(a-d)*c2 + (b-c)*c6,
		e*c3 - f*c7 - g*c1 - h*c5,
		(a - b - c + d) / math.Sqrt2,
		e*c5 - f*c1 + g*c7 + h*c3,
		(a-d)*c6 - (b-c)*c2,
		e*c7 - f*c5 + g*c3 - h*c1,
	}
}
