package encapsulated

import (
	"bytes"
	"context"
)

const markerScanWindow = 32 << 10

// scanFrameChunk preserves scanner state across chunks and stops immediately
// after the end marker. Payloads are opaque; only entropy without 0xff can be
// skipped by a marker search. Bound each search/skip to the cancellation window.
func scanFrameChunk(ctx context.Context, scanner frameScanner, data []byte) (consumed int, end bool, err error) {
	if s, ok := scanner.(*markerScanner); ok {
		return scanMarkerChunk(ctx, s, data)
	}
	// JPEG 2000 keeps its tile-part/packet-aware scanner unchanged.
	for i, b := range data {
		if i%markerScanWindow == 0 {
			if err := ctx.Err(); err != nil {
				return i, false, err
			}
		}
		end, err := scanner.feed(b)
		if end || err != nil {
			return i + 1, end, err
		}
	}
	return len(data), false, nil
}

func scanMarkerChunk(ctx context.Context, s *markerScanner, data []byte) (int, bool, error) {
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return offset, false, err
		}
		limit := offset + min(markerScanWindow, len(data)-offset)
		for offset < limit {
			switch s.state {
			case scanPayload:
				n := min(s.remaining, limit-offset)
				s.remaining -= n
				offset += n
				if s.remaining == 0 {
					s.endSegment()
				}
				continue
			case scanEntropy:
				if data[offset] != 0xff {
					i := bytes.IndexByte(data[offset:limit], 0xff)
					if i < 0 {
						offset = limit
						continue
					}
					offset += i
				}
				// The byte is known to be 0xff. Avoid a search and the
				// generic feed call for densely stuffed entropy.
				s.state = scanEntropyCode
				offset++
				continue
			case scanEntropyCode:
				b := data[offset]
				if b == 0xff {
					offset++ // fill byte; the marker code still follows
					continue
				}
				if b == 0 || s.format == JPEGLS && b < 0x80 {
					s.state = scanEntropy
					offset++
					continue
				}
			}
			end, err := s.feed(data[offset])
			offset++
			if end || err != nil {
				return offset, end, err
			}
		}
	}
	return len(data), false, nil
}
