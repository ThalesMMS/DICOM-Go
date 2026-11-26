package jpegxl

import "context"

// Image is a decoded integer JPEG XL image with interleaved samples.
type Image struct {
	Width, Height int
	// Channels is 1 (grey) or 3 (RGB); extra channels are not returned.
	Channels int
	// BitsPerSample is the codestream bit depth.
	BitsPerSample int
	// Samples holds Width*Height*Channels values in row-major,
	// channel-interleaved order.
	Samples []int32
}

// Decode decodes a JPEG XL file or bare codestream holding a single frame:
// a modular (lossless-capable) frame with integer samples, or a
// JPEG-compatible VarDCT frame (DCT8 blocks, RAW JPEG quantization table, no
// XYB) as made by JPEG recompression and EncodeLossy, rendered like a JPEG
// decoder would, or an 8-bit sRGB XYB DCT8 frame within the bounded profile.
// Animation and other features outside this profile return ErrUnsupported. Use
// ReconstructJPEG to restore the original file of a JPEG-recompressed image.
func Decode(data []byte) (*Image, error) {
	return decode(context.Background(), data, true)
}

func decode(ctx context.Context, data []byte, allowXYB bool) (_ *Image, err error) {
	defer recoverCorrupt(&err)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := parseContainer(data)
	if err != nil {
		return nil, err
	}
	br := newBitReader(c.codestream)
	hdr, err := readCodestreamHeader(br)
	if err != nil {
		return nil, err
	}
	if hdr.meta.xybEncoded && !allowXYB {
		return nil, unsupported("lossy XYB requires the JPEG XL Lossy transfer syntax")
	}
	if hdr.meta.xybEncoded && (hdr.meta.width > maxXYBDimension || hdr.meta.height > maxXYBDimension || len(c.codestream) > maxXYBCodestream) {
		return nil, ErrLimit
	}
	f, offsets, sizes, err := readFirstFrame(br, hdr)
	if err != nil {
		return nil, err
	}
	if !f.modular {
		if hdr.meta.xybEncoded {
			return decodeXYB(ctx, hdr, br, c.codestream, f, offsets, sizes)
		}
		return decodeJPEGCompatible(c.codestream)
	}
	if f.numPasses != 1 {
		return nil, unsupported("progressive modular passes")
	}
	readers, err := sectionReaders(br, c.codestream, offsets, sizes)
	if err != nil {
		return nil, err
	}
	img, err := decodeModularFrame(hdr, f, readers)
	if err != nil {
		return nil, err
	}
	nc := 3
	if hdr.meta.colour.colourSpace == colourSpaceGrey {
		nc = 1
	}
	out := &Image{Width: f.xsize, Height: f.ysize, Channels: nc, BitsPerSample: int(hdr.meta.depth.bitsPerSample)}
	n := f.xsize * f.ysize
	out.Samples = make([]int32, n*nc)
	for c := 0; c < nc; c++ {
		ch := img.channels[c]
		if ch.w != f.xsize || ch.h != f.ysize {
			return nil, corrupt("channel size")
		}
		for i, v := range ch.data {
			out.Samples[i*nc+c] = v
		}
	}
	return out, nil
}
