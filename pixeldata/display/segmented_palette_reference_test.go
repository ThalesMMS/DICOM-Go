package display

import (
	"bytes"
	"errors"
	"image/color"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestSegmentedPaletteWireExpansion(t *testing.T) {
	for _, wire := range segmentedPaletteWireSyntaxes() {
		t.Run(wire.name, func(t *testing.T) {
			t.Run("linear-and-reused-endpoint", func(t *testing.T) {
				const firstMapped = -2
				red := []uint16{0, 1, 9, 1, 3, 30, 0, 1, 90, 2, 1, 6, 0}
				// The copied linear segment starts at the end of the caller's
				// segment (90), so it traces a descending line to 30.
				want := []uint16{9, 16, 23, 30, 90, 70, 50, 30}
				green := []uint16{0, 1, 255, 1, 7, 10}
				wantGreen := []uint16{255, 220, 185, 150, 115, 80, 45, 10}
				blue := []uint16{0, 8, 27, 19, 13, 22, 45, 58, 75, 61}
				palette, err := PaletteFromObject(segmentedPaletteWireObject(t, wire.syntax, 8, firstMapped, 8, red, green, blue))
				if err != nil || palette == nil {
					t.Fatalf("read palette: %v", err)
				}
				for i, entry := range []struct {
					channel *LUT
					want    []uint16
				}{{palette.Red, want}, {palette.Green, wantGreen}, {palette.Blue, blue[2:]}} {
					if entry.channel.FirstMapped != firstMapped || entry.channel.BitsPerEntry != 8 || len(entry.channel.Adjustments) != 0 || !slices.Equal(entry.channel.Entries, entry.want) {
						t.Fatalf("channel %d = %+v, want %v starting at %d", i, entry.channel, entry.want, firstMapped)
					}
				}
				frame := ColorFrame{
					Rows: 1, Columns: 10, Pixels: []byte{253, 254, 255, 0, 1, 2, 3, 4, 5, 6},
					Photometric: "PALETTE COLOR", SamplesPerPixel: 1, Palette: palette,
					Format: PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7, Signed: true},
				}
				im, err := RenderColor(frame)
				if err != nil {
					t.Fatal(err)
				}
				for i, index := range []int{0, 0, 1, 2, 3, 4, 5, 6, 7, 7} {
					wantPixel := color.RGBA{R: uint8(want[index]), G: uint8(wantGreen[index]), B: uint8(blue[index+2]), A: 255}
					if got := im.RGBAAt(i, 0); got != wantPixel {
						t.Fatalf("pixel %d = %v, want %v", i, got, wantPixel)
					}
				}
			})
			t.Run("indirect-offset-with-upper-word", func(t *testing.T) {
				// The second segment starts at byte 0x10002. The pointer
				// stores the low word before the high word in both syntaxes.
				prefix := make([]uint16, 32767)
				for i := range prefix {
					prefix[i] = uint16(i)
				}
				words := append([]uint16{0, uint16(len(prefix))}, prefix...)
				words = append(words, 0, 2, 42009, 57263, 2, 1, 2, 1)
				want := append(prefix, 42009, 57263, 42009, 57263)
				palette, err := PaletteFromObject(segmentedPaletteWireObject(t, wire.syntax, uint16(len(want)), 0, 16, words, words, words))
				if err != nil || palette == nil {
					t.Fatalf("read palette: %v", err)
				}
				for i, channel := range []*LUT{palette.Red, palette.Green, palette.Blue} {
					if !slices.Equal(channel.Entries, want) {
						t.Fatalf("channel %d did not expand to prefix followed by duplicated segment; got %d entries", i, len(channel.Entries))
					}
				}
			})
		})
	}
}

func TestSegmentedPaletteWireRejectsMalformedChannel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		words []uint16
		want  error
	}{
		{"missing-length", []uint16{0}, ErrInvalidLUTData},
		{"discrete-payload-truncated", []uint16{0, 3, 18, 21}, ErrInvalidLUTData},
		{"linear-end-missing", []uint16{0, 1, 18, 1, 2}, ErrInvalidLUTData},
		{"linear-first", []uint16{1, 3, 255}, ErrInvalidLUTData},
		{"unknown-opcode", []uint16{7, 1, 3}, ErrInvalidLUTData},
		{"too-few-entries", []uint16{0, 2, 18, 42}, ErrInvalidLUTData},
		{"excess-after-exact-count", []uint16{0, 3, 18, 42, 56, 0, 1, 70}, ErrInvalidLUTData},
		{"indirect-odd-offset", []uint16{0, 1, 18, 2, 2, 3, 0}, ErrInvalidLUTData},
		{"indirect-end-offset", []uint16{0, 1, 18, 2, 2, 14, 0}, ErrInvalidLUTData},
		{"indirect-truncated-offset", []uint16{0, 1, 18, 2, 2, 0}, ErrInvalidLUTData},
		{"indirect-self-reference", []uint16{0, 1, 18, 2, 2, 6, 0}, ErrUnsupportedSegmentedPalette},
		{"indirect-into-indirect", []uint16{0, 1, 18, 2, 1, 0, 0, 2, 1, 6, 0}, ErrUnsupportedSegmentedPalette},
		{"indirect-count-crosses-into-indirect", []uint16{0, 1, 18, 2, 2, 0, 0}, ErrUnsupportedSegmentedPalette},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, wire := range segmentedPaletteWireSyntaxes() {
				t.Run(wire.name, func(t *testing.T) {
					valid := []uint16{0, 3, 58, 97, 213}
					obj := segmentedPaletteWireObject(t, wire.syntax, 3, 0, 8, valid, valid, tc.words)
					got, err := PaletteFromObject(obj)
					if got != nil || !errors.Is(err, tc.want) {
						t.Fatalf("palette=%v, err=%v, want no palette and %v", got, err, tc.want)
					}
				})
			}
		})
	}
}

func TestSegmentedPaletteWireRejectsIndirectPayloadOffset(t *testing.T) {
	for _, wire := range segmentedPaletteWireSyntaxes() {
		t.Run(wire.name, func(t *testing.T) {
			// The first segment contains a pair of values that look like an
			// opcode and length. An offset into those values can expand to
			// the declared count even though it points inside a segment.
			words := []uint16{0, 4, 0, 2, 126, 234, 0, 2, 91, 103, 2, 1, 12, 0}
			t.Run("segment-start", func(t *testing.T) {
				palette, err := PaletteFromObject(segmentedPaletteWireObject(t, wire.syntax, 8, 0, 8, words, words, words))
				if err != nil || palette == nil {
					t.Fatalf("read palette with indirect offset to second segment: %v", err)
				}
				want := []uint16{0, 2, 126, 234, 91, 103, 91, 103}
				if !slices.Equal(palette.Blue.Entries, want) {
					t.Fatalf("blue entries = %v, want %v", palette.Blue.Entries, want)
				}
			})
			t.Run("inside-payload", func(t *testing.T) {
				badBlue := slices.Clone(words)
				badBlue[12] = 4 // Byte offset to the first value of the first segment.
				palette, err := PaletteFromObject(segmentedPaletteWireObject(t, wire.syntax, 8, 0, 8, words, words, badBlue))
				if palette != nil {
					t.Fatalf("indirect offset into payload returned a palette with blue entries %v and error %v", palette.Blue.Entries, err)
				}
				if !errors.Is(err, ErrInvalidLUTData) {
					t.Fatalf("indirect offset into payload: error = %v, want %v", err, ErrInvalidLUTData)
				}
			})
		})
	}
}

type segmentedPaletteWireSyntax struct {
	name   string
	syntax transfer.Syntax
}

func segmentedPaletteWireSyntaxes() []segmentedPaletteWireSyntax {
	return []segmentedPaletteWireSyntax{{"LE", transfer.ExplicitVRLittleEndian}, {"BE", transfer.ExplicitVRBigEndian}}
}

func segmentedPaletteWireObject(t *testing.T, syntax transfer.Syntax, count uint16, firstMapped int16, depth uint16, red, green, blue []uint16) *object.Object {
	t.Helper()
	var signed uint16
	descriptorVR := core.VRUS
	if firstMapped < 0 {
		signed, descriptorVR = 1, core.VRSS
	}
	order := syntax.ByteOrder
	data := dicomtest.EncodeElements(syntax,
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, order, signed),
		dicomtest.Uint16Element(tagRedPaletteDesc, descriptorVR, order, count, uint16(firstMapped), depth),
		dicomtest.Uint16Element(tagGreenPaletteDesc, descriptorVR, order, count, uint16(firstMapped), depth),
		dicomtest.Uint16Element(tagBluePaletteDesc, descriptorVR, order, count, uint16(firstMapped), depth),
		dicomtest.Uint16Element(tagSegRedPalette, core.VROW, order, red...),
		dicomtest.Uint16Element(tagSegGreenPalette, core.VROW, order, green...),
		dicomtest.Uint16Element(tagSegBluePalette, core.VROW, order, blue...),
	)
	obj, err := object.ReadDataSet(bytes.NewReader(data), syntax)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}
