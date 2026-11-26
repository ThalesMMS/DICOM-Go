package vendors

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// ELSCINT1 private compressions of (07A1,xx11).
const (
	// ElscintPMSCTRLE1 is the 16-bit monochrome byte-run and delta coding.
	ElscintPMSCTRLE1 = "PMSCT_RLE1"
	// ElscintPMSCTRGB1 is the 8-bit RGB delta and repeat coding.
	ElscintPMSCTRGB1 = "PMSCT_RGB1"
)

// Diagnostics an ElscintPixels result carries.
const (
	// DiagnosticElscintPrivatePixels: the frame was decoded from the
	// ELSCINT1 private compressed pixel data, not from a standard transfer
	// syntax; the object's Pixel Data was not used or changed.
	DiagnosticElscintPrivatePixels = "elscint-private-pixel-data"
	// DiagnosticElscintStored: the private value already had the native
	// frame length and was copied as is.
	DiagnosticElscintStored = "elscint-private-pixel-data-stored"
	// DiagnosticElscintStandardPixelData: the compressed stream was read
	// from (7FE0,0010) because the private element is absent.
	DiagnosticElscintStandardPixelData = "elscint-compressed-standard-pixel-data"
)

// ElscintPixels is one native frame decoded from ELSCINT1 private
// compressed pixel data. It is returned beside the object, which is never
// modified: callers decide whether to display it.
type ElscintPixels struct {
	// Compression is the (07A1,xx11) value, ElscintPMSCTRLE1 or
	// ElscintPMSCTRGB1.
	Compression string
	// Source is the element the compressed stream was read from.
	Source core.Tag
	// Metadata describes Frame: Rows, Columns, Samples per Pixel, Bits
	// Allocated/Stored, High Bit, Pixel Representation, Photometric
	// Interpretation and Planar Configuration of the object.
	Metadata pixeldata.Metadata
	// Frame holds the native little-endian samples (16-bit monochrome for
	// PMSCT_RLE1; 8-bit RGB for PMSCT_RGB1, interleaved or planar as the
	// object's Planar Configuration says).
	Frame []byte
	// Diagnostics lists the Diagnostic* codes that apply.
	Diagnostics []string
}

const (
	elscintGroup        = 0x07A1
	elscintCompression  = 0x11
	elscintPixelElement = 0x0A
)

// ElscintPixelDataOf decodes the ELSCINT1 private compressed pixel data of
// dataset: the compression named by (07A1,xx11) of "ELSCINT1" applied to
// (07A1,xx0A), or to Pixel Data (7FE0,0010) when the private element is
// absent and Pixel Data does not have the native frame length. It returns
// ErrNotFound when the object names no ELSCINT1 compression, ErrInvalid for
// unknown compressions, metadata the compression cannot describe, and
// malformed streams, and ErrLimit beyond limits (MaxBytes bounds the stream,
// MaxDecodedBytes the frame).
//
// The codings are private and undocumented. They were implemented from the
// observed behavior of the GDCM rle2img and pmsct_rgb1 examples (no code
// ported):
//
//   - PMSCT_RLE1, 16-bit monochrome: first, byte 0xA5 followed by a count n
//     and a byte v stands for n+1 copies of v; every other byte stands for
//     itself. Then, over that expanded byte sequence, 0x5A followed by two
//     bytes sets the sample to their little-endian value, and every other
//     byte is a signed 8-bit difference from the previous sample (which
//     starts at 0), modulo 2^16.
//   - PMSCT_RGB1, 8-bit RGB: a state machine starting in gray mode with a
//     zero value. 0x81 toggles between gray and color mode and zeroes the
//     value; 0x83 n repeats the current pixel n times; 0x82 makes the next
//     one (gray) or three (color) bytes a difference even when they equal a
//     code; any other byte is the difference of the gray value, or of red
//     followed by the green and blue differences. Differences add modulo 256
//     and every pixel, gray ones as R=G=B, is written once.
//
// A value whose length equals the native frame is stored uncompressed.
func ElscintPixelDataOf(dataset *object.Object, limits Limits) (*ElscintPixels, error) {
	limits = limits.normalized()
	if dataset == nil {
		return nil, ErrNotFound
	}
	compressionTag, err := dataset.PrivateTag(elscintGroup, "ELSCINT1", elscintCompression)
	if err != nil || !dataset.Has(compressionTag) {
		return nil, ErrNotFound
	}
	compression, err := dataset.LookupString(compressionTag)
	if err != nil {
		return nil, fmt.Errorf("%w: ELSCINT1 compression type: %w", ErrInvalid, err)
	}
	compression = strings.TrimSpace(strings.TrimRight(compression, "\x00 "))
	if compression != ElscintPMSCTRLE1 && compression != ElscintPMSCTRGB1 {
		return nil, fmt.Errorf("%w: ELSCINT1 compression %q", ErrInvalid, compression)
	}
	metadata, err := pixeldata.ExtractMetadata(dataset)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := checkElscintMetadata(compression, metadata); err != nil {
		return nil, err
	}
	rows, columns := int(metadata.Rows), int(metadata.Columns)
	frameBytes := metadata.FrameSize()
	if frameBytes > int64(limits.MaxDecodedBytes) {
		return nil, fmt.Errorf("%w: %d-byte frame", ErrLimit, frameBytes)
	}
	result := &ElscintPixels{Compression: compression, Metadata: metadata, Diagnostics: []string{DiagnosticElscintPrivatePixels}}
	data, tag, err := privateValue(dataset, elscintGroup, []string{"ELSCINT1"}, elscintPixelElement, limits)
	if errors.Is(err, ErrNotFound) {
		raw, ok := dataset.GetRaw(core.TagPixelData)
		if !ok || int64(len(raw)) == frameBytes {
			// A native Pixel Data value is decoded as such, not here.
			return nil, ErrNotFound
		}
		if len(raw) > limits.MaxBytes {
			return nil, fmt.Errorf("%w: %d bytes", ErrLimit, len(raw))
		}
		data, tag, err = raw, core.TagPixelData, nil
		result.Diagnostics = append(result.Diagnostics, DiagnosticElscintStandardPixelData)
	}
	if err != nil {
		return nil, err
	}
	result.Source = tag
	if int64(len(data)) == frameBytes {
		result.Frame = append([]byte(nil), data...)
		result.Diagnostics = append(result.Diagnostics, DiagnosticElscintStored)
		return result, nil
	}
	if compression == ElscintPMSCTRLE1 {
		result.Frame, err = DecodeElscintPMSCTRLE1(data, rows, columns)
	} else {
		result.Frame, err = DecodeElscintPMSCTRGB1(data, rows, columns, metadata.PlanarConfigurationPresent && metadata.PlanarConfiguration == 1)
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

func checkElscintMetadata(compression string, m pixeldata.Metadata) error {
	if m.Rows == 0 || m.Columns == 0 || m.NumberOfFrames > 1 {
		return fmt.Errorf("%w: %s needs one frame of known size", ErrInvalid, compression)
	}
	if compression == ElscintPMSCTRLE1 {
		if m.SamplesPerPixel != 1 || m.BitsAllocated != 16 {
			return fmt.Errorf("%w: %s needs 16-bit monochrome samples, not %d x %d bits", ErrInvalid, compression, m.SamplesPerPixel, m.BitsAllocated)
		}
		return nil
	}
	if m.SamplesPerPixel != 3 || m.BitsAllocated != 8 {
		return fmt.Errorf("%w: %s needs 8-bit RGB samples, not %d x %d bits", ErrInvalid, compression, m.SamplesPerPixel, m.BitsAllocated)
	}
	return nil
}

// DecodeElscintPMSCTRLE1 decodes a PMSCT_RLE1 stream (see
// ElscintPixelDataOf) into rows*columns little-endian 16-bit samples. The
// stream must produce every sample; it may produce one more, a pad sample
// some streams end with, which is dropped. Anything else is ErrInvalid.
func DecodeElscintPMSCTRLE1(data []byte, rows, columns int) ([]byte, error) {
	if rows <= 0 || columns <= 0 {
		return nil, fmt.Errorf("%w: PMSCT_RLE1 frame %dx%d", ErrInvalid, columns, rows)
	}
	want := rows * columns
	out := make([]byte, 2*want)
	expanded := elscintRunReader{data: data}
	var sample uint16
	count := 0
	emit := func() error {
		if count == want+1 {
			return fmt.Errorf("%w: PMSCT_RLE1 stream holds more than %d samples", ErrInvalid, want)
		}
		if count < want {
			out[2*count], out[2*count+1] = byte(sample), byte(sample>>8)
		}
		count++
		return nil
	}
	for {
		b, ok, err := expanded.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if b == 0x5A {
			lo, ok1, err1 := expanded.next()
			hi, ok2, err2 := expanded.next()
			if err1 != nil || err2 != nil || !ok1 || !ok2 {
				return nil, fmt.Errorf("%w: PMSCT_RLE1 absolute sample truncated", ErrInvalid)
			}
			sample = uint16(lo) | uint16(hi)<<8
		} else {
			sample += uint16(int16(int8(b)))
		}
		if err := emit(); err != nil {
			return nil, err
		}
	}
	if count < want {
		return nil, fmt.Errorf("%w: PMSCT_RLE1 stream holds %d of %d samples", ErrInvalid, count, want)
	}
	return out, nil
}

// elscintRunReader yields the bytes of a PMSCT_RLE1 stream with its 0xA5
// runs expanded.
type elscintRunReader struct {
	data   []byte
	pos    int
	repeat int
	value  byte
}

func (r *elscintRunReader) next() (byte, bool, error) {
	if r.repeat > 0 {
		r.repeat--
		return r.value, true, nil
	}
	if r.pos >= len(r.data) {
		return 0, false, nil
	}
	b := r.data[r.pos]
	r.pos++
	if b != 0xA5 {
		return b, true, nil
	}
	if r.pos+2 > len(r.data) {
		return 0, false, fmt.Errorf("%w: PMSCT_RLE1 run truncated at byte %d", ErrInvalid, r.pos-1)
	}
	r.repeat, r.value = int(r.data[r.pos]), r.data[r.pos+1]
	r.pos += 2
	return r.value, true, nil
}

// DecodeElscintPMSCTRGB1 decodes a PMSCT_RGB1 stream (see
// ElscintPixelDataOf) into rows*columns 8-bit RGB pixels, interleaved or,
// when planar is set, as three planes. The stream must describe exactly
// that many pixels; bytes after the last pixel (value padding) are ignored.
func DecodeElscintPMSCTRGB1(data []byte, rows, columns int, planar bool) ([]byte, error) {
	if rows <= 0 || columns <= 0 {
		return nil, fmt.Errorf("%w: PMSCT_RGB1 frame %dx%d", ErrInvalid, columns, rows)
	}
	const (
		toggle = 0x81
		escape = 0x82
		repeat = 0x83
	)
	pixels := rows * columns
	out := make([]byte, 3*pixels)
	put := func(p int, rgb [3]byte) {
		if planar {
			out[p], out[pixels+p], out[2*pixels+p] = rgb[0], rgb[1], rgb[2]
		} else {
			out[3*p], out[3*p+1], out[3*p+2] = rgb[0], rgb[1], rgb[2]
		}
	}
	pos := 0
	take := func(n int) ([]byte, error) {
		if pos+n > len(data) {
			return nil, fmt.Errorf("%w: PMSCT_RGB1 stream truncated at byte %d", ErrInvalid, pos)
		}
		b := data[pos : pos+n]
		pos += n
		return b, nil
	}
	gray := true
	var value [3]byte
	add := func(differences []byte) {
		if gray {
			value[0] += differences[0]
			value[1], value[2] = value[0], value[0]
			return
		}
		value[0] += differences[0]
		value[1] += differences[1]
		value[2] += differences[2]
	}
	width := func() int {
		if gray {
			return 1
		}
		return 3
	}
	for p := 0; p < pixels; {
		code, err := take(1)
		if err != nil {
			return nil, err
		}
		switch code[0] {
		case toggle:
			gray = !gray
			value = [3]byte{}
		case repeat:
			n, err := take(1)
			if err != nil {
				return nil, err
			}
			if int(n[0]) > pixels-p {
				return nil, fmt.Errorf("%w: PMSCT_RGB1 repeat of %d exceeds the %d remaining pixels", ErrInvalid, n[0], pixels-p)
			}
			for i := 0; i < int(n[0]); i++ {
				put(p, value)
				p++
			}
		case escape:
			differences, err := take(width())
			if err != nil {
				return nil, err
			}
			add(differences)
			put(p, value)
			p++
		default:
			differences := []byte{code[0]}
			if !gray {
				rest, err := take(2)
				if err != nil {
					return nil, err
				}
				differences = append(differences, rest...)
			}
			add(differences)
			put(p, value)
			p++
		}
	}
	return out, nil
}
