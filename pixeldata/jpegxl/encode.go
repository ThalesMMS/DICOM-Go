package jpegxl

import (
	"encoding/binary"
	"fmt"
)

// Encoder-side headers, frames and the file container.

func writeSizeHeader(w *bitWriter, width, height int) {
	w.bool(false) // not small
	w.u32(uint32(height), bitsOff(9, 1), bitsOff(13, 1), bitsOff(18, 1), bitsOff(30, 1))
	w.write(3, 0) // explicit width
	w.u32(uint32(width), bitsOff(9, 1), bitsOff(13, 1), bitsOff(18, 1), bitsOff(30, 1))
}

func writeBitDepth(w *bitWriter, bits int) {
	w.bool(false) // integer samples
	w.u32(uint32(bits), val(8), val(10), val(12), bitsOff(6, 1))
}

// writeImageMetadata writes non-XYB metadata for grey or RGB images using
// the sRGB transfer function, D65 and relative intent.
func writeImageMetadata(w *bitWriter, bits int, grey bool) {
	w.bool(false) // not all default
	w.bool(false) // no extra fields
	writeBitDepth(w, bits)
	w.bool(bits <= 12) // modular 16-bit buffers suffice
	w.u32(0, val(0), val(1), bitsOff(4, 2), bitsOff(12, 1))
	w.bool(false) // not XYB
	if grey {
		w.bool(false) // colour encoding not default
		w.bool(false) // no ICC
		w.enum(colourSpaceGrey)
		w.enum(1) // D65
		w.bool(false)
		w.enum(13) // sRGB transfer function
		w.enum(1)  // relative rendering intent
	} else {
		w.bool(true) // sRGB
	}
	w.u64(0)     // extensions
	w.bool(true) // default transform data
}

type frameSpec struct {
	modular        bool
	doYCbCr        bool
	jpegUpsampling [3]uint32
	groupSizeShift uint32
	flags          uint64
}

func writeFrameHeader(w *bitWriter, fs frameSpec) {
	w.bool(false) // not all default
	w.write(2, 0) // regular frame
	if fs.modular {
		w.write(1, 1)
	} else {
		w.write(1, 0)
	}
	w.u64(fs.flags)
	w.bool(fs.doYCbCr)
	if fs.doYCbCr {
		for _, m := range fs.jpegUpsampling {
			w.write(2, uint64(m))
		}
	}
	w.u32(1, val(1), val(2), val(4), val(8)) // upsampling
	if fs.modular {
		w.write(2, uint64(fs.groupSizeShift))
	}
	w.u32(1, val(1), val(2), val(3), bitsOff(3, 4)) // one pass
	w.bool(false)                                   // no crop
	w.u32(0, val(0), val(1), val(2), bitsOff(2, 3)) // replace blending
	w.bool(true)                                    // is last
	w.u32(0, val(0), bitsN(4), bitsOff(5, 16), bitsOff(10, 48))
	// Restoration filter: no Gabor smoothing, no edge-preserving filter.
	w.bool(false)
	w.bool(false)
	w.write(2, 0)
	w.u64(0)
	w.u64(0) // frame extensions
}

// writeTOC writes the table of contents and returns the frame bytes.
func writeFrame(w *bitWriter, sections [][]byte) {
	w.bool(false) // not permuted
	w.zeroPad()
	for _, s := range sections {
		w.u32(uint32(len(s)), bitsN(10), bitsOff(14, 1024), bitsOff(22, 17408), bitsOff(30, 4211712))
	}
	w.zeroPad()
	for _, s := range sections {
		w.buf = append(w.buf, s...)
	}
}

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

// wrapContainer places a codestream (and optional jbrd payload) in the
// ISO/IEC 18181-2 container.
func wrapContainer(codestream, jbrd []byte) []byte {
	out := append([]byte{}, containerSignature...)
	out = append(out, box("ftyp", []byte{'j', 'x', 'l', ' ', 0, 0, 0, 0, 'j', 'x', 'l', ' '})...)
	if jbrd != nil {
		out = append(out, box("jbrd", jbrd)...)
	}
	return append(out, box("jxlc", codestream)...)
}

// LosslessOptions configures EncodeLossless.
type LosslessOptions struct {
	// BitsPerSample is the number of significant bits, 1-16.
	BitsPerSample int
}

// EncodeLossless encodes an unsigned integer grey or RGB image as a bare
// JPEG XL codestream with a single modular frame that reproduces every sample
// exactly. Samples must lie in [0, 2^BitsPerSample).
func EncodeLossless(img *Image, opts LosslessOptions) ([]byte, error) {
	if img == nil {
		return nil, fmt.Errorf("%w: nil image", ErrUnsupported)
	}
	bits := opts.BitsPerSample
	if bits == 0 {
		bits = img.BitsPerSample
	}
	if bits < 1 || bits > 16 {
		return nil, fmt.Errorf("%w: %d bits per sample", ErrUnsupported, bits)
	}
	if img.Channels != 1 && img.Channels != 3 {
		return nil, fmt.Errorf("%w: %d channels", ErrUnsupported, img.Channels)
	}
	if img.Width <= 0 || img.Height <= 0 || uint64(img.Width)*uint64(img.Height) > maxImagePixels {
		return nil, ErrLimit
	}
	n := img.Width * img.Height
	if len(img.Samples) != n*img.Channels {
		return nil, fmt.Errorf("%w: sample count", ErrInvalidCodestream)
	}
	limit := int32(1) << uint(bits)
	full := &modularImage{bitDepth: bits}
	for c := 0; c < img.Channels; c++ {
		ch := newChannel(img.Width, img.Height)
		for i := 0; i < n; i++ {
			v := img.Samples[i*img.Channels+c]
			if v < 0 || v >= limit {
				return nil, fmt.Errorf("%w: sample %d outside %d-bit range", ErrUnsupported, v, bits)
			}
			ch.data[i] = v
		}
		full.channels = append(full.channels, ch)
	}
	var transforms []transformInfo
	if img.Channels == 3 {
		forwardYCoCg(full.channels[0].data, full.channels[1].data, full.channels[2].data)
		transforms = []transformInfo{{id: transformRCT, beginC: 0, rctType: 6}}
	}
	fs := frameSpec{modular: true, groupSizeShift: 3}
	f := &frameHeader{width: img.Width, height: img.Height, groupSizeShift: fs.groupSizeShift, numPasses: 1}
	f.computeDims()

	root := weightedTree(bits)
	if img.Channels == 3 {
		root = &tnode{prop: 0, split: 0, left: weightedTree(bits + 1), right: weightedTree(bits)}
	}
	tree, order := flattenTree(root)
	numCtx := tree.numLeaves()

	// Global stream: channels that fit in a group are coded here.
	global, err := encodeModularStream(full, tree, defaultWP, 0, f.groupDim, numCtx)
	if err != nil {
		return nil, err
	}
	global.transforms = transforms
	var groups []*modularStream
	if !global.hasData {
		for g := 0; g < f.numGroups; g++ {
			gx, gy := g%f.xGroups, g/f.xGroups
			x0, y0 := gx*f.groupDim, gy*f.groupDim
			gi := &modularImage{bitDepth: bits}
			for _, fc := range full.channels {
				w := min(f.groupDim, fc.w-x0)
				h := min(f.groupDim, fc.h-y0)
				ch := newChannel(w, h)
				for y := 0; y < h; y++ {
					copy(ch.data[y*w:], fc.data[(y0+y)*fc.w+x0:(y0+y)*fc.w+x0+w])
				}
				gi.channels = append(gi.channels, ch)
			}
			stream := 1 + 3*f.numLFGroups + numQuantTables + g
			ms, err := encodeModularStream(gi, tree, defaultWP, stream, 1<<30, numCtx)
			if err != nil {
				return nil, err
			}
			groups = append(groups, ms)
		}
	}
	all := []*tokenStream{&global.tokens}
	for _, g := range groups {
		all = append(all, &g.tokens)
	}
	enc := buildEntropyEncoder(numCtx, all...)

	// LfGlobal: default LF dequantization, global tree and histograms, then
	// the global modular stream.
	lf := &bitWriter{}
	lf.bool(true)
	lf.bool(true) // has tree
	writeTree(lf, order)
	enc.writeHeader(lf)
	writeGroupHeader(lf, true, transforms)
	enc.writeStream(lf, global.tokens.tokens)

	cs := &bitWriter{}
	cs.write(16, 0x0AFF)
	writeSizeHeader(cs, img.Width, img.Height)
	writeImageMetadata(cs, bits, img.Channels == 1)
	cs.zeroPad()
	writeFrameHeader(cs, fs)
	var sections [][]byte
	if f.numGroups == 1 {
		sections = [][]byte{lf.bytes()}
	} else {
		sections = append(sections, lf.bytes())
		for g := 0; g < f.numLFGroups; g++ {
			sections = append(sections, nil)
		}
		sections = append(sections, nil) // HF global
		for _, g := range groups {
			gw := &bitWriter{}
			writeGroupHeader(gw, true, nil)
			enc.writeStream(gw, g.tokens.tokens)
			sections = append(sections, gw.bytes())
		}
	}
	writeFrame(cs, sections)
	return cs.bytes(), nil
}
