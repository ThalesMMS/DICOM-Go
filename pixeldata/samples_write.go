package pixeldata

import (
	"errors"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/storedsample"
)

// ErrInvalidStoredSampleValue reports a NaN or infinite stored value.
var ErrInvalidStoredSampleValue = errors.New("dicom: stored sample value must be finite")

// StoredSampleWriter writes one prepared value into samples of a fixed format.
// Use SampleFormat.Writer to prepare it.
type StoredSampleWriter struct {
	layout storedsample.Reader
	value  int64
}

// Writer prepares a stored value for writing. It validates the format and
// rejects non-finite values. Finite values are rounded to the nearest integer
// with ties away from zero, then clamped to the format's signed or unsigned
// range. Callers working with modality values must first invert the modality
// transform.
func (f SampleFormat) Writer(value float64) (StoredSampleWriter, error) {
	layout, err := f.reader()
	if err != nil {
		return StoredSampleWriter{}, err
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return StoredSampleWriter{}, ErrInvalidStoredSampleValue
	}
	minimum, maximum := layout.Range()
	value = min(float64(maximum), max(float64(minimum), math.Round(value)))
	return StoredSampleWriter{layout: layout, value: int64(value)}, nil
}

// Write replaces the BitsStored field ending at HighBit in the first sample
// container of dst. Bits outside that field and later bytes are preserved.
// An uninitialized writer or insufficient destination returns an error
// without modifying dst.
func (w *StoredSampleWriter) Write(dst []byte) error {
	width := w.layout.Width()
	if width == 0 {
		return ErrInvalidSampleFormat
	}
	if len(dst) < width {
		return ErrSampleBufferTooShort
	}
	w.layout.Put(dst, w.value)
	return nil
}
