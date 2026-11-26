package encapsulated

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// Retain the original byte-driven state machine as an independent path through
// the framing rules; the fast path must match its exact consumption and state.
func slowMarkerChunk(s *markerScanner, data []byte) (int, bool, error) {
	for i, b := range data {
		end, err := s.feed(b)
		if end || err != nil {
			return i + 1, end, err
		}
	}
	return len(data), false, nil
}

func equalMarkerError(a, b error) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Error() == b.Error() && errors.Is(a, ErrLayout) == errors.Is(b, ErrLayout)
}

func compareMarkerChunks(t testing.TB, data []byte, format Format, chunk int) {
	t.Helper()
	fast, slow := markerScanner{format: format}, markerScanner{format: format}
	for offset := 0; offset < len(data); {
		limit := offset + min(chunk, len(data)-offset)
		n, end, err := scanMarkerChunk(context.Background(), &fast, data[offset:limit])
		wantN, wantEnd, wantErr := slowMarkerChunk(&slow, data[offset:limit])
		if n != wantN || end != wantEnd || !equalMarkerError(err, wantErr) || fast != slow {
			t.Fatalf("format=%d chunk=%d offset=%d: fast %d/%t/%v %+v; slow %d/%t/%v %+v", format, chunk, offset, n, end, err, fast, wantN, wantEnd, wantErr, slow)
		}
		if end || err != nil {
			return
		}
		if n != limit-offset {
			t.Fatal("scanner made incomplete progress")
		}
		offset = limit
	}
}

func markerScanFixtures() [][]byte {
	longPayload := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0xff, 0xff}, bytes.Repeat([]byte{0xff, 0xd9, 0xff, 0xd8}, 16384)...)
	longPayload = longPayload[:6+65533]
	longPayload = append(longPayload, 0xff, 0xda, 0, 2, 1, 0xff, 0xd9)
	return [][]byte{
		boundaryStream([]byte{1, 2, 3, 0xff, 0, 4, 0xff, 0xd0, 5}),
		boundaryStream([]byte{1, 0xff, 0x7f, 2, 0xff, 0xff, 0, 3}),
		boundaryStream([]byte{1, 0xff, 0xdc, 0, 4, 0xff, 0xd9, 2, 0xff, 0x01, 3}),
		boundaryStream([]byte{1, 0xff, 0xc4, 0, 6, 0xff, 0xd9, 0xff, 0xd8, 0xff, 0xda, 0, 2, 3}),
		longPayload,
		boundaryStream(bytes.Repeat([]byte{1}, 3*markerScanWindow+7)),
		{0xff, 0xd8, 0xff, 0xe1, 0, 1},
		{0xff, 0xd8, 0xff, 0xd0},
		{0xff, 0xd8, 0xff, 0xda, 0, 2, 0xff, 0xd8},
		{0, 0xff, 0xd9},
	}
}

func TestMarkerBulkMatchesByteScanner(t *testing.T) {
	for _, format := range []Format{JPEG, JPEGLS} {
		for _, data := range markerScanFixtures() {
			for _, chunk := range []int{1, 2, 3, 5, 31, markerScanWindow - 1, markerScanWindow, markerScanWindow + 1, len(data)} {
				compareMarkerChunks(t, data, format, chunk)
			}
			// Every truncated prefix of the short structural fixtures is checked.
			if len(data) < 128 {
				for cut := range data {
					compareMarkerChunks(t, data[:cut], format, 7)
				}
			}
		}
	}
}

type markerCancelWindow struct {
	context.Context
	calls int
}

func (c *markerCancelWindow) Err() error {
	c.calls++
	if c.calls >= 2 {
		return context.Canceled
	}
	return nil
}

func TestMarkerBulkCancellationWindow(t *testing.T) {
	for _, state := range []int{scanEntropy, scanPayload} {
		ctx := &markerCancelWindow{Context: context.Background()}
		s := markerScanner{state: state, remaining: 2 * markerScanWindow, format: JPEG}
		n, end, err := scanMarkerChunk(ctx, &s, bytes.Repeat([]byte{1}, 2*markerScanWindow))
		if !errors.Is(err, context.Canceled) || end || n != markerScanWindow || ctx.calls != 2 {
			t.Fatalf("state=%d: consumed=%d end=%t err=%v checks=%d", state, n, end, err, ctx.calls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := markerScanner{format: JPEG}
	if n, end, err := scanMarkerChunk(ctx, &s, boundaryStream([]byte{1})); n != 0 || end || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel: %d %t %v", n, end, err)
	}
}

func FuzzMarkerBulkMatchesByteScanner(f *testing.F) {
	for i, data := range markerScanFixtures() {
		f.Add(data, uint16(31), byte(i&1))
	}
	f.Fuzz(func(t *testing.T, data []byte, chunk uint16, mode byte) {
		if len(data) > 128<<10 {
			return
		}
		format := JPEG
		if mode&1 != 0 {
			format = JPEGLS
		}
		compareMarkerChunks(t, data, format, int(chunk)+1)
	})
}
