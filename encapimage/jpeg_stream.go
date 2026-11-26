package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
)

// JPEG marker codes (ISO/IEC 10918-1 Table B.1).
const (
	markerSOF0 = 0xc0
	markerSOF1 = 0xc1
	markerSOF2 = 0xc2
	markerSOF3 = 0xc3
	markerDHT  = 0xc4
	markerJPG  = 0xc8
	markerDAC  = 0xcc
	markerSOI  = 0xd8
	markerEOI  = 0xd9
	markerSOS  = 0xda
	markerAPP0 = 0xe0
	markerAPP2 = 0xe2
	markerAPPE = 0xee
	markerCOM  = 0xfe
	markerTEM  = 0x01
)

// frameComponent is one component of the SOFn frame header.
type frameComponent struct {
	id         byte
	horizontal int
	vertical   int
}

// jpegStream is what the encapsulation needs to know about a JPEG
// interchange-format stream after one pass over its markers.
type jpegStream struct {
	sofMarker  byte
	sofCount   int
	precision  int
	rows       int
	columns    int
	components []frameComponent
	// predictors holds the Ss (predictor selection) of every scan; it is only
	// meaningful for the lossless process.
	predictors []int
	jfif       bool
	// adobeTransform is the APP14 Adobe transform flag, -1 when absent.
	adobeTransform int
	// segments lists every marker segment and scan before EOI in order;
	// out is assembled from those that are kept.
	segments []streamSegment
	// eoi is the offset just past the EOI marker.
	eoi int
}

type streamSegment struct {
	marker byte
	start  int
	end    int
}

func (s streamSegment) payload(data []byte) []byte {
	if s.end-s.start < 4 {
		return nil
	}
	return data[s.start+4 : s.end]
}

// parseJPEGStream walks data from SOI to EOI. Entropy-coded data is skipped
// by looking for the next marker that is neither a stuffed zero nor RSTn. It
// reads only the bytes it was given and never allocates in proportion to the
// declared sizes.
func parseJPEGStream(data []byte) (jpegStream, error) { return parseJPEGStreamLimited(nil, data, 0) }

// parseJPEGStreamLimited shares the legacy parser. Limits apply only to the
// new bounded importer; zero and nil preserve the existing FromJPEG contract.
func parseJPEGStreamLimited(ctx context.Context, data []byte, maxSegments int) (jpegStream, error) {
	s := jpegStream{adobeTransform: -1}
	if len(data) < 4 || data[0] != 0xff || data[1] != markerSOI {
		return s, ErrNotJPEG
	}
	s.segments = append(s.segments, streamSegment{marker: markerSOI, start: 0, end: 2})
	i := 2
	for {
		if ctx != nil {
			if e := ctx.Err(); e != nil {
				return s, e
			}
		}
		if maxSegments > 0 && len(s.segments) >= maxSegments {
			return s, ErrImportLimit
		}
		if i+2 > len(data) {
			return s, fmt.Errorf("%w: no EOI marker", ErrTruncatedJPEG)
		}
		if data[i] != 0xff {
			return s, fmt.Errorf("%w: expected a marker at offset %d", ErrMalformedJPEG, i)
		}
		marker := data[i+1]
		if marker == 0xff { // fill byte
			i++
			continue
		}
		switch {
		case marker == markerEOI:
			s.segments = append(s.segments, streamSegment{marker: marker, start: i, end: i + 2})
			s.eoi = i + 2
			if s.sofCount == 0 {
				return s, fmt.Errorf("%w: no frame header", ErrMalformedJPEG)
			}
			return s, nil
		case marker == markerSOI:
			return s, fmt.Errorf("%w: SOI inside the stream", ErrMalformedJPEG)
		case marker == markerTEM || marker >= 0xd0 && marker <= 0xd7 || marker == 0x00:
			return s, fmt.Errorf("%w: marker FF%02X outside a scan", ErrMalformedJPEG, marker)
		}
		if i+4 > len(data) {
			return s, fmt.Errorf("%w: marker FF%02X without a length", ErrTruncatedJPEG, marker)
		}
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		end := i + 2 + length
		if length < 2 || end > len(data) {
			return s, fmt.Errorf("%w: marker FF%02X segment of %d bytes", ErrTruncatedJPEG, marker, length)
		}
		segment := streamSegment{marker: marker, start: i, end: end}
		if err := s.inspect(segment, data); err != nil {
			return s, err
		}
		if marker == markerSOS {
			var e error
			segment.end, e = scanEndContext(ctx, data, end)
			if e != nil {
				return s, e
			}
			if segment.end == len(data) {
				return s, fmt.Errorf("%w: scan without a following marker", ErrTruncatedJPEG)
			}
		}
		s.segments = append(s.segments, segment)
		i = segment.end
	}
}

// scanEnd returns the offset of the first marker after the entropy-coded
// data that starts at i, or len(data).
func scanEnd(data []byte, i int) int {
	for {
		j := bytes.IndexByte(data[i:], 0xff)
		if j < 0 {
			return len(data)
		}
		i += j
		k := i + 1
		for k < len(data) && data[k] == 0xff {
			k++
		}
		if k >= len(data) {
			return len(data)
		}
		if next := data[k]; next == 0x00 || next >= 0xd0 && next <= 0xd7 {
			i = k + 1
			continue
		}
		return k - 1
	}
}

// scanEndContext retains the legacy scan for nil context. The bounded importer
// checks context between at most 4 KiB searches and within fill-byte runs.
func scanEndContext(ctx context.Context, data []byte, i int) (int, error) {
	if ctx == nil {
		return scanEnd(data, i), nil
	}
	for i < len(data) {
		if e := ctx.Err(); e != nil {
			return 0, e
		}
		end := len(data)
		if len(data)-i > 4096 {
			end = i + 4096
		}
		j := bytes.IndexByte(data[i:end], 0xff)
		if j < 0 {
			i = end
			continue
		}
		i += j
		k := i + 1
		for k < len(data) && data[k] == 0xff {
			if (k-i)&4095 == 0 {
				if e := ctx.Err(); e != nil {
					return 0, e
				}
			}
			k++
		}
		if k >= len(data) {
			return len(data), nil
		}
		if next := data[k]; next == 0 || next >= 0xd0 && next <= 0xd7 {
			i = k + 1
			continue
		}
		return k - 1, nil
	}
	return len(data), nil
}

func isSOF(marker byte) bool {
	return marker >= 0xc0 && marker <= 0xcf && marker != markerDHT && marker != markerJPG && marker != markerDAC
}

func (s *jpegStream) inspect(segment streamSegment, data []byte) error {
	payload := segment.payload(data)
	switch marker := segment.marker; {
	case isSOF(marker):
		s.sofCount++
		if s.sofCount > 1 {
			return fmt.Errorf("%w: more than one frame header", ErrUnsupportedJPEGProcess)
		}
		if len(payload) < 6 {
			return fmt.Errorf("%w: short frame header", ErrMalformedJPEG)
		}
		s.sofMarker = marker
		s.precision = int(payload[0])
		s.rows = int(binary.BigEndian.Uint16(payload[1:]))
		s.columns = int(binary.BigEndian.Uint16(payload[3:]))
		count := int(payload[5])
		if len(payload) < 6+3*count {
			return fmt.Errorf("%w: frame header declares %d components", ErrMalformedJPEG, count)
		}
		for c := range count {
			sampling := payload[6+3*c+1]
			s.components = append(s.components, frameComponent{
				id:         payload[6+3*c],
				horizontal: int(sampling >> 4),
				vertical:   int(sampling & 0x0f),
			})
		}
	case marker == markerSOS:
		if s.sofCount == 0 {
			return fmt.Errorf("%w: scan before the frame header", ErrMalformedJPEG)
		}
		if len(payload) < 1 || len(payload) < 1+2*int(payload[0])+3 {
			return fmt.Errorf("%w: short scan header", ErrMalformedJPEG)
		}
		s.predictors = append(s.predictors, int(payload[1+2*int(payload[0])]))
	case marker == markerAPP0:
		if bytes.HasPrefix(payload, []byte("JFIF\x00")) {
			s.jfif = true
		}
	case marker == markerAPPE:
		if len(payload) >= 12 && bytes.HasPrefix(payload, []byte("Adobe")) {
			s.adobeTransform = int(payload[11])
		}
	}
	return nil
}
