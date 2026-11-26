// Package display implements the DICOM grayscale display pipeline as a small,
// staged interface that callers can use without sequencing the individual
// transforms themselves.
//
// The pipeline follows the order defined by DICOM PS3.3 C.11 and PS3.4 Annex N:
//
//	stored pixel value -> Modality transform -> VOI transform -> display value
//
// The Modality transform is either a linear Rescale Slope/Intercept or a
// Modality LUT Sequence; the VOI transform is either a Window Center/Width with
// a window function or a VOI LUT Sequence. The package also provides
// presentation-stage helpers for inversion, LUTs, overlays, and shutters.
// RenderColor supplies shared RGB, palette, and YBR conversion alongside the
// grayscale modality and VOI pipeline.
package display

import (
	"encoding/binary"
	"errors"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/storedsample"
)

var (
	// ErrInvalidFrame reports geometry or pixel-format values that cannot
	// describe a renderable grayscale frame.
	ErrInvalidFrame = errors.New("dicom/display: invalid frame metadata")
	// ErrUnsupportedBitsAllocated reports a BitsAllocated the requested stage
	// does not handle. Modality decoding supports 8, 16, and 32 bits; RenderGray
	// intentionally remains limited to 8 and 16 bits.
	ErrUnsupportedBitsAllocated = errors.New("dicom/display: unsupported BitsAllocated")
	// ErrPixelDataTooShort reports pixel data shorter than the frame requires.
	ErrPixelDataTooShort = errors.New("dicom/display: pixel data too short for frame")
	// ErrDestinationTooShort reports a caller-provided modality buffer that
	// cannot hold one value for every frame pixel.
	ErrDestinationTooShort = errors.New("dicom/display: modality destination too short")
	// ErrFrameSizeOverflow reports frame dimensions whose pixel, stride, source,
	// or destination size cannot be represented safely by an int.
	ErrFrameSizeOverflow = errors.New("dicom/display: frame size overflow")
)

// FrameValidationError identifies the rejected frame field without including
// caller-controlled metadata or pixel payload in the error message.
type FrameValidationError struct {
	Field string
	Err   error
}

func (e *FrameValidationError) Error() string {
	if e == nil || e.Err == nil {
		return ErrInvalidFrame.Error()
	}
	if e.Field == "" {
		return e.Err.Error()
	}
	return e.Err.Error() + ": " + e.Field
}

func (e *FrameValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// PixelFormat describes how to read a single stored sample from a frame's pixel
// bytes. It mirrors the DICOM Image Pixel module attributes that govern stored
// value interpretation.
type PixelFormat struct {
	BitsAllocated int
	BitsStored    int
	HighBit       int
	// Signed reports whether stored values are two's-complement (Pixel
	// Representation 1).
	Signed bool
	// ByteOrder is the byte order of multi-byte samples; nil defaults to
	// little-endian.
	ByteOrder binary.ByteOrder
}

// Frame is the input to the grayscale display pipeline: one frame's stored
// pixel bytes plus the transforms to apply.
type Frame struct {
	Rows    int
	Columns int
	// Pixels holds one frame of stored, single-sample grayscale pixel data.
	Pixels []byte
	Format PixelFormat
	// Modality maps stored values to modality values.
	Modality ModalityLUT
	// VOI maps modality values to display values. When neither a window nor a
	// VOI LUT is set, a full-range default window is derived from the pixel
	// format and modality transform.
	VOI VOILUT
}

// Pipeline renders DICOM pixel data to a displayable image, applying the
// modality, VOI, and color transforms in DICOM-defined order so callers need
// not sequence the transforms themselves.
type Pipeline interface {
	RenderGray(Frame) (*image.Gray, error)
	RenderColor(ColorFrame) (*image.RGBA, error)
}

// pipeline is the default Pipeline implementation.
type pipeline struct{}

// New returns the default display Pipeline.
func New() Pipeline { return pipeline{} }

// RenderGray renders frame through the default pipeline.
func (pipeline) RenderGray(frame Frame) (*image.Gray, error) {
	return RenderGray(frame)
}

// RenderColor renders a color frame through the default pipeline.
func (pipeline) RenderColor(frame ColorFrame) (*image.RGBA, error) {
	return RenderColor(frame)
}

// RenderGray applies the modality and VOI transforms to a grayscale frame and
// returns an 8-bit image using the MONOCHROME2 convention (larger display value
// is brighter). MONOCHROME1 inversion is a presentation-stage concern handled
// by callers/later pipeline stages.
func RenderGray(frame Frame) (*image.Gray, error) {
	if err := validateFrameDimensions(frame.Rows, frame.Columns); err != nil {
		return nil, err
	}
	bitsAllocated := frame.Format.BitsAllocated
	if bitsAllocated != 8 && bitsAllocated != 16 {
		return nil, frameValidationError("BitsAllocated", ErrUnsupportedBitsAllocated)
	}
	bytesPerSample := bitsAllocated / 8
	layout, err := checkedFrameLayout(frame.Rows, frame.Columns, 1, bytesPerSample, 1)
	if err != nil {
		return nil, err
	}
	if len(frame.Pixels) < layout.sourceBytes {
		return nil, frameValidationError("Pixels", ErrPixelDataTooShort)
	}

	order := frame.Format.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}

	out := image.NewGray(image.Rect(0, 0, frame.Columns, frame.Rows))
	reader := newStoredPixelReader(frame.Format, order)
	mapper := frameVOIMapper(frame.VOI, frame.Modality, &reader)
	slope := frame.Modality.Slope
	if slope == 0 {
		slope = 1
	}
	intercept := frame.Modality.Intercept
	displayByte := func(stored int64) uint8 {
		if frame.Modality.LUT != nil {
			return mapper.Byte(float64(frame.Modality.LUT.lookupInt64(stored)))
		}
		return mapper.Byte(float64(stored)*slope + intercept)
	}
	// Every stored value maps to one display byte: when there are no more
	// possible stored values than pixels, map each once into a table.
	bitsStored := frame.Format.BitsStored
	if bitsStored <= 0 || bitsStored > bitsAllocated {
		bitsStored = bitsAllocated
	}
	if values := 1 << bitsStored; values <= layout.pixels {
		lowest := int64(0)
		if frame.Format.Signed {
			lowest = -int64(values / 2)
		}
		table := make([]uint8, values)
		for i := range table {
			table[i] = displayByte(lowest + int64(i))
		}
		for pixelIndex := 0; pixelIndex < layout.pixels; pixelIndex++ {
			out.Pix[pixelIndex] = table[reader.Value(frame.Pixels[pixelIndex*bytesPerSample:])-lowest]
		}
		return out, nil
	}
	for pixelIndex := 0; pixelIndex < layout.pixels; pixelIndex++ {
		out.Pix[pixelIndex] = displayByte(reader.Value(frame.Pixels[pixelIndex*bytesPerSample:]))
	}
	return out, nil
}

// DecodeModality extracts each stored pixel of a grayscale frame and applies the
// modality transform, returning one modality value per pixel in row-major order.
// It is the decode half of the grayscale pipeline, exposed for callers (such as
// render caches) that window the same modality values repeatedly without
// re-extracting stored pixels.
func DecodeModality(frame Frame) ([]float64, error) {
	return decodeModality[float64](frame, 8)
}

// DecodeModalityFloat32 is DecodeModality with float32 output. It is intended
// for memory-sensitive render caches whose downstream calculations already use
// float32 precision, avoiding a transient float64 buffer and conversion pass.
func DecodeModalityFloat32(frame Frame) ([]float32, error) {
	return decodeModality[float32](frame, 4)
}

// DecodeModalityFloat32Into is DecodeModalityFloat32 without an output
// allocation. It writes Rows*Columns values into destination and leaves any
// remaining capacity untouched.
func DecodeModalityFloat32Into(destination []float32, frame Frame) error {
	return decodeModalityInto(destination, frame, 4)
}

// DecodeStoredInto writes the stored samples of a grayscale 16-bit frame,
// masked to BitsStored and sign-extended when Signed, without the modality
// transform: compact volumes keep them with one rescale (#1065). It fails for
// other BitsAllocated and for samples outside T's range (unsigned 16-bit
// samples into int16).
func DecodeStoredInto[T int16 | uint16](destination []T, frame Frame) error {
	if frame.Format.BitsAllocated != 16 {
		return frameValidationError("BitsAllocated", ErrUnsupportedBitsAllocated)
	}
	count, err := modalitySampleCount(frame, 2)
	if err != nil {
		return err
	}
	if len(destination) < count {
		return frameValidationError("Destination", ErrDestinationTooShort)
	}
	order := frame.Format.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}
	var zero T
	minimum, maximum := int64(0), int64(math.MaxUint16)
	if _, signed := any(zero).(int16); signed {
		minimum, maximum = math.MinInt16, math.MaxInt16
	}
	reader := newStoredPixelReader(frame.Format, order)
	for i := range destination[:count] {
		stored := reader.Value(frame.Pixels[i*2:])
		if stored < minimum || stored > maximum {
			return frameValidationError("Pixels", ErrInvalidFrame)
		}
		destination[i] = T(stored)
	}
	return nil
}

// MaxModalityLevels is the largest level table DecodeModalityLevels builds:
// what a uint16 index addresses.
const MaxModalityLevels = 1 << 16

// DecodeModalityLevels is DecodeModalityFloat32 in compact form for 8- and
// 16-bit frames: index holds each pixel's stored value minus the smallest one,
// and levels holds the float32 modality value of every stored value in that
// range, computed exactly as DecodeModalityFloat32 computes it, so
// levels[index[i]] equals its i-th value bit for bit. Render caches keep the
// 2-byte index instead of a 4-byte plane and window a frame by mapping each
// level once (#1612). ok is false, with no error, when the stored range spans
// more than MaxModalityLevels values or more values than the frame has pixels;
// callers then decode the float32 plane.
func DecodeModalityLevels(frame Frame) (index []uint16, levels []float32, ok bool, err error) {
	count, err := modalitySampleCount(frame, 4)
	if err != nil {
		return nil, nil, false, err
	}
	bitsAllocated := frame.Format.BitsAllocated
	if bitsAllocated != 8 && bitsAllocated != 16 {
		return nil, nil, false, nil
	}
	bytesPerSample := bitsAllocated / 8
	order := frame.Format.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}
	reader := newStoredPixelReader(frame.Format, order)
	// Stored values of 8- and 16-bit samples fit 16 bits once masked and
	// sign-extended, so the first pass keeps their bits in the index and the
	// second only subtracts the lowest value.
	index = make([]uint16, count)
	lowest, highest := int64(math.MaxInt64), int64(math.MinInt64)
	for i := range index {
		stored := reader.Value(frame.Pixels[i*bytesPerSample:])
		index[i] = uint16(stored)
		lowest, highest = min(lowest, stored), max(highest, stored)
	}
	span := highest - lowest + 1
	if span > MaxModalityLevels || span > int64(count) {
		return nil, nil, false, nil
	}
	levels = make([]float32, span)
	if frame.Modality.LUT != nil {
		for k := range levels {
			levels[k] = float32(frame.Modality.LUT.lookupInt64(lowest + int64(k)))
		}
	} else {
		slope := frame.Modality.Slope
		if slope == 0 {
			slope = 1
		}
		intercept := frame.Modality.Intercept
		for k := range levels {
			levels[k] = float32(float64(lowest+int64(k))*slope + intercept)
		}
	}
	// Subtracting in 16-bit two's complement gives stored - lowest for signed
	// and unsigned values alike, since the span fits 16 bits.
	offset := uint16(lowest)
	for i := range index {
		index[i] -= offset
	}
	return index, levels, true, nil
}

func decodeModality[T ~float32 | ~float64](frame Frame, outputBytesPerPixel int) ([]T, error) {
	count, err := modalitySampleCount(frame, outputBytesPerPixel)
	if err != nil {
		return nil, err
	}
	out := make([]T, count)
	if err := decodeModalityInto(out, frame, outputBytesPerPixel); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeModalityInto[T ~float32 | ~float64](out []T, frame Frame, outputBytesPerPixel int) error {
	count, err := modalitySampleCount(frame, outputBytesPerPixel)
	if err != nil {
		return err
	}
	if len(out) < count {
		return frameValidationError("Destination", ErrDestinationTooShort)
	}
	out = out[:count]
	bitsAllocated := frame.Format.BitsAllocated
	bytesPerSample := bitsAllocated / 8
	order := frame.Format.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}
	reader := newStoredPixelReader(frame.Format, order)
	if frame.Modality.LUT != nil {
		for i := range out {
			stored := reader.Value(frame.Pixels[i*bytesPerSample:])
			out[i] = T(frame.Modality.LUT.lookupInt64(stored))
		}
	} else {
		slope := frame.Modality.Slope
		if slope == 0 {
			slope = 1
		}
		intercept := frame.Modality.Intercept
		for i := range out {
			stored := reader.Value(frame.Pixels[i*bytesPerSample:])
			out[i] = T(float64(stored)*slope + intercept)
		}
	}
	return nil
}

func modalitySampleCount(frame Frame, outputBytesPerPixel int) (int, error) {
	if err := validateFrameDimensions(frame.Rows, frame.Columns); err != nil {
		return 0, err
	}
	bitsAllocated := frame.Format.BitsAllocated
	if bitsAllocated != 8 && bitsAllocated != 16 && bitsAllocated != 32 {
		return 0, frameValidationError("BitsAllocated", ErrUnsupportedBitsAllocated)
	}
	bytesPerSample := bitsAllocated / 8
	layout, err := checkedFrameLayout(frame.Rows, frame.Columns, 1, bytesPerSample, outputBytesPerPixel)
	if err != nil {
		return 0, err
	}
	if len(frame.Pixels) < layout.sourceBytes {
		return 0, frameValidationError("Pixels", ErrPixelDataTooShort)
	}
	return layout.pixels, nil
}

type frameLayout struct {
	pixels      int
	sourceBytes int
}

func checkedFrameLayout(rows, columns, samplesPerPixel, bytesPerSample, outputBytesPerPixel int) (frameLayout, error) {
	if err := validateFrameDimensions(rows, columns); err != nil {
		return frameLayout{}, err
	}
	if samplesPerPixel <= 0 {
		return frameLayout{}, frameValidationError("SamplesPerPixel", ErrInvalidFrame)
	}
	if bytesPerSample <= 0 {
		return frameLayout{}, frameValidationError("BytesPerSample", ErrInvalidFrame)
	}
	if outputBytesPerPixel <= 0 {
		return frameLayout{}, frameValidationError("OutputBytesPerPixel", ErrInvalidFrame)
	}

	pixels, ok := checkedDisplayMul(rows, columns)
	if !ok {
		return frameLayout{}, frameValidationError("PixelCount", ErrFrameSizeOverflow)
	}
	samples, ok := checkedDisplayMul(pixels, samplesPerPixel)
	if !ok {
		return frameLayout{}, frameValidationError("SourceStride", ErrFrameSizeOverflow)
	}
	sourceBytes, ok := checkedDisplayMul(samples, bytesPerSample)
	if !ok {
		return frameLayout{}, frameValidationError("SourceBytes", ErrFrameSizeOverflow)
	}
	if _, ok := checkedDisplayMul(pixels, outputBytesPerPixel); !ok {
		return frameLayout{}, frameValidationError("OutputBytes", ErrFrameSizeOverflow)
	}
	return frameLayout{pixels: pixels, sourceBytes: sourceBytes}, nil
}

func validateFrameDimensions(rows, columns int) error {
	if rows <= 0 {
		return frameValidationError("Rows", ErrInvalidFrame)
	}
	if columns <= 0 {
		return frameValidationError("Columns", ErrInvalidFrame)
	}
	return nil
}

func checkedDisplayMul(left, right int) (int, bool) {
	if left < 0 || right < 0 {
		return 0, false
	}
	if left != 0 && right > int(^uint(0)>>1)/left {
		return 0, false
	}
	return left * right, true
}

func frameValidationError(field string, err error) error {
	return &FrameValidationError{Field: field, Err: err}
}

// scaleToByte linearly maps a VOI LUT output value in [min, min+span] to the
// 8-bit display range. A zero span (constant LUT) maps everything to black.
func scaleToByte(value, minValue, span float64) uint8 {
	if span <= 0 {
		return 0
	}
	scaled := (value - minValue) / span * 255
	return uint8(math.Round(clampFloat(scaled, 0, 255)))
}

// storedPixelValue extracts one stored sample, honoring BitsStored, HighBit,
// and the signed/unsigned representation.
func storedPixelValue(data []byte, format PixelFormat, order binary.ByteOrder) int64 {
	reader := newStoredPixelReader(format, order)
	return reader.Value(data)
}

// newStoredPixelReader returns the shared stored-sample reader with the
// display pipeline's historical repair of BitsStored and HighBit.
func newStoredPixelReader(format PixelFormat, order binary.ByteOrder) storedsample.Reader {
	return storedsample.Legacy(format.BitsAllocated, format.BitsStored, format.HighBit, format.Signed, order)
}

// frameVOIMapper uses the transformed endpoints of the stored value domain
// when no VOI is supplied.
func frameVOIMapper(voi VOILUT, modality ModalityLUT, reader *storedsample.Reader) VOIByteMapper {
	if voi.LUT != nil || voi.hasWindow() {
		return NewVOIByteMapper(voi)
	}
	minStored, maxStored := reader.Range()
	minValue := modality.Apply(int(minStored))
	maxValue := modality.Apply(int(maxStored))
	if minValue > maxValue {
		minValue, maxValue = maxValue, minValue
	}
	span := maxValue - minValue
	if span > 0 && finiteWindowValue(span) {
		// PS3.3 C.11.2.1.2.1 gives LINEAR a half-sample shift and a
		// width minus one. Mapping the endpoints directly is equivalent
		// to the corresponding full-range window and retains small spans.
		return VOIByteMapper{rangeMin: minValue, rangeSpan: span}
	}
	// Keep the existing threshold behavior for a collapsed range.
	return NewVOIByteMapper(VOILUT{Center: (minValue + maxValue) / 2, Width: span + 1})
}
