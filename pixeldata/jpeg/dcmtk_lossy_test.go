package jpeg

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestLossyJPEGAgainstDCMTK decodes cjpeg-made lossy objects with
// dcmdjpeg under each color policy and compares the samples: 12-bit
// YBR_FULL_422 color, signed 8-bit monochrome and 12-bit precision with Bits
// Stored 10. Enable with DICOMGO_DCMTK_INTEGRATION=1 (cjpeg and dcmdjpeg on
// PATH).
func TestLossyJPEGAgainstDCMTK(t *testing.T) {
	dcmtkoracle.Require(t)
	cjpeg, err := exec.LookPath("cjpeg")
	if err != nil {
		t.Fatal("cjpeg is required")
	}
	dcmdjpeg := dcmtkoracle.Tool(t, "dcmdjpeg")
	dir := t.TempDir()
	const width, height = 37, 21
	pnm := func(name string, color bool, maxValue int, value func(x, y, c int) int) string {
		var buf bytes.Buffer
		magic, channels := "P5", 1
		if color {
			magic, channels = "P6", 3
		}
		fmt.Fprintf(&buf, "%s\n%d %d\n%d\n", magic, width, height, maxValue)
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				for c := 0; c < channels; c++ {
					if maxValue > 255 {
						buf.Write(binary.BigEndian.AppendUint16(nil, uint16(value(x, y, c))))
					} else {
						buf.WriteByte(byte(value(x, y, c)))
					}
				}
			}
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	encode := func(input string, args ...string) []byte {
		output := input + ".jpg"
		if out, err := runJPEGInteropCommand(cjpeg, append(append(args, "-outfile", output), input)...); err != nil {
			t.Fatalf("cjpeg: %v: %s", err, out)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	colorSource := pnm("color.ppm", true, 4095, func(x, y, c int) int {
		return []int{(x*97 + y*13) % 4096, (x*31 + y*211) % 4096, (x*x + y*y*3) % 4096}[c]
	})
	signedSource := pnm("signed.pgm", false, 255, func(x, y, _ int) int { return (x*7 + y*11) % 256 })
	// Maxval 4095 so cjpeg keeps the 10-bit values unscaled.
	tenSource := pnm("ten.pgm", false, 4095, func(x, y, _ int) int { return (x*29 + y*43) % 1024 })
	cases := []struct {
		name        string
		stream      []byte
		syntax      transfer.Syntax
		samples     uint16
		bits        [3]uint16 // allocated, stored, signed
		photometric string
		policy      pixeldata.JPEGColorConversion
		flag        string
		tolerance   int
	}{
		{"color12-photometric", encode(colorSource, "-precision", "12", "-quality", "90", "-sample", "2x2,1x1,1x1"), transfer.JPEGExtended, 3, [3]uint16{16, 12, 0}, "YBR_FULL_422", pixeldata.JPEGColorPhotometric, "+cp", 2},
		{"color12-never", encode(colorSource, "-precision", "12", "-quality", "90", "-sample", "2x1,1x1,1x1"), transfer.JPEGExtended, 3, [3]uint16{16, 12, 0}, "YBR_FULL_422", pixeldata.JPEGColorNever, "+cn", 2},
		{"signed8", encode(signedSource, "-grayscale", "-quality", "90"), transfer.JPEGBaseline, 1, [3]uint16{8, 8, 1}, "MONOCHROME2", pixeldata.JPEGColorGuess, "+cp", 1},
		{"stored10", encode(tenSource, "-precision", "12", "-grayscale", "-quality", "90"), transfer.JPEGExtended, 1, [3]uint16{16, 10, 0}, "MONOCHROME2", pixeldata.JPEGColorGuess, "+cp", 1},
		// 8-bit color goes through image/jpeg.
		{"color8-guess", encode(pnm("color8.ppm", true, 255, func(x, y, c int) int { return []int{(x * 7) % 256, (y * 13) % 256, (x + y) * 5 % 256}[c] }), "-quality", "90"), transfer.JPEGBaseline, 3, [3]uint16{8, 8, 0}, "YBR_FULL_422", pixeldata.JPEGColorGuess, "+cg", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := filepath.Join(dir, tc.name+".dcm")
			writeLossyJPEGDICOM(t, input, tc.stream, tc.syntax, width, height, tc.samples, tc.bits, tc.photometric)
			output := filepath.Join(dir, tc.name+"-dcmtk.dcm")
			dcmtkoracle.Run(t, fmt.Sprintf("within %d per sample (lossy)", tc.tolerance), dcmdjpeg, tc.flag, input, output)
			reference, err := object.OpenFile(output)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := reference.Dataset.GetRaw(core.TagPixelData)
			wantPhotometric, _ := reference.Dataset.GetString(tagPhotometricInterpretation)
			source, err := object.OpenFile(input)
			if err != nil {
				t.Fatal(err)
			}
			registry := pixeldata.NewMemoryRegistry()
			if err := Register(registry); err != nil {
				t.Fatal(err)
			}
			decoded, err := pixeldata.DecompressFile(source, pixeldata.DecompressOptions{Registry: registry, JPEGColorConversion: tc.policy})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := decoded.Dataset.GetRaw(core.TagPixelData)
			gotPhotometric, _ := decoded.Dataset.GetString(tagPhotometricInterpretation)
			if gotPhotometric != wantPhotometric {
				t.Errorf("photometric %q, DCMTK %q", gotPhotometric, wantPhotometric)
			}
			// DCMTK pads odd-length Pixel Data.
			if len(want) == len(got)+1 {
				want = want[:len(got)]
			}
			if len(got) != len(want) {
				t.Fatalf("pixel bytes %d, DCMTK %d", len(got), len(want))
			}
			width := int(tc.bits[0] / 8)
			worst := 0
			for i := 0; i+width <= len(got); i += width {
				a, b := int(got[i]), int(want[i])
				if width == 2 {
					a, b = int(binary.LittleEndian.Uint16(got[i:])), int(binary.LittleEndian.Uint16(want[i:]))
				}
				if tc.bits[2] == 1 {
					a, b = int(int8(a)), int(int8(b))
				}
				worst = max(worst, abs(a-b))
			}
			t.Logf("%s: max |diff| from dcmdjpeg %d", tc.name, worst)
			if tc.tolerance > 0 && worst > tc.tolerance {
				t.Errorf("%s differs from dcmdjpeg by %d (tolerance %d)", tc.name, worst, tc.tolerance)
			}
		})
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func writeLossyJPEGDICOM(t *testing.T, path string, stream []byte, syntax transfer.Syntax, width, height int, samples uint16, bits [3]uint16, photometric string) {
	t.Helper()
	if len(stream)%2 == 1 {
		stream = append(stream, 0)
	}
	elements := []core.Element{
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.977.1"),
		dicomtest.Uint16Element(tagSamplesPerPixel, core.VRUS, nil, samples),
		dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, photometric),
		dicomtest.Uint16Element(tagRows, core.VRUS, nil, uint16(height)),
		dicomtest.Uint16Element(tagColumns, core.VRUS, nil, uint16(width)),
		dicomtest.Uint16Element(tagBitsAllocated, core.VRUS, nil, bits[0]),
		dicomtest.Uint16Element(tagBitsStored, core.VRUS, nil, bits[1]),
		dicomtest.Uint16Element(tagHighBit, core.VRUS, nil, bits[1]-1),
		dicomtest.Uint16Element(tagPixelRepresentation, core.VRUS, nil, bits[2]),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, stream),
	}
	if samples == 3 {
		elements = append(elements, dicomtest.Uint16Element(tagPlanarConfiguration, core.VRUS, nil, 0))
	}
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: syntax}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
