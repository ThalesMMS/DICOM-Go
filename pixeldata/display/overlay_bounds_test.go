package display

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// overlayGroupElements builds a 1-bit overlay group with the given
// dimensions, origin, Number of Frames in Overlay (empty: absent) and data.
func overlayGroupElements(group, rows, cols uint16, originRow, originCol int16, frames string, data []byte) []core.Element {
	origin := binary.LittleEndian.AppendUint16(nil, uint16(originRow))
	origin = binary.LittleEndian.AppendUint16(origin, uint16(originCol))
	out := []core.Element{
		ovUS(group, tagOverlayRows, rows), ovUS(group, tagOverlayColumns, cols),
		core.NewRawElement(core.NewTag(group, tagOverlayOrigin), core.VRSS, origin),
		ovUS(group, tagOverlayBitsAlloc, 1), ovUS(group, tagOverlayBitPosition, 0),
		core.NewRawElement(core.NewTag(group, tagOverlayData), core.VROB, data),
	}
	if frames != "" {
		out = append(out, ovText(group, tagOverlayFrames, core.VRIS, frames))
	}
	return out
}

func TestOverlaysFromObjectRejectsShortOverlayData(t *testing.T) {
	cases := []struct {
		name       string
		rows, cols uint16
		frames     string
		data       []byte
		wantErr    bool
	}{
		{name: "exact single frame", rows: 4, cols: 4, data: make([]byte, 2)},
		{name: "odd plane padded to even", rows: 3, cols: 3, data: make([]byte, 2)},
		{name: "longer than required", rows: 2, cols: 2, data: make([]byte, 8)},
		{name: "exact multi-frame", rows: 4, cols: 4, frames: "3", data: make([]byte, 6)},
		{name: "one byte short", rows: 4, cols: 4, data: make([]byte, 1), wantErr: true},
		{name: "huge plane with two bytes", rows: 65535, cols: 65535, data: make([]byte, 2), wantErr: true},
		{name: "huge rows with two bytes", rows: 65535, cols: 1, data: make([]byte, 2), wantErr: true},
		{name: "frames beyond data", rows: 4, cols: 4, frames: "4", data: make([]byte, 6), wantErr: true},
		{name: "frames overflow int", rows: 65535, cols: 65535, frames: "9223372036854775807", data: make([]byte, 2), wantErr: true},
		{name: "frames overflow with small plane", rows: 2, cols: 2, frames: "4611686018427387905", data: make([]byte, 2), wantErr: true},
		{name: "zero rows", rows: 0, cols: 4, data: make([]byte, 2), wantErr: true},
		{name: "zero columns", rows: 4, cols: 0, data: make([]byte, 2), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A valid neighbour group must survive the rejection.
			elements := overlayGroupElements(0x6000, 1, 8, 1, 1, "", []byte{0xFF, 0x00})
			elements = append(elements, overlayGroupElements(0x6002, tc.rows, tc.cols, 1, 1, tc.frames, tc.data)...)
			obj := object.FromElements(elements, nil)
			overlays, err := OverlaysFromObject(obj)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidOverlay) {
					t.Fatalf("err = %v, want ErrInvalidOverlay", err)
				}
				if len(overlays) != 1 || overlays[0].Group != 0x6000 {
					t.Fatalf("valid overlays = %+v, want only group 0x6000", overlays)
				}
				return
			}
			if err != nil || len(overlays) != 2 {
				t.Fatalf("err = %v, overlays = %d", err, len(overlays))
			}
			if err := overlays[1].Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestPresentationFromObjectReportsInvalidOverlay(t *testing.T) {
	elements := overlayGroupElements(0x6000, 1, 8, 1, 1, "", []byte{0xFF, 0x00})
	elements = append(elements, overlayGroupElements(0x6002, 65535, 65535, 1, 1, "", []byte{0xFF, 0xFF})...)
	pres, err := PresentationFromObject(object.FromElements(elements, nil))
	if !errors.Is(err, ErrInvalidOverlay) {
		t.Fatalf("err = %v, want ErrInvalidOverlay", err)
	}
	if len(pres.Overlays) != 1 || pres.Overlays[0].Group != 0x6000 {
		t.Fatalf("overlays = %+v", pres.Overlays)
	}
}

func TestBitmapShutterRejectsShortOverlayData(t *testing.T) {
	elements := []core.Element{
		ovText(0x0018, 0x1600, core.VRCS, "BITMAP"),
		ovUS(0x0018, 0x1623, 0x6000),
	}
	elements = append(elements, overlayGroupElements(0x6000, 65535, 65535, 1, 1, "", []byte{0xFF, 0xFF})...)
	_, err := ShutterFromObject(object.FromElements(elements, nil))
	if !errors.Is(err, ErrInvalidShutter) || !errors.Is(err, ErrInvalidOverlay) {
		t.Fatalf("err = %v, want ErrInvalidShutter and ErrInvalidOverlay", err)
	}
}

func TestOverlayValidate(t *testing.T) {
	cases := []struct {
		name    string
		overlay Overlay
		wantErr bool
	}{
		{name: "valid", overlay: Overlay{Rows: 2, Columns: 4, OriginRow: 1, OriginCol: 1, Data: []byte{0xFF}}},
		{name: "valid negative origin", overlay: Overlay{Rows: 2, Columns: 4, OriginRow: -32768, OriginCol: 32767, Data: []byte{0xFF}}},
		{name: "valid multi-frame", overlay: Overlay{Rows: 2, Columns: 4, Frames: 2, Data: []byte{0xFF, 0x0F}}},
		{name: "valid embedded", overlay: Overlay{Rows: 2, Columns: 4, Embedded: true, BitPosition: 15, Data: []byte{0xFF}}},
		{name: "zero rows", overlay: Overlay{Columns: 4, Data: []byte{0xFF}}, wantErr: true},
		{name: "negative columns", overlay: Overlay{Rows: 2, Columns: -4, Data: []byte{0xFF}}, wantErr: true},
		{name: "rows beyond US", overlay: Overlay{Rows: 65536, Columns: 1, Data: make([]byte, 8192)}, wantErr: true},
		{name: "origin beyond SS", overlay: Overlay{Rows: 1, Columns: 8, OriginRow: 32768, Data: []byte{0xFF}}, wantErr: true},
		{name: "origin below SS", overlay: Overlay{Rows: 1, Columns: 8, OriginCol: -32769, Data: []byte{0xFF}}, wantErr: true},
		{name: "short data", overlay: Overlay{Rows: 3, Columns: 3, Data: []byte{0xFF}}, wantErr: true},
		{name: "frames beyond data", overlay: Overlay{Rows: 2, Columns: 4, Frames: 3, Data: []byte{0xFF, 0x0F}}, wantErr: true},
		{name: "frame origin beyond US", overlay: Overlay{Rows: 2, Columns: 4, FrameOrigin: 65536, Data: []byte{0xFF}}, wantErr: true},
		{name: "bit position beyond 16-bit samples", overlay: Overlay{Rows: 2, Columns: 4, Embedded: true, BitPosition: 16, Data: []byte{0xFF}}, wantErr: true},
		{name: "negative bit position", overlay: Overlay{Rows: 2, Columns: 4, Embedded: true, BitPosition: -1, Data: []byte{0xFF}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.overlay.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidOverlay) {
				t.Fatalf("Validate() = %v, want ErrInvalidOverlay", err)
			}
		})
	}
}

// compositeReference is the straightforward per-bit burn-in over the whole
// overlay plane, kept as the oracle for the clipped implementation.
func compositeReference(o Overlay, img *image.Gray, value uint8) {
	bounds := img.Bounds()
	for r := 0; r < o.Rows; r++ {
		for c := 0; c < o.Columns; c++ {
			idx := r*o.Columns + c
			if idx/8 >= len(o.Data) || o.Data[idx/8]&(1<<uint(idx%8)) == 0 {
				continue
			}
			y, x := o.OriginRow-1+r, o.OriginCol-1+c
			if y < 0 || y >= bounds.Dy() || x < 0 || x >= bounds.Dx() {
				continue
			}
			img.Pix[y*img.Stride+x] = value
		}
	}
}

func TestOverlayCompositeMatchesReferenceWhenClipped(t *testing.T) {
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i*37 + 11)
	}
	cases := []struct {
		name    string
		overlay Overlay
	}{
		{name: "inside", overlay: Overlay{Rows: 5, Columns: 7, OriginRow: 2, OriginCol: 3}},
		{name: "negative origin", overlay: Overlay{Rows: 12, Columns: 13, OriginRow: -3, OriginCol: -5}},
		{name: "origin zero", overlay: Overlay{Rows: 4, Columns: 4, OriginRow: 0, OriginCol: 0}},
		{name: "past right and bottom", overlay: Overlay{Rows: 9, Columns: 11, OriginRow: 7, OriginCol: 6}},
		{name: "larger than image", overlay: Overlay{Rows: 20, Columns: 24, OriginRow: -2, OriginCol: -1}},
		{name: "entirely left", overlay: Overlay{Rows: 4, Columns: 4, OriginRow: 1, OriginCol: -10}},
		{name: "entirely below", overlay: Overlay{Rows: 4, Columns: 4, OriginRow: 11, OriginCol: 1}},
		{name: "SS extremes", overlay: Overlay{Rows: 4, Columns: 4, OriginRow: -32768, OriginCol: 32767}},
		{name: "short data clips rows", overlay: Overlay{Rows: 40, Columns: 9, OriginRow: 1, OriginCol: 1}},
		{name: "short data mid row", overlay: Overlay{Rows: 40, Columns: 3, OriginRow: -150, OriginCol: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.overlay
			o.Data = data
			if tc.name == "short data mid row" || tc.name == "short data clips rows" {
				o.Data = data[:5]
			}
			base := image.NewGray(image.Rect(0, 0, 10, 12))
			for i := range base.Pix {
				base.Pix[i] = byte(i)
			}
			got := image.NewGray(base.Rect)
			copy(got.Pix, base.Pix)
			want := image.NewGray(base.Rect)
			copy(want.Pix, base.Pix)
			o.Composite(got, 200)
			compositeReference(o, want, 200)
			if !bytes.Equal(got.Pix, want.Pix) {
				t.Fatalf("composite differs from reference\n got %v\nwant %v", got.Pix, want.Pix)
			}
		})
	}
}

func TestOverlayCompositeSubImage(t *testing.T) {
	parent := image.NewGray(image.Rect(0, 0, 8, 8))
	sub := parent.SubImage(image.Rect(2, 3, 6, 7)).(*image.Gray)
	Overlay{Rows: 2, Columns: 2, OriginRow: 4, OriginCol: 4, Data: []byte{0x0F}}.Composite(sub, 9)
	// The overlay's last row and column land at sub (3,3), parent (5,6).
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			want := uint8(0)
			if x == 5 && y == 6 {
				want = 9
			}
			if got := parent.GrayAt(x, y).Y; got != want {
				t.Fatalf("parent (%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
}

// TestHugeOverlayWorkIsBoundedByImage burns sixteen maximal overlay planes
// (65535x65535) that carry almost no data onto a small image. Iterating the
// declared planes took seconds per overlay; clipping to the image and the
// data keeps the whole run to a few thousand bit tests.
func TestHugeOverlayWorkIsBoundedByImage(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 64, 64))
	origins := [][2]int{{1, 1}, {-32768, -32768}, {32767, 32767}, {-32768, 1}, {1, 32767}}
	start := time.Now()
	for i := 0; i < 16; i++ {
		origin := origins[i%len(origins)]
		o := Overlay{Rows: 65535, Columns: 65535, OriginRow: origin[0], OriginCol: origin[1], Data: []byte{0xFF, 0xFF}}
		o.Composite(img, 255)
		if plane, ok := o.ForImageFrame(0); ok {
			t.Fatalf("ForImageFrame returned a %dx%d plane from 2 bytes", plane.Rows, plane.Columns)
		}
		if unpacked := o.Unpacked(); unpacked != nil {
			t.Fatalf("Unpacked returned %d bytes from 2 bytes of data", len(unpacked))
		}
		b := BitmapShutter{Overlay: o}
		_ = b.masks(63, 63)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("sixteen huge overlays took %v", elapsed)
	}
	// The two data bytes cover the first 16 bits of row 1.
	for x := 0; x < 64; x++ {
		want := uint8(0)
		if x < 16 {
			want = 255
		}
		if got := img.Pix[x]; got != want {
			t.Fatalf("pixel (0,%d) = %d, want %d", x, got, want)
		}
	}
	if allocs := testing.AllocsPerRun(10, func() {
		o := Overlay{Rows: 65535, Columns: 65535, Frames: 1 << 20, Data: []byte{0xFF, 0xFF}}
		o.Frame(3)
		o.ForImageFrame(1 << 19)
	}); allocs != 0 {
		t.Fatalf("Frame on an overlay without data allocated %v times", allocs)
	}
}

func TestOverlayFrameNeedsItsData(t *testing.T) {
	// 2x4 planes, two frames present, three declared.
	o := Overlay{Rows: 2, Columns: 4, Frames: 3, Data: []byte{0x81, 0x3C}}
	first, ok := o.Frame(0)
	if !ok || !bytes.Equal(first.Data, []byte{0x81}) {
		t.Fatalf("frame 0 = %v %v", ok, first.Data)
	}
	second, ok := o.Frame(1)
	if !ok || !bytes.Equal(second.Data, []byte{0x3C}) {
		t.Fatalf("frame 1 = %v %v", ok, second.Data)
	}
	if _, ok := o.Frame(2); ok {
		t.Fatal("frame 2 has no data")
	}
	// Unaligned frames: 3x3 planes pack across byte boundaries.
	o = Overlay{Rows: 3, Columns: 3, Frames: 2, Data: []byte{0x00, 0xFE, 0x03}}
	second, ok = o.Frame(1)
	if !ok || !bytes.Equal(second.Data, []byte{0xFF, 0x01}) {
		t.Fatalf("unaligned frame 1 = %v %v", ok, second.Data)
	}
	short := Overlay{Rows: 3, Columns: 3, Data: []byte{0xFF}}
	if _, ok := short.Frame(0); ok {
		t.Fatal("frame 0 accepted with 8 of 9 bits")
	}
	if short.Unpacked() != nil {
		t.Fatal("Unpacked accepted 8 of 9 bits")
	}
}

// TestOverlayFrameMatchesBitwiseExtraction checks the byte-shifting frame
// copy against a bit-by-bit extraction for every small geometry and frame.
func TestOverlayFrameMatchesBitwiseExtraction(t *testing.T) {
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i*73 + 29)
	}
	for rows := 1; rows <= 6; rows++ {
		for cols := 1; cols <= 9; cols++ {
			plane := rows * cols
			frames := len(data) * 8 / plane
			o := Overlay{Rows: rows, Columns: cols, Frames: frames, Data: data}
			for index := 0; index < frames; index++ {
				want := make([]byte, (plane+7)/8)
				for i := 0; i < plane; i++ {
					bit := index*plane + i
					if data[bit/8]&(1<<uint(bit%8)) != 0 {
						want[i/8] |= 1 << uint(i%8)
					}
				}
				got, ok := o.Frame(index)
				if !ok || !bytes.Equal(got.Data, want) {
					t.Fatalf("%dx%d frame %d: ok=%v got %v want %v", rows, cols, index, ok, got.Data, want)
				}
			}
			if _, ok := o.Frame(frames); ok {
				t.Fatalf("%dx%d frame %d beyond %d frames accepted", rows, cols, frames, frames)
			}
		}
	}
}

func TestEmbeddedOverlaysRejectMalformedAttributes(t *testing.T) {
	native := make([]byte, 2*2*2*2) // 2x2, 2 frames, 16-bit
	base := func(extra ...core.Element) *object.Object {
		elements := []core.Element{
			ovUS(0x0028, 0x0010, 2), ovUS(0x0028, 0x0011, 2), ovUS(0x0028, 0x0100, 16),
			ovUS(0x0028, 0x0101, 12), ovUS(0x0028, 0x0102, 11),
			ovText(0x0028, 0x0008, core.VRIS, "2"),
			ovUS(0x6000, 0x0010, 2), ovUS(0x6000, 0x0011, 2), ovUS(0x6000, 0x0100, 16),
		}
		return object.FromElements(append(elements, extra...), nil)
	}
	cases := []struct {
		name    string
		obj     *object.Object
		wantErr bool
	}{
		{name: "unused high bit", obj: base(ovUS(0x6000, 0x0102, 15))},
		{name: "bit position beyond samples", obj: base(ovUS(0x6000, 0x0102, 16)), wantErr: true},
		{name: "bit position inside stored bits", obj: base(ovUS(0x6000, 0x0102, 4)), wantErr: true},
		{name: "bit position at high bit", obj: base(ovUS(0x6000, 0x0102, 11)), wantErr: true},
		{name: "missing bit position", obj: base(), wantErr: true},
		{name: "frames beyond pixel data", obj: base(ovUS(0x6000, 0x0102, 15), ovText(0x6000, 0x0015, core.VRIS, "3")), wantErr: true},
		{name: "frame origin beyond pixel data", obj: base(ovUS(0x6000, 0x0102, 15), ovText(0x6000, 0x0015, core.VRIS, "1"), ovUS(0x6000, 0x0051, 3)), wantErr: true},
		{name: "frames overflow int", obj: base(ovUS(0x6000, 0x0102, 15), ovText(0x6000, 0x0015, core.VRIS, "4611686018427387905")), wantErr: true},
		{name: "frames wrap to zero", obj: base(ovUS(0x6000, 0x0102, 15), ovText(0x6000, 0x0015, core.VRIS, "4611686018427387904")), wantErr: true},
		{name: "frames and origin overflow", obj: base(ovUS(0x6000, 0x0102, 15), ovText(0x6000, 0x0015, core.VRIS, "9223372036854775807"), ovUS(0x6000, 0x0051, 65535)), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overlays, err := EmbeddedOverlays(tc.obj, native, binary.LittleEndian)
			if !tc.wantErr {
				if err != nil || len(overlays) != 1 {
					t.Fatalf("err = %v, overlays = %d", err, len(overlays))
				}
				if err := overlays[0].Validate(); err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidOverlay) {
				t.Fatalf("err = %v, want ErrInvalidOverlay", err)
			}
		})
	}
}

// FuzzOverlayFromObject feeds arbitrary overlay attributes and data through
// extraction, frame selection and burn-in. Nothing may panic, and burn-in
// work is bounded by the image and the data.
func FuzzOverlayFromObject(f *testing.F) {
	f.Add(uint16(4), uint16(4), int16(1), int16(1), "", uint16(0), []byte{0xFF, 0x00})
	f.Add(uint16(65535), uint16(65535), int16(1), int16(1), "", uint16(0), []byte{0xFF, 0xFF})
	f.Add(uint16(65535), uint16(1), int16(-32768), int16(32767), "", uint16(0), []byte{0x01, 0x02})
	f.Add(uint16(4), uint16(4), int16(-2), int16(-2), "4", uint16(2), []byte{0xAA, 0x55, 0xAA, 0x55, 0xAA, 0x55})
	f.Add(uint16(2), uint16(2), int16(1), int16(1), "4611686018427387905", uint16(65535), []byte{0x0F, 0x00})
	f.Add(uint16(3), uint16(3), int16(0), int16(0), "9223372036854775807", uint16(1), []byte{0xFF, 0x01})
	f.Add(uint16(0), uint16(8), int16(1), int16(1), "", uint16(0), []byte{0xFF, 0x00})
	f.Fuzz(func(t *testing.T, rows, cols uint16, originRow, originCol int16, frames string, frameOrigin uint16, data []byte) {
		if len(data) == 0 {
			return
		}
		elements := overlayGroupElements(0x6000, rows, cols, originRow, originCol, frames, data)
		elements = append(elements, ovUS(0x6000, tagOverlayFrameOrigin, frameOrigin))
		overlays, err := OverlaysFromObject(object.FromElements(elements, nil))
		if err != nil && !errors.Is(err, ErrInvalidOverlay) {
			t.Fatalf("untyped error %v", err)
		}
		img := image.NewGray(image.Rect(0, 0, 16, 16))
		for _, o := range overlays {
			if err := o.Validate(); err != nil {
				t.Fatalf("extracted overlay fails Validate: %v", err)
			}
			for frame := -1; frame < 3; frame++ {
				if plane, ok := o.ForImageFrame(frame); ok {
					plane.Composite(img, 255)
					if len(plane.Data) > len(data) {
						t.Fatalf("frame %d grew to %d bytes from %d", frame, len(plane.Data), len(data))
					}
				}
			}
			o.Composite(img, 128)
		}
		// Burn-in of the raw, unvalidated attributes must stay safe too.
		raw := Overlay{Rows: int(rows), Columns: int(cols), OriginRow: int(originRow), OriginCol: int(originCol), Data: data}
		raw.Composite(img, 64)
	})
}
