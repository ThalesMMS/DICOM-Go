package display

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Exercise both the table and per-pixel paths against PS3.3 C.11.2.1.2–3.
// The expected values start from the integers the fixture encodes. Shifted
// samples exercise the documented legacy High Bit reader.
func TestVOIStoredDomainOracle(t *testing.T) {
	profiles := []struct {
		name         string
		format       PixelFormat
		slope, shift float64
	}{
		{"full-u8", PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, 1, -100},
		{"full-s8", PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7, Signed: true}, -2, 1},
		{"shifted-u5-in-u8", PixelFormat{BitsAllocated: 8, BitsStored: 5, HighBit: 6}, 8, -117},
		{"shifted-s6-in-u8", PixelFormat{BitsAllocated: 8, BitsStored: 6, HighBit: 7, Signed: true}, 3, .25},
		{"shifted-u10-in-le16", PixelFormat{BitsAllocated: 16, BitsStored: 10, HighBit: 12, ByteOrder: binary.LittleEndian}, .25, -120},
		{"shifted-s12-in-be16", PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 14, Signed: true, ByteOrder: binary.BigEndian}, -.125, 3},
		{"full-u16-be", PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, ByteOrder: binary.BigEndian}, .004, -100},
		{"full-s16-le", PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, Signed: true, ByteOrder: binary.LittleEndian}, .004, 0},
	}
	windows := []struct {
		name     string
		voi      VOILUT
		response func(float64) float64
	}{
		{"linear", VOILUT{Center: .5, Width: 257}, func(v float64) float64 { return (v + 128) / 256 }},
		{"threshold", VOILUT{Center: .75, Width: 1}, func(v float64) float64 {
			if v > .25 {
				return 1
			}
			return 0
		}},
		{"linear-exact", VOILUT{Center: 8, Width: 192, Function: VOILinearExact}, func(v float64) float64 { return (v + 88) / 192 }},
		{"sigmoid", VOILUT{Center: -4, Width: 72, Function: VOISigmoid}, func(v float64) float64 {
			return 1 / (1 + math.Exp(-(v+4)/18))
		}},
	}
	for _, profile := range profiles {
		for _, window := range windows {
			t.Run(profile.name+"/"+window.name, func(t *testing.T) {
				format := profile.format
				count := 1 << format.BitsStored
				lowest := 0
				if format.Signed {
					lowest = -count / 2
				}
				alignment := format.HighBit - format.BitsStored + 1
				noise := uint16(0xa55a & ^((count - 1) << alignment))
				write := func(raw []byte, stored int) []byte {
					word := uint16((stored&(count-1))<<alignment) | noise
					if format.BitsAllocated == 8 {
						return append(raw, byte(word))
					}
					var encoded [2]byte
					format.ByteOrder.PutUint16(encoded[:], word)
					return append(raw, encoded[:]...)
				}
				check := func(stored []int, columns int) {
					t.Helper()
					raw := make([]byte, 0, len(stored)*format.BitsAllocated/8)
					for _, value := range stored {
						raw = write(raw, value)
					}
					original := slices.Clone(raw)
					input := Frame{
						Rows: len(stored) / columns, Columns: columns, Pixels: raw, Format: format,
						Modality: ModalityLUT{Slope: profile.slope, Intercept: profile.shift}, VOI: window.voi,
					}
					gray, err := RenderGray(input)
					if err != nil {
						t.Fatal(err)
					}
					gray16, err := RenderGray16(input)
					if err != nil {
						t.Fatal(err)
					}
					for i, value := range stored {
						modality := float64(value)*profile.slope + profile.shift
						unit := math.Max(0, math.Min(1, window.response(modality)))
						want8, want16 := byte(math.Round(unit*255)), uint16(math.Round(unit*65535))
						if got8, got16 := gray.Pix[i], gray16.Gray16At(i%columns, i/columns).Y; got8 != want8 || got16 != want16 {
							t.Fatalf("stored %d, modality %g: output (%d,%d), want (%d,%d)", value, modality, got8, got16, want8, want16)
						}
					}
					if !bytes.Equal(raw, original) {
						t.Fatal("render changed stored pixels")
					}
				}
				domain := make([]int, count)
				for i := range domain {
					domain[i] = lowest + i
				}
				check(domain, min(count, 256))
				check([]int{domain[0], domain[count/4], domain[count/2-1], domain[count/2], domain[count-2], domain[count-1]}, 3)
			})
		}
	}
}

func TestVOIAlternativesPreservePositionsAndValidSiblings(t *testing.T) {
	goodItem := ds(
		core.NewRawElement(tagLUTDescriptor, core.VRUS, []byte{3, 0, 5, 0, 8, 0}),
		core.NewRawElement(tagLUTData, core.VROW, []byte{30, 90, 210, 0}),
		str(tagLUTExplanation, core.VRLO, "  Detail  "),
	)
	otherItem := ds(
		core.NewRawElement(tagLUTDescriptor, core.VRUS, []byte{2, 0, 5, 0, 8, 0}),
		core.NewRawElement(tagLUTData, core.VROW, []byte{60, 190}),
		str(tagLUTExplanation, core.VRLO, "Survey"),
	)
	badItem := ds(core.NewRawElement(tagLUTDescriptor, core.VRUS, []byte{3, 0}))
	expectedBytes := map[string][]byte{"Detail": {30, 30, 90, 210, 210}, "Survey": {60, 60, 190, 190, 190}}
	cases := []struct {
		name, function string
		wantFunction   VOIFunction
		wantWindows    []VOILUT
		items          []core.DataSet
		wantLUTNames   []string
		wantError      error
	}{
		{"linear-skips-subunit-at-its-own-position", "LINEAR", VOILinear,
			[]VOILUT{{Center: 24, Width: 1, Explanation: "Threshold"}, {Center: 44, Width: 6}}, nil, nil, nil},
		{"exact-keeps-positive-subunit", "LINEAR_EXACT", VOILinearExact,
			[]VOILUT{{Center: 14, Width: .5, Explanation: "Narrow"}, {Center: 24, Width: 1, Explanation: "Threshold"}, {Center: 44, Width: 6}}, nil, nil, nil},
		{"sigmoid-keeps-positive-subunit", "SIGMOID", VOISigmoid,
			[]VOILUT{{Center: 14, Width: .5, Explanation: "Narrow"}, {Center: 24, Width: 1, Explanation: "Threshold"}, {Center: 44, Width: 6}}, nil, nil, nil},
		{"malformed-first-LUT", "LINEAR", VOILinear,
			[]VOILUT{{Center: 24, Width: 1, Explanation: "Threshold"}, {Center: 44, Width: 6}}, []core.DataSet{badItem, goodItem}, []string{"Detail"}, ErrInvalidLUTDescriptor},
		{"malformed-middle-LUT", "LINEAR", VOILinear,
			[]VOILUT{{Center: 24, Width: 1, Explanation: "Threshold"}, {Center: 44, Width: 6}}, []core.DataSet{goodItem, badItem, otherItem}, []string{"Detail", "Survey"}, ErrInvalidLUTDescriptor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attrs := []core.Element{
				str(tagWindowCenter, core.VRDS, "14", "24", "34", "44", "54"),
				str(tagWindowWidth, core.VRDS, ".5", "1", "0", "6"),
				str(tagWindowCenterWidthExplanation, core.VRLO, " Narrow ", "Threshold", "Disabled"),
				str(tagVOILUTFunction, core.VRCS, tc.function),
			}
			if tc.items != nil {
				attrs = append(attrs, seq(tagVOILUTSequence, tc.items...))
			}
			obj := object.FromElements([]core.Element{seq(tagPerFrame, ds(seq(tagFrameVOILUTSequence, ds(attrs...))))}, nil)
			got, err := VOIsAt(obj, 0)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("VOIsAt error = %v, want %v", err, tc.wantError)
			}
			if len(got) != len(tc.wantWindows)+len(tc.wantLUTNames) {
				t.Fatalf("alternatives = %+v", got)
			}
			for i, want := range tc.wantWindows {
				want.Function = tc.wantFunction
				if got[i] != want {
					t.Fatalf("window %d = %+v, want %+v", i, got[i], want)
				}
			}
			for i, name := range tc.wantLUTNames {
				alt := got[len(tc.wantWindows)+i]
				if alt.LUT == nil || alt.Explanation != name || alt.Function != tc.wantFunction {
					t.Fatalf("LUT alternative %d = %+v, want %s", i, alt, name)
				}
				img, err := RenderGray(Frame{
					Rows: 1, Columns: 5, Pixels: []byte{0, 5, 6, 7, 255},
					Format: PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, VOI: alt,
				})
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(img.Pix, expectedBytes[name]) {
					t.Fatalf("%s mapped %v, want %v", name, img.Pix, expectedBytes[name])
				}
			}
		})
	}
}

func TestModalityAndVOILUTCompositionPreservesOutputDepth(t *testing.T) {
	modality := mustContractLUT(t, []int{4, -2, 16}, []uint16{26, 22, 23, 24})
	voi := mustContractLUT(t, []int{4, 22, 12}, []uint16{100, 101, 3500, 4095})
	for _, tc := range []struct {
		name   string
		invert bool
	}{
		{"normal", false}, {"inverse", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := Frame{
				Rows: 2, Columns: 3, Pixels: []byte{0xf3, 0xfc, 0xfd, 0xf0, 0xf1, 0xff},
				Format:   PixelFormat{BitsAllocated: 8, BitsStored: 3, HighBit: 2, Signed: true},
				Modality: ModalityLUT{Slope: 30, Intercept: -200, LUT: modality},
				VOI:      VOILUT{Center: 500, Width: 1, LUT: voi},
			}
			gray, err := RenderGray(frame)
			if err != nil {
				t.Fatal(err)
			}
			gray16, err := RenderGray16(frame)
			if err != nil {
				t.Fatal(err)
			}
			presentation := Presentation{Invert: tc.invert}
			presentation.Apply(gray)
			presentation.ApplyGray16(gray16)
			for i, entry := range []int{3500, 4095, 4095, 101, 3500, 100} {
				want8 := byte((entry*255 + 2047) / 4095)
				want16 := uint16((entry*65535 + 2047) / 4095)
				if tc.invert {
					want8, want16 = 255-want8, 65535-want16
				}
				if got8, got16 := gray.Pix[i], gray16.Gray16At(i%3, i/3).Y; got8 != want8 || got16 != want16 {
					t.Fatalf("pixel %d from VOI entry %d = (%d,%d), want (%d,%d)", i, entry, got8, got16, want8, want16)
				}
			}
		})
	}
}

func mustContractLUT(t *testing.T, descriptor []int, entries []uint16) *LUT {
	t.Helper()
	lut, err := NewLUT(descriptor, entries)
	if err != nil {
		t.Fatal(err)
	}
	return lut
}
