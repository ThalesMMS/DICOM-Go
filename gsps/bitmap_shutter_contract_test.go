package gsps

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// A set bit replaces the corresponding source pixel with the shutter
// presentation value. A clear bit preserves the source's presentation.
func TestReferenceGSPSBitmapShutterMasksSetBits(t *testing.T) {
	state := renderState()
	state.DisplayedArea = DisplayedArea{Defined: true, TopLeftX: 1, TopLeftY: 1, BottomRightX: 3, BottomRightY: 1}
	state.ReferencedImages = []ReferencedImage{{
		SeriesInstanceUID: "2.25.66.20", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2",
		SOPInstanceUID: "2.25.66.21",
	}}
	const hidden = 52
	shutter := &display.Shutter{
		PresentationValue: hidden,
		Bitmap: &display.BitmapShutter{Group: 0x6000, Overlay: display.Overlay{
			Rows: 1, Columns: 3, OriginRow: 1, OriginCol: 1, Data: []byte{0b010, 0},
		}},
	}
	frame := display.Frame{
		Rows: 1, Columns: 3, Pixels: []byte{20, 40, 60},
		Format: display.PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7},
	}
	before := bytes.Clone(frame.Pixels)
	target := ImageState{SOPInstanceUID: state.ReferencedImages[0].SOPInstanceUID, FrameNumber: 1}
	for _, tc := range []struct {
		name    string
		shutter *display.Shutter
		want    []byte
	}{
		{"no shutter", nil, []byte{20, 40, 60}},
		{"bitmap", shutter, []byte{20, hidden, 60}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state.Shutter = tc.shutter
			wire := shutterContractWire(t, state, nil)
			read, err := Read(wire.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Render(frame, read, target, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Pix, tc.want) {
				t.Errorf("rendered pixels = %v, want %v", got.Pix, tc.want)
			}
			if !bytes.Equal(frame.Pixels, before) {
				t.Errorf("stored source pixels changed from %v to %v", before, frame.Pixels)
			}
		})
	}
}

// Two overlay planes apply to different frames. Render their full raster
// through a rectangular shutter, including a transformed presentation.
func TestReferenceGSPSFrameOverlayComposition(t *testing.T) {
	const hidden = 52
	var overlayValue, frameValue uint16 = 176 << 8, 144 << 8
	ref := ReferencedImage{
		SeriesInstanceUID: "2.25.66.10", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2.1",
		SOPInstanceUID: "2.25.66.11", Frames: []int{1, 2},
	}
	frame := display.Frame{
		Rows: 3, Columns: 5,
		Pixels: []byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150},
		Format: display.PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7},
	}
	firstOverlay := display.Overlay{Rows: 3, Columns: 5, OriginRow: 1, OriginCol: 1, Group: 0x6002, Frames: 1, FrameOrigin: 1, Data: []byte{0x22, 0}}
	laterOverlay := firstOverlay
	laterOverlay.Group, laterOverlay.Frames, laterOverlay.FrameOrigin = 0x6004, 1, 2
	laterOverlay.Data = []byte{0x80, 0x40}
	inactive := firstOverlay
	inactive.Group = 0x6006
	inactive.Data = []byte{0xFF, 0x7F}
	opts := RenderOptions{WholeImage: true, ImageOverlays: []display.Overlay{firstOverlay, laterOverlay, inactive}}
	before := bytes.Clone(frame.Pixels)
	state := renderState()
	state.ReferencedImages = []ReferencedImage{ref}
	state.Shutter = &display.Shutter{
		PresentationValue: hidden,
		Rectangle:         &display.RectShutter{Left: 2, Right: 5, Upper: 1, Lower: 2},
	}
	state.GraphicLayers = []GraphicLayer{
		{Name: "FIRST", Order: 1, RecommendedGrayscale: &overlayValue},
		{Name: "FRAME", Order: 2, RecommendedGrayscale: &frameValue},
	}
	state.OverlayActivations = []OverlayActivation{{Group: 0x6002, Layer: "FIRST"}, {Group: 0x6004, Layer: "FRAME"}}
	for _, tc := range []struct {
		name      string
		transform SpatialTransform
		width     int
		want      func(byte, byte) []byte
	}{
		{"identity", SpatialTransform{}, 5, func(first, later byte) []byte {
			return []byte{hidden, first, 30, 40, 50, hidden, 70, later, 90, 100, hidden, hidden, hidden, hidden, hidden}
		}},
		{"rotate 90", SpatialTransform{RotationDegrees: 90}, 3, func(first, later byte) []byte {
			return []byte{hidden, hidden, hidden, hidden, 70, first, hidden, later, 30, hidden, 90, 40, hidden, 100, 50}
		}},
		{"rotate 90 and flip", SpatialTransform{RotationDegrees: 90, FlipHorizontal: true}, 3, func(first, later byte) []byte {
			return []byte{hidden, hidden, hidden, first, 70, hidden, 30, later, hidden, 40, 90, hidden, 50, 100, hidden}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state.SpatialTransform = tc.transform
			wire := shutterContractWire(t, state, nil)
			read, err := Read(wire.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			target := ImageState{SOPInstanceUID: ref.SOPInstanceUID}
			for i, values := range [][2]byte{{176, 80}, {20, 144}} {
				target.FrameNumber = i + 1
				out, err := Render(frame, read, target, opts)
				want := tc.want(values[0], values[1])
				if err != nil || out == nil || out.Rect.Dx() != tc.width || out.Rect.Dy() != len(want)/tc.width || !bytes.Equal(out.Pix, want) {
					t.Fatalf("frame %d render = %+v, %v; want raster %v", target.FrameNumber, out, err, want)
				}
			}
			target.FrameNumber = 3
			if out, err := Render(frame, read, target, opts); out != nil || !errors.Is(err, ErrMissingReference) {
				t.Fatalf("rendered a nonreferenced frame: %+v, %v", out, err)
			}
			if !bytes.Equal(frame.Pixels, before) {
				t.Fatalf("render changed the stored source pixels: %v", frame.Pixels)
			}
		})
	}
}

func TestReferenceGSPSBitmapShutterRejectsUnresolvedWireGroup(t *testing.T) {
	state := renderState()
	state.Shutter = &display.Shutter{Bitmap: &display.BitmapShutter{
		Group: 0x6000, Overlay: display.Overlay{Rows: 3, Columns: 5, OriginRow: 1, OriginCol: 1, Data: []byte{1, 0}},
	}}
	for _, tc := range []struct {
		name   string
		mutate func(*object.Object)
	}{
		{"odd group", func(obj *object.Object) { obj.Put(derivedio.US(tagShutterOverlayGroup, 0x6001)) }},
		{"group below range", func(obj *object.Object) { obj.Put(derivedio.US(tagShutterOverlayGroup, 0x5FFE)) }},
		{"group above range", func(obj *object.Object) { obj.Put(derivedio.US(tagShutterOverlayGroup, 0x6020)) }},
		{"unresolved group", func(obj *object.Object) { obj.Put(derivedio.US(tagShutterOverlayGroup, 0x601E)) }},
		{"missing group", func(obj *object.Object) { obj.Remove(tagShutterOverlayGroup) }},
		{"missing mask data", func(obj *object.Object) { obj.Remove(core.NewTag(0x6000, 0x3000)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := shutterContractWire(t, state, tc.mutate)
			if got, err := Read(wire.Dataset); got != nil || !errors.Is(err, display.ErrInvalidShutter) {
				t.Fatalf("Read published a state with an unresolved shutter: %+v, %v", got, err)
			}
		})
	}
}

func shutterContractWire(t *testing.T, state *State, mutate func(*object.Object)) *object.File {
	t.Helper()
	file, err := Write(state)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(file.Dataset)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parsed.Close() })
	return parsed
}
