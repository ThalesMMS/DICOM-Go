package imagebuild

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// SliceSource owns the source values and metadata. A slice remains valid until
// the next Slice call. Calls are serial; no source payload is retained after
// that instance's synchronous emit callback. Source count and contents must
// remain unchanged until WriteSeries returns; cancellation must unblock Slice.
type SliceSource interface {
	Slices() int
	Slice(context.Context, int) (Slice, error)
}

// StreamOptions admits the incremental path. MaxPixelBufferBytes bounds the
// simultaneously borrowed float64 slice and owned native16 pixel buffer (10
// bytes/pixel); it excludes caller-retained source storage and DICOM metadata.
// MaxSlices caps per-series work/identities. Both limits must be positive.
// Metadata inherited from Common and derivation remains caller-bounded.
type StreamOptions struct {
	MaxPixelBufferBytes int64
	MaxSlices           int
	// Replayable explicitly authorizes a first pass for one global rescale
	// and signed representation. The second pass emits the same source slices.
	Replayable bool
	// StoredRepresentation, 0 unsigned or 1 signed, together with Options.Rescale
	// enables one pass. Each value must fit those fixed parameters; no clipping.
	StoredRepresentation *uint16
}

type SeriesWriteResult struct {
	Instances           int
	SeriesInstanceUID   string
	StudyInstanceUID    string
	FrameOfReferenceUID string
	PixelBufferBytes    int64
}

func validateInstanceUIDs(ids []string, count int) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) != count {
		return fmt.Errorf("%w: SOP identities do not match slice count", ErrInvalid)
	}
	seen := make(map[string]bool, count)
	for _, id := range ids {
		if !core.IsValidUID(id) || seen[id] {
			return fmt.Errorf("%w: invalid or duplicate SOP identity", ErrInvalid)
		}
		seen[id] = true
	}
	return nil
}

// WriteSeries builds and serializes one classic instance at a time through the
// existing frame writer. emit must synchronously call write exactly once and
// may not retain it. It owns staging/publication: this library writes neither
// paths nor archives. An error may follow earlier complete emitted instances;
// callers needing all-or-nothing publication must stage the whole series.
// The streaming profile requires nil Slice.Valid; Build supports masked output.
func WriteSeries(ctx context.Context, geometry Geometry, source SliceSource, opts Options, limits StreamOptions, emit func(int, func(io.Writer) error) error) (SeriesWriteResult, error) {
	var result SeriesWriteResult
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if source == nil || emit == nil || limits.MaxSlices <= 0 || limits.MaxPixelBufferBytes <= 0 {
		return result, fmt.Errorf("%w: source, emitter and positive stream budgets required", ErrInvalid)
	}
	count := source.Slices()
	if count <= 0 || count > limits.MaxSlices {
		return result, fmt.Errorf("%w: slice count exceeds stream budget", ErrInvalid)
	}
	if geometry.Rows < 1 || geometry.Columns < 1 || geometry.Rows > math.MaxUint16 || geometry.Columns > math.MaxUint16 || int64(geometry.Rows)*int64(geometry.Columns) > limits.MaxPixelBufferBytes/10 {
		return result, fmt.Errorf("%w: slice exceeds pixel buffer budget", ErrInvalid)
	}
	result.PixelBufferBytes = int64(geometry.Rows) * int64(geometry.Columns) * 10
	if err := validateInstanceUIDs(opts.SOPInstanceUIDs, count); err != nil {
		return result, err
	}
	var rescale Rescale
	var signed bool
	if limits.StoredRepresentation != nil {
		if opts.Rescale == nil || *limits.StoredRepresentation > 1 {
			return result, fmt.Errorf("%w: one-pass representation requires fixed rescale and sign", ErrInvalid)
		}
		if _, _, err := chooseRescaleRange(opts.Rescale.Intercept, opts.Rescale.Intercept, true, opts.Rescale); err != nil {
			return result, err
		}
		rescale, signed = *opts.Rescale, *limits.StoredRepresentation == 1
	} else {
		if !limits.Replayable {
			return result, fmt.Errorf("%w: global rescale requires replayable source or fixed parameters", ErrInvalid)
		}
		low, high, integral := math.Inf(1), math.Inf(-1), true
		for i := 0; i < count; i++ {
			s, err := readStreamSlice(ctx, geometry, source, opts, i)
			if err != nil {
				return result, err
			}
			for j, v := range s.Values {
				if j%4096 == 0 {
					if err := ctx.Err(); err != nil {
						return result, err
					}
				}
				low, high = min(low, v), max(high, v)
				integral = integral && math.Abs(v-math.Round(v)) < 1e-6
			}
		}
		var err error
		rescale, signed, err = chooseRescaleRange(low, high, integral, opts.Rescale)
		if err != nil {
			return result, err
		}
	}
	seriesUID, frameUID, studyUID := strings.TrimSpace(opts.SeriesInstanceUID), strings.TrimSpace(geometry.FrameOfReferenceUID), ""
	for _, entry := range []struct {
		target   *string
		generate bool
	}{
		{&seriesUID, seriesUID == ""},
		{&frameUID, frameUID == "" && opts.hasImagePlane() && (opts.Common.Source == nil || !opts.Common.Source.Has(tagFrameOfReferenceUID))},
		{&studyUID, opts.Common.Source == nil || !opts.Common.Source.Has(tagStudyInstanceUID)},
	} {
		if entry.generate {
			uid, err := newUID()
			if err != nil {
				return result, err
			}
			*entry.target = uid
		}
	}
	result.SeriesInstanceUID, result.StudyInstanceUID, result.FrameOfReferenceUID = seriesUID, studyUID, frameUID
	now := opts.ContentTime
	if now.IsZero() {
		now = time.Now()
	}
	if rescale.Slope == 0 || math.IsNaN(rescale.Slope) || math.IsInf(rescale.Slope, 0) || math.IsNaN(rescale.Intercept) || math.IsInf(rescale.Intercept, 0) {
		return result, fmt.Errorf("%w: nonfinite global rescale", ErrInvalid)
	}
	pixels := make([]byte, 2*geometry.Rows*geometry.Columns)
	for i := 0; i < count; i++ {
		s, err := readStreamSlice(ctx, geometry, source, opts, i)
		if err != nil {
			return result, err
		}
		for j, v := range s.Values {
			if j%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return result, err
				}
			}
			stored := math.Round((v - rescale.Intercept) / rescale.Slope)
			low, high := 0.0, float64(math.MaxUint16)
			if signed {
				low, high = math.MinInt16, math.MaxInt16
			}
			if math.IsNaN(stored) || math.IsInf(stored, 0) || stored < low || stored > high {
				return result, fmt.Errorf("%w: slice %d value does not fit selected rescale", ErrInvalid, i+1)
			}
		}
		file, err := buildInstancePixels(ctx, geometry, s, i+1, opts, rescale, signed, seriesUID, studyUID, frameUID, now, pixels, nil)
		if err != nil {
			return result, err
		}
		if result.StudyInstanceUID == "" {
			result.StudyInstanceUID, _ = file.Dataset.GetUID(tagStudyInstanceUID)
		}
		if result.FrameOfReferenceUID == "" {
			result.FrameOfReferenceUID, _ = file.Dataset.GetUID(tagFrameOfReferenceUID)
		}
		pixel, _ := file.Dataset.Get(core.TagPixelData)
		frames := &streamImageFrame{ctx: ctx, data: pixel.Value.(core.RawValue)}
		called := false
		var writeErr error
		err = emit(i, func(w io.Writer) error {
			if called {
				writeErr = fmt.Errorf("%w: instance emitter called writer twice", ErrInvalid)
				return writeErr
			}
			called = true
			writeErr = object.WriteFileFrames(contextStreamWriter{ctx, w}, file, frames, object.FrameWriteOptions{})
			return writeErr
		})
		_ = file.Close()
		if err != nil {
			return result, err
		}
		if writeErr != nil {
			return result, writeErr
		}
		if !called {
			return result, fmt.Errorf("%w: instance emitter did not write", ErrInvalid)
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Instances++
	}
	return result, nil
}

func readStreamSlice(ctx context.Context, geometry Geometry, source SliceSource, opts Options, index int) (Slice, error) {
	if err := ctx.Err(); err != nil {
		return Slice{}, err
	}
	s, err := source.Slice(ctx, index)
	if err != nil {
		return Slice{}, err
	}
	if s.Valid != nil {
		return Slice{}, fmt.Errorf("%w: stream does not admit a validity mask", ErrInvalid)
	}
	validationOpts := opts
	validationOpts.SOPInstanceUIDs = nil
	if err := validate(geometry, []Slice{s}, validationOpts); err != nil {
		return Slice{}, err
	}
	for i, v := range s.Values {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return Slice{}, err
			}
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Slice{}, fmt.Errorf("%w: nonfinite slice value", ErrInvalid)
		}
	}
	return s, ctx.Err()
}

type contextStreamWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextStreamWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(data)
}

type streamImageFrame struct {
	ctx  context.Context
	data []byte
}

func (s *streamImageFrame) Frames() int                    { return 1 }
func (s *streamImageFrame) FrameLength(int) (int64, error) { return int64(len(s.data)), nil }
func (s *streamImageFrame) WriteFrameTo(_ int, w io.Writer) (int64, error) {
	var total int64
	for remaining := s.data; len(remaining) > 0; {
		if err := s.ctx.Err(); err != nil {
			return total, err
		}
		chunk := remaining[:min(len(remaining), 32<<10)]
		n, err := w.Write(chunk)
		total += int64(n)
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return total, err
		}
		remaining = remaining[n:]
	}
	return total, nil
}
