package display

import (
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestMultigroupOverlayGeometryAndFrameOrigins(t *testing.T) {
	origin := func(group uint16, row, column int16) core.Element {
		raw := binary.LittleEndian.AppendUint16(nil, uint16(row))
		raw = binary.LittleEndian.AppendUint16(raw, uint16(column))
		return core.NewRawElement(core.NewTag(group, 0x0050), core.VRSS, raw)
	}
	data := func(group uint16, packed byte) core.Element {
		return core.NewRawElement(core.NewTag(group, 0x3000), core.VROB, []byte{packed, 0})
	}
	obj := object.FromElements([]core.Element{
		ovUS(0x6000, 0x0010, 5), ovUS(0x6000, 0x0011, 5), data(0x6000, 0xff),
		// Two three-bit planes start at image frame 2. The first plane's first
		// set bit is off-image, and its second plane starts mid-byte.
		ovUS(0x6002, 0x0010, 1), ovUS(0x6002, 0x0011, 3),
		ovText(0x6002, 0x0015, core.VRIS, "2"), ovUS(0x6002, 0x0051, 2),
		origin(0x6002, 2, 0), data(0x6002, 0x15),
		ovUS(0x601e, 0x0010, 2), ovUS(0x601e, 0x0011, 2),
		ovText(0x601e, 0x0015, core.VRIS, "2"), ovUS(0x601e, 0x0051, 3),
		origin(0x601e, 1, 3), data(0x601e, 0x69),
	}, nil)
	parsed, err := PresentationFromObject(obj)
	if !errors.Is(err, ErrInvalidOverlay) {
		t.Fatalf("short 0x6000 overlay error = %v, want ErrInvalidOverlay", err)
	}
	if len(parsed.Overlays) != 2 || parsed.Overlays[0].Group != 0x6002 || parsed.Overlays[1].Group != 0x601e {
		t.Fatalf("valid overlay groups = %+v", parsed.Overlays)
	}
	for index, tc := range []struct {
		name    string
		burned  []image.Point
		origins int
	}{
		{name: "before-both"},
		{name: "clipped-first-plane", burned: []image.Point{{X: 1, Y: 1}}, origins: 1},
		{name: "overlapping-frames", burned: []image.Point{{X: 0, Y: 1}, {X: 2, Y: 0}, {X: 3, Y: 1}}, origins: 2},
		{name: "second-group-only", burned: []image.Point{{X: 3, Y: 0}, {X: 2, Y: 1}}, origins: 1},
		{name: "after-both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selected Presentation
			for _, group := range parsed.Overlays {
				if plane, ok := group.ForImageFrame(index); ok {
					selected.Overlays = append(selected.Overlays, plane)
				}
			}
			if len(selected.Overlays) != tc.origins {
				t.Fatalf("frame %d selected %d planes, want %d", index, len(selected.Overlays), tc.origins)
			}
			// Nonzero existing bounds with extra row stride detect misusing
			// the visible width or the image origin as an offset into Pix.
			bounds := image.Rect(3, 2, 7, 4)
			gray := image.NewGray(image.Rect(0, 0, 8, 5)).SubImage(bounds).(*image.Gray)
			gray16 := image.NewGray16(image.Rect(0, 0, 8, 5)).SubImage(bounds).(*image.Gray16)
			selected.Apply(gray)
			selected.ApplyGray16(gray16)
			for y := 0; y < bounds.Dy(); y++ {
				for x := 0; x < bounds.Dx(); x++ {
					var want8 uint8
					var want16 uint16
					if slices.Contains(tc.burned, image.Pt(x, y)) {
						want8, want16 = 255, 65535
					}
					ax, ay := x+bounds.Min.X, y+bounds.Min.Y
					if got8, got16 := gray.GrayAt(ax, ay).Y, gray16.Gray16At(ax, ay).Y; got8 != want8 || got16 != want16 {
						t.Fatalf("frame %d pixel (%d,%d) = (%d,%d), want (%d,%d)", index, x, y, got8, got16, want8, want16)
					}
				}
			}
		})
	}
}

func TestShutterShapesAgreeAcrossVisibilityAndOutputDepth(t *testing.T) {
	rectangle := &RectShutter{Left: -1, Right: 4, Upper: 2, Lower: 4}
	circle := &CircleShutter{CenterRow: 3, CenterCol: 4, Radius: 2}
	polygon := &PolygonShutter{Vertices: []ShutterPoint{{1, 2}, {1, 6}, {4, 6}, {4, 2}}}
	for _, tc := range []struct {
		name      string
		rectangle *RectShutter
		circle    *CircleShutter
		polygon   *PolygonShutter
		bitmap    *BitmapShutter
		visible   [5]string
	}{
		{"clipped-rectangle", rectangle, nil, nil, nil, [5]string{"000000", "111100", "111100", "111100", "000000"}},
		{"circle-boundaries", nil, circle, nil, nil, [5]string{"000100", "001110", "011111", "001110", "000100"}},
		{"polygon-boundaries", nil, nil, polygon, nil, [5]string{"011110", "011110", "011110", "000000", "000000"}},
		{"all-geometric-shapes-intersect", rectangle, circle, polygon, nil, [5]string{"000000", "001100", "011100", "000000", "000000"}},
		{"disjoint-circle", nil, &CircleShutter{CenterCol: 12, CenterRow: 3, Radius: 2}, nil, nil, [5]string{"000000", "000000", "000000", "000000", "000000"}},
		{
			"offset-bitmap", nil, nil, nil, &BitmapShutter{Group: 0x6000, Overlay: Overlay{
				Rows: 2, Columns: 3, OriginRow: 2, OriginCol: 0, Data: []byte{0b110101},
			}}, [5]string{"111111", "101111", "001111", "111111", "111111"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const source8, source16, masked8, masked16 = 179, 45111, 18, 0x1294
			shutter := &Shutter{
				Rectangle: tc.rectangle, Circle: tc.circle, Polygon: tc.polygon, Bitmap: tc.bitmap,
				PresentationValue: masked8, PresentationPValue: masked16, PresentationPValueDefined: true,
			}
			bounds := image.Rect(4, 7, 10, 12)
			source := image.NewGray(bounds)
			gray16 := image.NewGray16(bounds)
			for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
				for x := bounds.Min.X; x < bounds.Max.X; x++ {
					source.SetGray(x, y, color.Gray{Y: source8})
					gray16.SetGray16(x, y, color.Gray16{Y: source16})
				}
			}
			gray, ok := shutter.ApplyTo(source).(*image.Gray)
			if !ok || gray.Bounds() != bounds {
				t.Fatalf("ApplyTo did not preserve grayscale bounds %v", bounds)
			}
			Presentation{Shutter: shutter}.ApplyGray16(gray16)
			for y, row := range tc.visible {
				for x, mask := range row {
					visible := mask == '1'
					want8, want16 := uint8(masked8), uint16(masked16)
					if visible {
						want8, want16 = source8, source16
					}
					ax, ay := x+bounds.Min.X, y+bounds.Min.Y
					gotVisible := shutter.VisibleSourcePixel(x, y)
					got8, got16 := gray.GrayAt(ax, ay).Y, gray16.Gray16At(ax, ay).Y
					if gotVisible != visible || got8 != want8 || got16 != want16 {
						t.Fatalf("(%d,%d): visible = %v, output (%d,%d); want %v and (%d,%d)", x, y, gotVisible, got8, got16, visible, want8, want16)
					}
					if source.GrayAt(ax, ay).Y != source8 {
						t.Fatalf("ApplyTo mutated source at (%d,%d)", x, y)
					}
				}
			}
		})
	}
}

func TestPresentationLUTAndShutterPrecedeOverlayBurn(t *testing.T) {
	lut := mustContractLUT(t, []int{4, 0, 8}, []uint16{28, 63, 178, 215})
	gray := image.NewGray(image.Rect(0, 0, 4, 1))
	copy(gray.Pix, []byte{0, 1, 2, 3})
	gray16 := image.NewGray16(gray.Bounds())
	for x, input := range []uint16{0, 21845, 43690, 65535} {
		gray16.SetGray16(x, 0, color.Gray16{Y: input})
	}
	presentation := Presentation{
		LUT: lut, Invert: true,
		Shutter: &Shutter{Rectangle: &RectShutter{Left: 2, Right: 2, Upper: 1, Lower: 1},
			PresentationValue: 61},
		Overlays: []Overlay{{Rows: 1, Columns: 4, OriginCol: 1, OriginRow: 1, Data: []byte{0x09}}},
	}
	presentation.Apply(gray)
	presentation.ApplyGray16(gray16)
	for x, want := range []uint8{255, 192, 61, 255} {
		if got8, got16 := gray.GrayAt(x, 0).Y, gray16.Gray16At(x, 0).Y; got8 != want || got16 != uint16(want)*257 {
			t.Fatalf("presentation pixel %d = (%d,%d), want (%d,%d)", x, got8, got16, want, uint16(want)*257)
		}
	}
}
