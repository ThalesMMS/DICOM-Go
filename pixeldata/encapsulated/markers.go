package encapsulated

import (
	"context"
	"fmt"
)

const (
	scanSOI = iota
	scanSOICode
	scanMarker
	scanMarkerCode
	scanLengthHigh
	scanLengthLow
	scanPayload
	scanEntropy
	scanEntropyCode
	scanEnd
)

// markerScanner identifies framing, not coding parameters. In particular APP,
// COM, tables and scan headers are opaque length-delimited payloads: embedded
// FF D9, FF D8 and DICOM delimiter bytes there have no framing significance.
type markerScanner struct {
	state, length, remaining int
	marker                   byte
	format                   Format
	resumeEntropy            bool
}

// ValidateFrame checks that a buffer contains exactly one structurally framed
// JPEG/JPEG-LS codestream, with at most one zero pad after EOI. It does not
// validate coding parameters or entropy. Decoders that otherwise ignore bytes
// after EOI can use it even when an offset table already supplied the bounds.
func ValidateFrame(ctx context.Context, data []byte, format Format) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if format != JPEG && format != JPEGLS && format != JPEG2000 {
		return ErrLayout
	}
	s := newFrameScanner(format)
	consumed, end, err := scanFrameChunk(ctx, s, data)
	if err != nil {
		return err
	}
	if end {
		remaining := data[consumed:]
		if len(remaining) > 1 || len(remaining) == 1 && !validFramePad(format, remaining[0], len(data)) {
			return fmt.Errorf("%w: trailing bytes after EOI", ErrLayout)
		}
		return ctx.Err()
	}
	return fmt.Errorf("%w: missing EOI", ErrLayout)
}

// JPEGScanSelectionValues returns the selection value from every JPEG SOS in
// scan order. It reuses the structural frame scanner, so marker-like bytes in
// opaque segments and stuffed entropy are not scan headers. For SOF3 these are
// lossless predictors; the caller must establish SOF3 and validate entropy.
// Memory is bounded by the supplied frame size; cancellation is checked in
// bounded windows. The complete frame and each SOS layout must be valid.
func JPEGScanSelectionValues(ctx context.Context, data []byte) ([]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s := &markerScanner{format: JPEG}
	var values []int
	for i, b := range data {
		if i%markerScanWindow == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if s.state == scanLengthLow && s.marker == 0xda && s.length|int(b) < 8 {
			return nil, fmt.Errorf("%w: invalid SOS", ErrLayout)
		}
		if s.state == scanPayload && s.marker == 0xda && s.remaining == s.length-2 {
			if s.remaining < 6 || s.remaining > len(data)-i {
				return nil, fmt.Errorf("%w: invalid SOS", ErrLayout)
			}
			payload := data[i : i+s.remaining]
			count := int(payload[0])
			if count < 1 || len(payload) != 1+2*count+3 {
				return nil, fmt.Errorf("%w: invalid SOS", ErrLayout)
			}
			values = append(values, int(payload[1+2*count]))
		}
		end, err := s.feed(b)
		if err != nil {
			return nil, err
		}
		if end {
			remaining := data[i+1:]
			if len(remaining) > 1 || len(remaining) == 1 && !validFramePad(JPEG, remaining[0], len(data)) || len(values) == 0 {
				return nil, fmt.Errorf("%w: missing scan or trailing bytes", ErrLayout)
			}
			return values, ctx.Err()
		}
	}
	return nil, fmt.Errorf("%w: missing EOI", ErrLayout)
}

func (s *markerScanner) feed(b byte) (bool, error) {
	switch s.state {
	case scanSOI:
		if b != 0xff {
			return false, fmt.Errorf("%w: missing SOI", ErrLayout)
		}
		s.state = scanSOICode
	case scanSOICode:
		if b != 0xd8 {
			return false, fmt.Errorf("%w: missing SOI", ErrLayout)
		}
		s.state = scanMarker
	case scanMarker:
		if b != 0xff {
			return false, fmt.Errorf("%w: expected marker", ErrLayout)
		}
		s.state = scanMarkerCode
	case scanMarkerCode:
		if b == 0xff {
			return false, nil
		}
		return s.beginMarker(b, false)
	case scanLengthHigh:
		s.length = int(b) << 8
		s.state = scanLengthLow
	case scanLengthLow:
		s.length |= int(b)
		if s.length < 2 {
			return false, fmt.Errorf("%w: invalid marker length", ErrLayout)
		}
		s.remaining = s.length - 2
		s.state = scanPayload
		if s.remaining == 0 {
			s.endSegment()
		}
	case scanPayload:
		s.remaining--
		if s.remaining == 0 {
			s.endSegment()
		}
	case scanEntropy:
		if b == 0xff {
			s.state = scanEntropyCode
		}
	case scanEntropyCode:
		if b == 0xff {
			return false, nil
		}
		if b == 0 || s.format == JPEGLS && b < 0x80 {
			s.state = scanEntropy
			return false, nil
		}
		return s.beginMarker(b, true)
	case scanEnd:
		return false, fmt.Errorf("%w: data after EOI", ErrLayout)
	}
	return false, nil
}

func (s *markerScanner) beginMarker(b byte, entropy bool) (bool, error) {
	if b == 0 || b == 0xd8 {
		return false, fmt.Errorf("%w: invalid/nested marker", ErrLayout)
	}
	if b == 0xd9 {
		s.state = scanEnd
		return true, nil
	}
	if b == 0x01 || b >= 0xd0 && b <= 0xd7 {
		if !entropy {
			return false, fmt.Errorf("%w: standalone marker outside scan", ErrLayout)
		}
		s.state = scanEntropy
		return false, nil
	}
	s.marker = b
	s.resumeEntropy = entropy && b == 0xdc // DNL resumes the interrupted scan.
	s.state = scanLengthHigh
	return false, nil
}
func (s *markerScanner) endSegment() {
	s.state = scanMarker
	if s.marker == 0xda || s.resumeEntropy {
		s.state = scanEntropy
	}
}

func inferFrames(ctx context.Context, p *Plan, frames int, format Format, limits Limits) ([]int, error) {
	starts, _, _, err := scanFrameStarts(ctx, p, frames, format, limits, false)
	return starts, err
}

func scanFrameStarts(ctx context.Context, p *Plan, frames int, format Format, limits Limits, prefix bool) ([]int, int, int, error) {
	starts := make([]int, 0, frames)
	starts = append(starts, 0)
	scanner := newFrameScanner(format)
	buffer := make([]byte, 32<<10)
	completed := 0
	lastComplete := 0
	var frameBytes uint64
	for i, size := range p.sizes {
		if i-starts[len(starts)-1]+1 > limits.MaxFragmentsPerFrame || size > limits.MaxFrameBytes-frameBytes {
			return nil, 0, 0, ErrResourceLimit
		}
		frameBytes += size
		end := false
		var eoiOffset uint64
		for offset := uint64(0); offset < size; {
			chunk := min(uint64(len(buffer)), size-offset)
			data := buffer[:int(chunk)]
			if err := readExact(ctx, p.source, i, data, int64(offset)); err != nil {
				return nil, 0, 0, err
			}
			if !end {
				consumed, complete, err := scanFrameChunk(ctx, scanner, data)
				if err != nil {
					return nil, 0, 0, err
				}
				end = complete
				if end {
					eoiOffset = offset + uint64(consumed) - 1
				}
				data = data[consumed:]
			}
			if end && len(data) != 0 {
				if len(data) != 1 || offset+chunk != size || offset+chunk-1 != eoiOffset+1 || !validFramePad(format, data[0], int(size)) {
					return nil, 0, 0, fmt.Errorf("%w: frame ends inside an Item or invalid padding", ErrLayout)
				}
			}
			offset += chunk
		}
		if !end {
			if size&1 != 0 {
				return nil, 0, 0, fmt.Errorf("%w: odd non-final fragment", ErrLayout)
			}
			continue
		}
		completed++
		lastComplete = i + 1
		if completed > frames || !prefix && completed == frames && i+1 < len(p.sizes) {
			return nil, 0, 0, fmt.Errorf("%w: %w", ErrLayout, ErrFrameCount)
		}
		if i+1 < len(p.sizes) {
			starts = append(starts, i+1)
			scanner = newFrameScanner(format)
			frameBytes = 0
		}
	}
	if prefix && completed > 0 {
		return starts[:completed], completed, lastComplete, nil
	}
	if completed != frames || len(starts) != frames {
		return nil, 0, 0, fmt.Errorf("%w: %w: incomplete codestream boundaries", ErrLayout, ErrFrameCount)
	}
	return starts, completed, lastComplete, nil
}
