package pixeldata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/storedsample"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	// ErrInvalidSampleFormat reports BitsAllocated, BitsStored or HighBit
	// values that do not describe a stored sample.
	ErrInvalidSampleFormat = errors.New("dicom: invalid stored sample format")
	// ErrSampleTypeTooNarrow reports a destination integer type that cannot
	// hold every value of the format (for example signed samples into an
	// unsigned type, or 16-bit unsigned samples into int16).
	ErrSampleTypeTooNarrow = errors.New("dicom: destination type cannot hold every stored sample value")
	// ErrSampleBufferTooShort reports a destination shorter than the samples
	// in the source.
	ErrSampleBufferTooShort = errors.New("dicom: destination shorter than the stored samples")
	// ErrSampleDataLength reports source bytes that are not a whole number of
	// containers.
	ErrSampleDataLength = errors.New("dicom: stored sample bytes are not a whole number of samples")
	// ErrSampleBudgetExceeded reports more samples than the caller allows.
	ErrSampleBudgetExceeded = errors.New("dicom: stored samples exceed the caller budget")
	// ErrInvalidPixelPadding reports Pixel Padding Value or Range Limit
	// attributes that contradict the sample format.
	ErrInvalidPixelPadding = errors.New("dicom: invalid pixel padding")
)

// DefaultMaxStoredSamples bounds StoredSamples when the caller sets no budget.
const DefaultMaxStoredSamples = 1 << 28

// sampleCheckInterval is how many samples are interpreted between
// cancellation checks.
const sampleCheckInterval = 1 << 16

// SampleFormatError names the attribute that makes a format invalid.
type SampleFormatError struct {
	Field string
	Err   error
}

func (e *SampleFormatError) Error() string {
	return fmt.Sprintf("%s: %s: %v", ErrInvalidSampleFormat, e.Field, e.Err)
}

func (e *SampleFormatError) Unwrap() []error { return []error{ErrInvalidSampleFormat, e.Err} }

// SampleFormat describes how stored samples are laid out in pixel bytes. It is
// storage interpretation only: byte order, the BitsStored field at HighBit and
// sign extension from BitsStored. Rescale, LUTs, VOI, MONOCHROME1 inversion and
// colour conversion are not part of it, so a stored sample is not a modality
// value (for example Hounsfield units) until a modality transform is applied.
type SampleFormat struct {
	BitsAllocated int // container: 8, 16 or 32
	BitsStored    int
	HighBit       int
	// Signed selects two's complement of BitsStored (Pixel Representation 1).
	Signed bool
	// ByteOrder of 16- and 32-bit containers; nil is little-endian.
	ByteOrder binary.ByteOrder
}

// SampleFormat returns the format of frames in m, in the given byte order.
// Native frames are in their transfer syntax's byte order; frames decoded by
// a codec are little-endian.
func (m Metadata) SampleFormat(order binary.ByteOrder) SampleFormat {
	return SampleFormat{
		BitsAllocated: int(m.BitsAllocated),
		BitsStored:    int(m.BitsStored),
		HighBit:       int(m.HighBit),
		Signed:        m.PixelRepresentation != 0,
		ByteOrder:     order,
	}
}

func (f SampleFormat) reader() (storedsample.Reader, error) {
	r, err := storedsample.New(f.BitsAllocated, f.BitsStored, f.HighBit, f.Signed, f.ByteOrder)
	switch {
	case errors.Is(err, storedsample.ErrBitsAllocated):
		return r, &SampleFormatError{Field: "BitsAllocated", Err: err}
	case errors.Is(err, storedsample.ErrBitsStored):
		return r, &SampleFormatError{Field: "BitsStored", Err: err}
	case errors.Is(err, storedsample.ErrHighBit):
		return r, &SampleFormatError{Field: "HighBit", Err: err}
	}
	return r, nil
}

// Validate reports whether the format describes a stored sample. 1-bit and
// 64-bit containers are not integer stored samples and are refused.
func (f SampleFormat) Validate() error {
	_, err := f.reader()
	return err
}

// Range returns the smallest and largest stored value the format can hold.
func (f SampleFormat) Range() (minimum, maximum int64, err error) {
	r, err := f.reader()
	if err != nil {
		return 0, 0, err
	}
	minimum, maximum = r.Range()
	return minimum, maximum, nil
}

// StoredSampleReader interprets single samples of one format, for random
// access such as sampling a voxel or editing a pixel. Bulk reads use
// ReadStoredSamples.
type StoredSampleReader struct {
	r storedsample.Reader
}

// Reader prepares a StoredSampleReader for a valid format.
func (f SampleFormat) Reader() (StoredSampleReader, error) {
	r, err := f.reader()
	if err != nil {
		return StoredSampleReader{}, err
	}
	return StoredSampleReader{r: r}, nil
}

// Width is the container size in bytes.
func (r *StoredSampleReader) Width() int { return r.r.Width() }

// Value interprets the sample at the start of sample, which must hold at
// least Width bytes.
func (r *StoredSampleReader) Value(sample []byte) int64 { return r.r.Value(sample) }

// StoredInteger lists the destination types for interpreted stored samples.
type StoredInteger interface {
	~int8 | ~int16 | ~int32 | ~int64 | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// integerRange returns T's bounds without overflowing int64 for uint64.
func integerRange[T StoredInteger]() (minimum int64, maximum uint64) {
	var zero T
	bits := uint(unsafe.Sizeof(zero)) * 8
	if zero-1 < zero { // signed
		return -(int64(1) << (bits - 1)), uint64(1)<<(bits-1) - 1
	}
	if bits == 64 {
		return 0, math.MaxUint64
	}
	return 0, uint64(1)<<bits - 1
}

func checkedReader[T StoredInteger](f SampleFormat) (storedsample.Reader, error) {
	r, err := f.reader()
	if err != nil {
		return r, err
	}
	lo, hi := r.Range()
	tlo, thi := integerRange[T]()
	if lo < tlo || uint64(hi) > thi {
		var zero T
		return r, fmt.Errorf("%w: %T for %d-bit %s samples", ErrSampleTypeTooNarrow, zero, f.BitsStored, signedness(f.Signed))
	}
	return r, nil
}

func signedness(signed bool) string {
	if signed {
		return "signed"
	}
	return "unsigned"
}

func sampleCount(src []byte, width int) (int, error) {
	if len(src)%width != 0 {
		return 0, fmt.Errorf("%w: %d bytes in %d-byte containers", ErrSampleDataLength, len(src), width)
	}
	return len(src) / width, nil
}

// ReadStoredSamples interprets every sample in src into dst, in storage order
// (interleaved or planar exactly as stored), and returns the number written.
// It is lossless: T must hold the format's whole range, or
// ErrSampleTypeTooNarrow is returned before anything is written. src is only
// read and never retained; dst is the caller's buffer, so no allocation takes
// place. ctx is checked between blocks of samples; on cancellation or error
// the contents of dst are unspecified.
func ReadStoredSamples[T StoredInteger](ctx context.Context, dst []T, src []byte, format SampleFormat) (int, error) {
	r, err := checkedReader[T](format)
	if err != nil {
		return 0, err
	}
	n, err := sampleCount(src, r.Width())
	if err != nil {
		return 0, err
	}
	if len(dst) < n {
		return 0, fmt.Errorf("%w: %d slots for %d samples", ErrSampleBufferTooShort, len(dst), n)
	}
	width := r.Width()
	for start := 0; start < n; start += sampleCheckInterval {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		end := min(start+sampleCheckInterval, n)
		for i := start; i < end; i++ {
			dst[i] = T(r.Value(src[i*width:]))
		}
	}
	return n, nil
}

// StoredSamples is ReadStoredSamples into a new caller-owned slice. maxSamples
// bounds the allocation; 0 means DefaultMaxStoredSamples.
func StoredSamples[T StoredInteger](ctx context.Context, src []byte, format SampleFormat, maxSamples int) ([]T, error) {
	r, err := checkedReader[T](format)
	if err != nil {
		return nil, err
	}
	n, err := sampleCount(src, r.Width())
	if err != nil {
		return nil, err
	}
	if maxSamples <= 0 {
		maxSamples = DefaultMaxStoredSamples
	}
	if n > maxSamples {
		return nil, fmt.Errorf("%w: %d samples, limit %d", ErrSampleBudgetExceeded, n, maxSamples)
	}
	out := make([]T, n)
	if _, err := ReadStoredSamples(ctx, out, src, format); err != nil {
		return nil, err
	}
	return out, nil
}

// PixelPadding is the stored-value range excluded from statistics: Pixel
// Padding Value alone, or with Pixel Padding Range Limit the closed range
// between the two in either order (PS3.3 C.7.5.1.1.2).
type PixelPadding struct {
	Value      int64
	RangeLimit int64
	HasRange   bool
}

// Contains reports whether a stored value is padding.
func (p PixelPadding) Contains(stored int64) bool {
	if !p.HasRange {
		return stored == p.Value
	}
	return stored >= min(p.Value, p.RangeLimit) && stored <= max(p.Value, p.RangeLimit)
}

var (
	tagPixelPaddingValue      = core.NewTag(0x0028, 0x0120)
	tagPixelPaddingRangeLimit = core.NewTag(0x0028, 0x0121)
)

// PixelPaddingFromObject reads Pixel Padding Value and Range Limit as stored
// values of format. It returns nil without error when Pixel Padding Value is
// absent. The attributes must be US for unsigned and SS for signed samples
// (UN is read as the format requires) and must fit
// the format's range; anything else is ErrInvalidPixelPadding.
func PixelPaddingFromObject(obj *object.Object, format SampleFormat) (*PixelPadding, error) {
	lo, hi, err := format.Range()
	if err != nil {
		return nil, err
	}
	value, ok, err := paddingValue(obj, tagPixelPaddingValue, format.Signed)
	if err != nil || !ok {
		return nil, err
	}
	padding := &PixelPadding{Value: value}
	if limit, ok, err := paddingValue(obj, tagPixelPaddingRangeLimit, format.Signed); err != nil {
		return nil, err
	} else if ok {
		padding.RangeLimit, padding.HasRange = limit, true
	}
	for _, v := range []int64{padding.Value, padding.RangeLimit} {
		if v < lo || v > hi {
			return nil, fmt.Errorf("%w: value outside the %d-bit %s range", ErrInvalidPixelPadding, format.BitsStored, signedness(format.Signed))
		}
	}
	return padding, nil
}

func paddingValue(obj *object.Object, tag core.Tag, signed bool) (int64, bool, error) {
	if obj == nil {
		return 0, false, nil
	}
	elem, ok := obj.Get(tag)
	if !ok {
		return 0, false, nil
	}
	switch elem.VR() {
	case core.VRUS:
		if signed {
			return 0, false, fmt.Errorf("%w: %s is US for signed samples", ErrInvalidPixelPadding, tag)
		}
	case core.VRSS:
		if !signed {
			return 0, false, fmt.Errorf("%w: %s is SS for unsigned samples", ErrInvalidPixelPadding, tag)
		}
	case core.VRUN:
	default:
		return 0, false, fmt.Errorf("%w: %s has VR %s", ErrInvalidPixelPadding, tag, elem.VR())
	}
	switch values := elem.Value.(type) {
	case core.Uint16Value:
		if len(values) == 1 {
			return int64(values[0]), true, nil
		}
	case core.Int16Value:
		if len(values) == 1 {
			return int64(values[0]), true, nil
		}
	default:
		if raw, ok := elem.RawBytes(); ok && len(raw) == 2 {
			v := obj.ValueByteOrder().Uint16(raw)
			if signed {
				return int64(int16(v)), true, nil
			}
			return int64(v), true, nil
		}
	}
	return 0, false, fmt.Errorf("%w: %s must hold one 16-bit value", ErrInvalidPixelPadding, tag)
}

// StoredSampleStatistics summarizes interpreted stored samples. Minimum,
// Maximum and Mean are stored values, not modality values, and are meaningful
// only when Samples > 0.
type StoredSampleStatistics struct {
	Samples int64 // samples included
	Padding int64 // samples excluded as padding
	Minimum int64
	Maximum int64
	Mean    float64
}

// ComputeStoredSampleStatistics interprets every sample in src once and
// summarizes those that are not padding. padding may be nil.
func ComputeStoredSampleStatistics(ctx context.Context, src []byte, format SampleFormat, padding *PixelPadding) (StoredSampleStatistics, error) {
	r, err := format.reader()
	if err != nil {
		return StoredSampleStatistics{}, err
	}
	n, err := sampleCount(src, r.Width())
	if err != nil {
		return StoredSampleStatistics{}, err
	}
	stats := StoredSampleStatistics{Minimum: math.MaxInt64, Maximum: math.MinInt64}
	width := r.Width()
	// 128-bit two's complement sum, so the mean is exact before division
	// whatever the frame size.
	var sumHi int64
	var sumLo uint64
	for start := 0; start < n; start += sampleCheckInterval {
		if err := ctx.Err(); err != nil {
			return StoredSampleStatistics{}, err
		}
		end := min(start+sampleCheckInterval, n)
		for i := start; i < end; i++ {
			v := r.Value(src[i*width:])
			if padding != nil && padding.Contains(v) {
				stats.Padding++
				continue
			}
			stats.Samples++
			stats.Minimum = min(stats.Minimum, v)
			stats.Maximum = max(stats.Maximum, v)
			var carry uint64
			sumLo, carry = bits.Add64(sumLo, uint64(v), 0)
			sumHi += int64(carry)
			if v < 0 {
				sumHi--
			}
		}
	}
	if stats.Samples == 0 {
		stats.Minimum, stats.Maximum = 0, 0
		return stats, nil
	}
	stats.Mean = int128Float(sumHi, sumLo) / float64(stats.Samples)
	return stats, nil
}

// int128Float converts a two's complement 128-bit value through its
// magnitude, so small negative sums are not lost to rounding of the low word.
func int128Float(hi int64, lo uint64) float64 {
	if hi >= 0 {
		return float64(hi)*(1<<64) + float64(lo)
	}
	lo, borrow := bits.Sub64(0, lo, 0)
	magnitude := uint64(^hi) + 1 - borrow
	return -(float64(magnitude)*(1<<64) + float64(lo))
}
