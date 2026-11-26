package render

import (
	"bytes"
	"encoding/binary"
	"image"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

type windowLevelsCase struct {
	name        string
	bits        uint16
	stored      uint16
	signed      bool
	photometric string
	order       binary.ByteOrder
	rescale     Rescale
	values      func(i int) int64
	windows     []WindowLevel
	wantIndex   bool
}

func windowLevelsFrame(t testing.TB, tc windowLevelsCase, rows, cols int) *Frame {
	t.Helper()
	order := tc.order
	if order == nil {
		order = binary.LittleEndian
	}
	width := int(tc.bits / 8)
	data := make([]byte, rows*cols*width)
	mask := uint64(1)<<tc.stored - 1
	for i := 0; i < rows*cols; i++ {
		raw := uint64(tc.values(i)) & mask
		switch tc.bits {
		case 8:
			data[i] = byte(raw)
		case 16:
			order.PutUint16(data[i*2:], uint16(raw))
		case 32:
			order.PutUint32(data[i*4:], uint32(raw))
		}
	}
	representation := uint16(0)
	if tc.signed {
		representation = 1
	}
	return &Frame{
		Metadata: pixeldata.Metadata{
			Rows:                      uint16(rows),
			Columns:                   uint16(cols),
			SamplesPerPixel:           1,
			BitsAllocated:             tc.bits,
			BitsStored:                tc.stored,
			HighBit:                   tc.stored - 1,
			PixelRepresentation:       representation,
			PhotometricInterpretation: tc.photometric,
		},
		ByteOrder:     order,
		PixelBytes:    data,
		Rescale:       tc.rescale,
		DefaultWindow: WindowLevel{Center: 100, Width: 400},
	}
}

func windowLevelsVOILUT(t testing.TB) *display.LUT {
	t.Helper()
	entries := make([]uint16, 512)
	for i := range entries {
		entries[i] = uint16((i * i) % 4096)
	}
	lut, err := display.NewLUT([]int{len(entries), -200, 12}, entries)
	if err != nil {
		t.Fatal(err)
	}
	return lut
}

func windowLevelsCases(t testing.TB) []windowLevelsCase {
	linearDrag := []WindowLevel{
		{Center: 40, Width: 400},
		{Center: 41, Width: 401},
		{Center: -600, Width: 1500},
		{Center: 0.5, Width: 1},
		{Center: 1000, Width: 1},
	}
	ramp12 := func(i int) int64 { return int64((i*37)%4096 - 2048) }
	return []windowLevelsCase{
		{name: "monochrome2 unsigned 12-bit", bits: 16, stored: 12, photometric: "MONOCHROME2", rescale: Rescale{Slope: 1}, values: func(i int) int64 { return int64((i * 29) % 4096) }, windows: linearDrag, wantIndex: true},
		{name: "rescaled fractional slope", bits: 16, stored: 12, photometric: "MONOCHROME2", rescale: Rescale{Slope: 0.37, Intercept: -1024.25}, values: func(i int) int64 { return int64((i * 29) % 4096) }, windows: linearDrag, wantIndex: true},
		{name: "signed with rescale", bits: 16, stored: 12, signed: true, photometric: "MONOCHROME2", rescale: Rescale{Slope: 1, Intercept: -1024}, values: ramp12, windows: linearDrag, wantIndex: true},
		{name: "signed 16-bit big endian", bits: 16, stored: 16, signed: true, photometric: "MONOCHROME2", order: binary.BigEndian, rescale: Rescale{Slope: 2, Intercept: 7}, values: func(i int) int64 { return int64((i*101)%3000 - 1500) }, windows: linearDrag, wantIndex: true},
		{name: "monochrome1", bits: 16, stored: 12, signed: true, photometric: "MONOCHROME1", rescale: Rescale{Slope: 1}, values: ramp12, windows: linearDrag, wantIndex: true},
		{name: "8-bit", bits: 8, stored: 8, photometric: "MONOCHROME2", rescale: Rescale{Slope: 1}, values: func(i int) int64 { return int64(i % 256) }, windows: []WindowLevel{{Center: 128, Width: 256}, {Center: 30, Width: 10}}, wantIndex: true},
		{name: "voi functions", bits: 16, stored: 12, signed: true, photometric: "MONOCHROME2", rescale: Rescale{Slope: 1}, values: ramp12, windows: []WindowLevel{
			{Center: 40, Width: 400, Function: display.VOILinearExact},
			{Center: -10.5, Width: 0.5, Function: display.VOILinearExact},
			{Center: 40, Width: 400, Function: display.VOISigmoid},
			{Center: 300, Width: 2000, Function: display.VOISigmoid},
		}, wantIndex: true},
		{name: "voi lut", bits: 16, stored: 12, signed: true, photometric: "MONOCHROME1", rescale: Rescale{Slope: 1}, values: ramp12, windows: []WindowLevel{{LUT: windowLevelsVOILUT(t)}}, wantIndex: true},
		{name: "stored range wider than frame", bits: 16, stored: 16, photometric: "MONOCHROME2", rescale: Rescale{Slope: 1}, values: func(i int) int64 { return int64(i * 977 % 65536) }, windows: linearDrag, wantIndex: false},
		{name: "32-bit container", bits: 32, stored: 32, photometric: "MONOCHROME2", rescale: Rescale{Slope: 1}, values: func(i int) int64 { return int64(i % 100) }, windows: linearDrag, wantIndex: true},
	}
}

// referenceWindowedGray is the per-pixel VOI map RenderCache used before the
// level table (#1222): the golden every table-driven window must equal.
func referenceWindowedGray(t testing.TB, frame *Frame, window WindowLevel) []uint8 {
	t.Helper()
	decoded, err := decodeSliceFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	window = normalizedRenderWindow(frame, window)
	mapper := display.NewVOIByteMapper(display.VOILUT{Center: window.Center, Width: window.Width, Function: window.Function, LUT: window.LUT})
	out := make([]uint8, len(decoded.grayscale))
	for i, modality := range decoded.grayscale {
		gray := mapper.Byte(float64(modality))
		if decoded.photometric == "MONOCHROME1" {
			gray = 255 - gray
		}
		out[i] = gray
	}
	return out
}

func TestRenderCacheWindowLevelsMatchPerPixelGolden(t *testing.T) {
	const rows, cols = 64, 96
	for _, tc := range windowLevelsCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			frame := windowLevelsFrame(t, tc, rows, cols)
			cache := NewRenderCache(1 << 24)
			// The default window is the first render, as when a slice is
			// scrolled into view; the drag windows follow.
			windows := append([]WindowLevel{frame.DefaultWindow}, tc.windows...)
			for _, window := range windows {
				img, err := cache.RenderFrame(frame, window)
				if err != nil {
					t.Fatal(err)
				}
				gray, ok := img.(*image.Gray)
				if !ok {
					t.Fatalf("image = %T, want *image.Gray", img)
				}
				if want := referenceWindowedGray(t, frame, window); !bytes.Equal(gray.Pix, want) {
					t.Fatalf("window %+v differs from the per-pixel golden", window)
				}
			}
			decoded, err := cache.decoded(frame)
			if err != nil {
				t.Fatal(err)
			}
			if got := decoded.hasLevelIndex(); got != tc.wantIndex {
				t.Fatalf("level index = %v, want %v", got, tc.wantIndex)
			}
			if decoded.hasLevelIndex() {
				// The cached compact frame (#1612) keeps no float32 plane;
				// the uncached decode is the golden.
				reference, err := decodeSliceFrame(frame)
				if err != nil {
					t.Fatal(err)
				}
				for i, level := range decoded.levelIndex {
					if math.Float32bits(decoded.levels[level]) != math.Float32bits(reference.grayscale[i]) {
						t.Fatalf("pixel %d: level %v != modality %v", i, decoded.levels[level], reference.grayscale[i])
					}
				}
			}
		})
	}
}

func TestRenderCacheGray16AndRemapMatchPerPixelGolden(t *testing.T) {
	const rows, cols = 48, 80
	remap := func(value float64) float64 {
		if value > 1500 {
			return math.NaN()
		}
		return value*0.75 + 12
	}
	for _, tc := range windowLevelsCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			frame := windowLevelsFrame(t, tc, rows, cols)
			cache := NewRenderCache(1 << 24)
			warmWindowDrag(t, cache, frame)
			reference, err := decodeSliceFrame(frame)
			if err != nil {
				t.Fatal(err)
			}
			for _, window := range tc.windows {
				got16, err := cache.RenderFrameGray16(frame, window)
				if err != nil {
					t.Fatal(err)
				}
				// The uncached decode carries no level index, so it takes the
				// per-pixel path and is the golden.
				want16, err := renderDecodedFrameGray16(reference, normalizedRenderWindow(frame, window))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got16.Pix, want16.Pix) {
					t.Fatalf("Gray16 window %+v differs from the per-pixel golden", window)
				}
				gotRemap, err := cache.RenderFrameRemapped(frame, window, remap)
				if err != nil {
					t.Fatal(err)
				}
				var uncached *RenderCache
				wantRemap, err := uncached.RenderFrameRemapped(frame, window, remap)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotRemap.Pix, wantRemap.Pix) {
					t.Fatalf("remapped window %+v differs from the per-pixel golden", window)
				}
			}
		})
	}
}

// warmWindowDrag renders two windows, the second of which builds the level
// index of frame's cached decoded plane.
func warmWindowDrag(t testing.TB, cache *RenderCache, frame *Frame) {
	t.Helper()
	for _, window := range []WindowLevel{frame.DefaultWindow, {Center: frame.DefaultWindow.Center + 1, Width: frame.DefaultWindow.Width}} {
		if _, err := cache.RenderFrame(frame, window); err != nil {
			t.Fatal(err)
		}
	}
}

// An 8- or 16-bit frame enters the cache compact (#1612): 2 bytes per pixel
// plus its level table, from the first window on and without a float32
// plane. A frame decoded to a float32 plane, such as a 32-bit container,
// still builds its index only once the window changes, so scrolling it never
// pays for the index.
func TestRenderCacheDecodesCompactFramesOnce(t *testing.T) {
	cases := windowLevelsCases(t)
	compact := cases[2]
	frame := windowLevelsFrame(t, compact, 64, 64)
	cache := NewRenderCache(1 << 24)
	if _, err := cache.RenderFrame(frame, frame.DefaultWindow); err != nil {
		t.Fatal(err)
	}
	decoded, err := cache.decoded(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.hasLevelIndex() || decoded.grayscale != nil {
		t.Fatalf("first decode index=%v float plane=%v, want a compact frame", decoded.hasLevelIndex(), decoded.grayscale != nil)
	}
	want := int64(64*64*2 + len(decoded.levels)*4)
	if got := cache.Stats().DecodedBytes; got != want {
		t.Fatalf("decoded bytes = %d, want %d", got, want)
	}
	warmWindowDrag(t, cache, frame)
	if again, err := cache.decoded(frame); err != nil || again != decoded {
		t.Fatalf("window drag replaced the compact frame (err %v)", err)
	}
	if got := cache.Stats().DecodedBytes; got != want {
		t.Fatalf("decoded bytes after the drag = %d, want %d", got, want)
	}

	var wide windowLevelsCase
	for _, tc := range cases {
		if tc.bits == 32 {
			wide = tc
		}
	}
	frame = windowLevelsFrame(t, wide, 64, 64)
	cache = NewRenderCache(1 << 24)
	for range 2 {
		if _, err := cache.RenderFrame(frame, frame.DefaultWindow); err != nil {
			t.Fatal(err)
		}
	}
	if decoded, err = cache.decoded(frame); err != nil || decoded.hasLevelIndex() {
		t.Fatal("a float32 plane rendered at one window built a level index; scrolling must not pay for it")
	}
	if got, want := cache.Stats().DecodedBytes, int64(64*64*4); got != want {
		t.Fatalf("decoded bytes before the drag = %d, want %d", got, want)
	}
	warmWindowDrag(t, cache, frame)
	if decoded, err = cache.decoded(frame); err != nil || !decoded.hasLevelIndex() {
		t.Fatal("second window did not build the level index of a float32 plane")
	}
}

// Concurrent requests for one frame wait for a single decode (#1612): a
// render, a read-ahead and a volume build asking at once decode it once.
func TestRenderCacheDecodesConcurrentRequestsOnce(t *testing.T) {
	frame := windowLevelsFrame(t, windowLevelsCases(t)[2], 64, 64)
	cache := NewRenderCache(1 << 24)
	var decodes atomic.Int32
	release := make(chan struct{})
	decode := cache.decodeFrame
	cache.decodeFrame = func(slice *Frame) (*decodedFrame, error) {
		decodes.Add(1)
		<-release
		return decode(slice)
	}
	var wg sync.WaitGroup
	results := make([]*decodedFrame, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decoded, err := cache.decoded(frame)
			if err != nil {
				t.Error(err)
			}
			results[i] = decoded
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for decodes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := decodes.Load(); got != 1 {
		t.Fatalf("decoded %d times, want once", got)
	}
	for i, decoded := range results {
		if decoded == nil || decoded != results[0] {
			t.Fatalf("request %d got another frame", i)
		}
	}
}
