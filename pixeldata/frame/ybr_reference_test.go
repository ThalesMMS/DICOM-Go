package frame

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	ybrReferenceRows   = 3
	ybrReferenceFrames = 3
)

// This exercises Part 10 reads, including the selective source used for lazy
// loading. The 27-byte 4:4:4 frames straddle OW word boundaries in Big Endian.
func TestNativeYBRPart10StorageAndSelection(t *testing.T) {
	for _, storage := range []struct {
		name   string
		syntax transfer.Syntax
		vr     core.VR
	}{
		{"LE-OB", transfer.ExplicitVRLittleEndian, core.VROB},
		{"LE-OW", transfer.ExplicitVRLittleEndian, core.VROW},
		{"Implicit-LE", transfer.ImplicitVRLittleEndian, core.VROW},
		{"BE-OB", transfer.ExplicitVRBigEndian, core.VROB},
		{"BE-OW", transfer.ExplicitVRBigEndian, core.VROW},
	} {
		t.Run(storage.name, func(t *testing.T) {
			for _, layout := range []struct {
				name, photometric string
				columns           uint16
				planar            uint16
			}{
				{"full-interleaved", "YBR_FULL", 3, 0},
				{"full-planar", "YBR_FULL", 3, 1},
				{"row-paired", "YBR_FULL_422", 4, 0},
			} {
				t.Run(layout.name, func(t *testing.T) {
					bounds := image.Rect(0, 0, int(layout.columns), ybrReferenceRows)
					var value []byte
					for i := range ybrReferenceFrames {
						value = append(value, ybrReferenceBytes(i, bounds, layout.photometric, layout.planar)...)
					}
					if len(value)%2 != 0 {
						value = append(value, 0)
					}
					if storage.vr == core.VROW && storage.syntax.UID == transfer.ExplicitVRBigEndian.UID {
						for i := 0; i < len(value); i += 2 {
							value[i], value[i+1] = value[i+1], value[i]
						}
					}
					data := ybrReferenceFile(t, storage.syntax, storage.vr, layout.columns, layout.photometric, layout.planar, value)
					file, err := object.ReadFile(bytes.NewReader(data))
					if err != nil {
						t.Fatal(err)
					}
					frames, err := ExtractFrames(file)
					if err != nil || len(frames) != ybrReferenceFrames {
						t.Fatalf("extract frames: count=%d error=%v", len(frames), err)
					}
					path := filepath.Join(t.TempDir(), "color.dcm")
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					ctx := context.Background()
					reader, err := pixeldata.OpenFrameReader(ctx, path, pixeldata.FrameReaderOptions{})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := reader.Close(); err != nil {
							t.Error(err)
						}
					})
					for _, i := range []int{2, 0, 1} {
						t.Run([]string{"first", "middle", "last"}[i], func(t *testing.T) {
							want := ybrReferenceBytes(i, bounds, layout.photometric, layout.planar)
							stored, _, err := frames[i].StoredSampleData()
							if err != nil || !bytes.Equal(stored, want) {
								t.Fatalf("stored samples: got %v, want %v, error %v", stored, want, err)
							}
							assertYBRReferenceImage(t, frames[i], i, bounds, layout.photometric)
							all, meta, err := reader.Frame(ctx, i)
							if err != nil || !bytes.Equal(all, want) || meta.NumberOfFrames != 1 || meta.PlanarConfiguration != layout.planar || meta.PhotometricInterpretation != layout.photometric {
								t.Fatalf("select frame: got %v, want %v, metadata %+v, error %v", all, want, meta, err)
							}
							assertYBRReferenceImage(t, NewNativeFrame(i, all, meta), i, bounds, layout.photometric)

							region := image.Rect(1, 1, 3, 3)
							cropped, meta, err := reader.Region(ctx, i, region)
							if layout.photometric == "YBR_FULL_422" {
								if cropped != nil || !errors.Is(err, pixeldata.ErrFrameLayout) {
									t.Fatalf("subsampled native region: pixels=%v error=%v", cropped, err)
								}
								return
							}
							want = ybrReferenceBytes(i, region, layout.photometric, layout.planar)
							if err != nil || !bytes.Equal(cropped, want) || int(meta.Rows) != region.Dy() || int(meta.Columns) != region.Dx() || meta.PlanarConfiguration != layout.planar {
								t.Fatalf("select region: got %v, want %v, metadata %+v, error %v", cropped, want, meta, err)
							}
							assertYBRReferenceImage(t, NewNativeFrame(i, cropped, meta), i, region, layout.photometric)
						})
					}
				})
			}
		})
	}
}

func ybrReferenceFile(t *testing.T, syntax transfer.Syntax, vr core.VR, columns uint16, photometric string, planar uint16, value []byte) []byte {
	t.Helper()
	us := func(element, v uint16) core.Element {
		return dicomtest.Uint16Element(core.NewTag(0x0028, element), core.VRUS, syntax.ByteOrder, v)
	}
	data, err := dicomtest.Part10File(syntax,
		us(0x0002, 3), dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, photometric), us(0x0006, planar),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, "3"),
		us(0x0010, ybrReferenceRows), us(0x0011, columns),
		us(0x0100, 8), us(0x0101, 8), us(0x0102, 7), us(0x0103, 0),
		core.NewRawElement(core.TagPixelData, vr, value),
	)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Rows, frames and components all vary to expose a mistaken pitch, chroma
// pair, plane order or frame start. The matrix includes clipped RGB samples.
func ybrReferenceSample(frame, x, y int, photometric string) [3]byte {
	chromaX := x
	if photometric == "YBR_FULL_422" {
		chromaX = x / 2
	}
	return [3]byte{
		byte(30 + 71*frame + 43*y + 27*x),
		byte(165 + 47*frame - 37*y + 63*chromaX),
		byte(80 - 51*frame + 81*y - 17*chromaX),
	}
}

func ybrReferenceBytes(frame int, region image.Rectangle, photometric string, planar uint16) []byte {
	var pixels []byte
	var planes [3][]byte
	for y := region.Min.Y; y < region.Max.Y; y++ {
		for x := region.Min.X; x < region.Max.X; x++ {
			s := ybrReferenceSample(frame, x, y, photometric)
			switch {
			case planar == 1:
				for channel, v := range s {
					planes[channel] = append(planes[channel], v)
				}
			case photometric == "YBR_FULL_422":
				if x%2 == 0 {
					next := ybrReferenceSample(frame, x+1, y, photometric)
					pixels = append(pixels, s[0], next[0], s[1], s[2])
				}
			default:
				pixels = append(pixels, s[:]...)
			}
		}
	}
	for _, plane := range planes {
		pixels = append(pixels, plane...)
	}
	return pixels
}

func assertYBRReferenceImage(t *testing.T, frame *Frame, index int, region image.Rectangle, photometric string) {
	t.Helper()
	im, err := frame.GetImage()
	if err != nil {
		t.Fatal(err)
	}
	if got := im.Bounds(); got != region.Sub(region.Min) {
		t.Fatalf("bounds %v, want dimensions %v", got, region.Size())
	}
	for y := region.Min.Y; y < region.Max.Y; y++ {
		for x := region.Min.X; x < region.Max.X; x++ {
			s := ybrReferenceSample(index, x, y, photometric)
			// Invert the luma-weighted BT.601 transform independently. Solve
			// for R and B by chroma definition, then recover G from the luma.
			const kr, kb = 0.299, 0.114
			luma := float64(s[0])
			r := luma + 2*(1-kr)*(float64(s[2])-128)
			b := luma + 2*(1-kb)*(float64(s[1])-128)
			g := (luma - kr*r - kb*b) / (1 - kr - kb)
			round := func(v float64) uint8 { return uint8(math.Round(max(0, min(v, 255)))) }
			want := color.RGBA{R: round(r), G: round(g), B: round(b), A: 255}
			got := requireRGBA(t, im).RGBAAt(x-region.Min.X, y-region.Min.Y)
			if got != want {
				t.Fatalf("pixel (%d,%d) from samples %v = %+v, want %+v", x, y, s, got, want)
			}
		}
	}
}
