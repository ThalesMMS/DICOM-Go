package display_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestLUTEncodingsAgainstDCMTK renders synthetic 8-bit images through VOI
// LUTs and palette color LUTs in each LUT Data encoding with dcm2pnm and
// compares every pixel with RenderGray and RenderColor. Legacy encodings are
// read with LUTOptions.Legacy here and with DCMTK's defaults where no depth is
// corrected; a corrected depth needs +Mc (--check-lut-depth) for palettes and
// +Mv (--ignore-vlut-depth, the depth of the largest entry, which is the
// corrected depth for these tables) for VOI LUTs, since DCMTK does not check
// VOI LUT depths. Enable with DICOMGO_DCMTK_INTEGRATION=1 and dcm2pnm on PATH.
func TestLUTEncodingsAgainstDCMTK(t *testing.T) {
	dcm2pnm := dcmtkoracle.Tool(t, "dcm2pnm")
	const count = 256
	table := func(f func(i int) int) []uint16 {
		out := make([]uint16, count)
		for i := range out {
			out[i] = uint16(f(i))
		}
		return out
	}
	words := func(entries []uint16) []byte {
		out := make([]byte, 0, 2*len(entries))
		for _, entry := range entries {
			out = binary.LittleEndian.AppendUint16(out, entry)
		}
		return out
	}
	square8 := table(func(i int) int { return i * i / 255 })
	square12 := table(func(i int) int { return i * i * 4095 / (255 * 255) })
	square16 := table(func(i int) int { return i * i * 65535 / (255 * 255) })
	ramp10 := table(func(i int) int { return i * 4 })
	repeated := table(func(i int) int { return 255 - i })

	// VOI output is scaled over 0..2^n-1 here and over 2^n by DCMTK: one
	// 8-bit step apart at most. Palette entries keep their top eight bits in
	// both.
	cases := []struct {
		name       string
		palette    bool
		bits       int
		data       []byte
		legacy     bool
		dcmtkFlags []string
		tolerance  int
	}{
		{name: "voi-8bit-packed", bits: 8, data: display.EncodeLUTData(square8, 8), tolerance: 1},
		{name: "voi-8bit-one-per-word", bits: 8, data: words(square8), tolerance: 1},
		{name: "voi-12bit", bits: 12, data: words(square12), tolerance: 1},
		{name: "voi-16bit", bits: 16, data: words(square16), tolerance: 1},
		{name: "voi-short-data", bits: 16, data: words(square16[:200]), legacy: true, tolerance: 1},
		{name: "voi-16bit-of-8bit-values", bits: 16, data: words(square8), legacy: true, dcmtkFlags: []string{"+Mv"}, tolerance: 1},
		{name: "voi-8bit-of-16bit-values", bits: 8, data: words(square16), legacy: true, dcmtkFlags: []string{"+Mv"}, tolerance: 1},
		{name: "voi-bits-out-of-range", bits: 7, data: words(ramp10), legacy: true, tolerance: 1},
		{name: "palette-8bit-packed", palette: true, bits: 8, data: display.EncodeLUTData(square8, 8)},
		{name: "palette-8bit-one-per-word", palette: true, bits: 8, data: words(repeated)},
		{name: "palette-12bit", palette: true, bits: 12, data: words(square12)},
		{name: "palette-16bit", palette: true, bits: 16, data: words(square16)},
		{name: "palette-16bit-of-8bit-values", palette: true, bits: 16, data: words(square8), legacy: true, dcmtkFlags: []string{"+Mc"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	const columns, rows = 16, 16
	pixels := make([]byte, columns*rows)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataset := lutOracleDataset(tc.palette, count, tc.bits, tc.data, rows, columns, pixels)
			options := display.LUTOptions{Legacy: tc.legacy}
			var want []int
			if tc.palette {
				palette, err := display.PaletteFromObject(dataset, options)
				if err != nil {
					t.Fatal(err)
				}
				img, err := display.RenderColor(display.ColorFrame{Rows: rows, Columns: columns, Pixels: pixels, Photometric: "PALETTE COLOR", SamplesPerPixel: 1, Format: display.PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, Palette: palette})
				if err != nil {
					t.Fatal(err)
				}
				for i := range pixels {
					want = append(want, int(img.Pix[4*i]), int(img.Pix[4*i+1]), int(img.Pix[4*i+2]))
				}
				t.Logf("adjustments: %v", palette.Red.Adjustments)
			} else {
				voi, err := display.VOIFromObject(dataset, options)
				if err != nil {
					t.Fatal(err)
				}
				img, err := display.RenderGray(display.Frame{Rows: rows, Columns: columns, Pixels: pixels, Format: display.PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, VOI: voi})
				if err != nil {
					t.Fatal(err)
				}
				for _, value := range img.Pix {
					want = append(want, int(value))
				}
				t.Logf("adjustments: %v", voi.LUT.Adjustments)
			}

			path := filepath.Join(dir, tc.name+".dcm")
			writeLUTOracleFile(t, path, dataset)
			output := filepath.Join(dir, tc.name+".pnm")
			args := append([]string{"+op"}, tc.dcmtkFlags...)
			if !tc.palette {
				args = append(args, "+Wl", "1")
			}
			args = append(args, path, output)
			tolerance := dcmtkoracle.Exact
			if tc.tolerance > 0 {
				tolerance = fmt.Sprintf("within %d per output value", tc.tolerance)
			}
			if out, err := dcmtkoracle.Command(ctx, t, tolerance, dcm2pnm, args...).CombinedOutput(); err != nil {
				t.Fatalf("dcm2pnm %v: %v\n%s", args, err, out)
			}
			got := readPNM(t, output)
			if len(got) != len(want) {
				t.Fatalf("dcm2pnm wrote %d samples, want %d", len(got), len(want))
			}
			worst := 0
			for i := range got {
				worst = max(worst, abs(got[i]-want[i]))
			}
			t.Logf("max |diff| %d", worst)
			if worst > tc.tolerance {
				t.Errorf("differs from dcm2pnm by %d (tolerance %d)", worst, tc.tolerance)
			}
		})
	}
}

// lutOracleDataset is an 8-bit MONOCHROME2 image with a VOI LUT (none when
// data is nil), or a PALETTE COLOR image whose red, green and blue LUTs share
// the data.
func lutOracleDataset(palette bool, count, bits int, data []byte, rows, columns int, pixels []byte) *object.Object {
	us := func(tag core.Tag, values ...int) core.Element {
		out := make([]byte, 0, 2*len(values))
		for _, value := range values {
			out = binary.LittleEndian.AppendUint16(out, uint16(value))
		}
		return core.NewRawElement(tag, core.VRUS, out)
	}
	text := func(tag core.Tag, vr core.VR, value string) core.Element {
		if len(value)%2 == 1 {
			if vr == core.VRUI {
				value += "\x00"
			} else {
				value += " "
			}
		}
		return core.NewRawElement(tag, vr, []byte(value))
	}
	photometric := "MONOCHROME2"
	if palette {
		photometric = "PALETTE COLOR"
	}
	elements := []core.Element{
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.1027.1"),
		text(core.NewTag(0x0008, 0x0060), core.VRCS, "OT"),
		us(core.NewTag(0x0028, 0x0002), 1),
		text(core.NewTag(0x0028, 0x0004), core.VRCS, photometric),
		us(core.NewTag(0x0028, 0x0010), rows),
		us(core.NewTag(0x0028, 0x0011), columns),
		us(core.NewTag(0x0028, 0x0100), 8),
		us(core.NewTag(0x0028, 0x0101), 8),
		us(core.NewTag(0x0028, 0x0102), 7),
		us(core.NewTag(0x0028, 0x0103), 0),
	}
	if palette {
		for element := uint16(0x1101); element <= 0x1103; element++ {
			elements = append(elements,
				us(core.NewTag(0x0028, element), count, 0, bits),
				core.NewRawElement(core.NewTag(0x0028, element+0x100), core.VROW, data))
		}
	} else if data != nil {
		elements = append(elements, dicomtest.NewSequenceElement(core.NewTag(0x0028, 0x3010), core.DataSet{Elements: []core.Element{
			us(core.NewTag(0x0028, 0x3002), count, 0, bits),
			core.NewRawElement(core.NewTag(0x0028, 0x3006), core.VROW, data),
		}}))
	}
	elements = append(elements, core.NewRawElement(core.TagPixelData, core.VROB, pixels))
	return object.FromElements(elements, nil)
}

func writeLUTOracleFile(t *testing.T, path string, dataset *object.Object) {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readPNM reads the samples of a binary 8-bit PGM (P5) or PPM (P6).
func readPNM(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(bytes.NewReader(data))
	token := func() string {
		for {
			var value string
			if _, err := fmt.Fscan(reader, &value); err != nil {
				t.Fatalf("PNM: %v", err)
			}
			if value[0] == '#' {
				reader.ReadString('\n')
				continue
			}
			return value
		}
	}
	magic := token()
	width, _ := strconv.Atoi(token())
	height, _ := strconv.Atoi(token())
	if maxValue, _ := strconv.Atoi(token()); maxValue != 255 {
		t.Fatalf("PNM maximum value %d, want 255", maxValue)
	}
	channels := 1
	switch magic {
	case "P5":
	case "P6":
		channels = 3
	default:
		t.Fatalf("PNM magic %q", magic)
	}
	reader.ReadByte()
	raw := make([]byte, channels*width*height)
	if _, err := readFullPPM(reader, raw); err != nil {
		t.Fatal(err)
	}
	out := make([]int, len(raw))
	for i, b := range raw {
		out[i] = int(b)
	}
	return out
}

// TestLUTFirstValueSignAgainstDCMTK renders 16-bit images through VOI LUTs
// whose first value mapped is negative, with the descriptor encoded as US or
// SS, on signed pixels and on unsigned pixels rescaled below zero, and a
// constant VOI LUT, with dcm2pnm, and compares every pixel with RenderGray.
// Enable with DICOMGO_DCMTK_INTEGRATION=1 and dcm2pnm on PATH.
func TestLUTFirstValueSignAgainstDCMTK(t *testing.T) {
	dcm2pnm := dcmtkoracle.Tool(t, "dcm2pnm")
	const columns, rows = 32, 32
	ramp := make([]uint16, 2048)
	for i := range ramp {
		ramp[i] = uint16(i * 2)
	}
	constant := make([]uint16, 2048)
	for i := range constant {
		constant[i] = 2048
	}
	cases := []struct {
		name         string
		signed       bool
		intercept    string
		descriptorVR core.VR
		entries      []uint16
	}{
		{name: "signed-pixels-us-descriptor", signed: true, descriptorVR: core.VRUS, entries: ramp},
		{name: "signed-pixels-ss-descriptor", signed: true, descriptorVR: core.VRSS, entries: ramp},
		{name: "rescaled-below-zero-us-descriptor", intercept: "-1024", descriptorVR: core.VRUS, entries: ramp},
		{name: "rescaled-below-zero-ss-descriptor", intercept: "-1024", descriptorVR: core.VRSS, entries: ramp},
		{name: "constant-lut", signed: true, descriptorVR: core.VRUS, entries: constant},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Modality values from -1100 to +1100, beyond both ends of the
			// LUT (first value mapped -1024, 2048 entries).
			pixels := make([]byte, 0, 2*columns*rows)
			for i := 0; i < columns*rows; i++ {
				value := -1100 + i*2200/(columns*rows-1)
				if !tc.signed {
					value += 1024
				}
				pixels = binary.LittleEndian.AppendUint16(pixels, uint16(int16(value)))
			}
			dataset := signOracleDataset(tc.signed, tc.intercept, tc.descriptorVR, tc.entries, rows, columns, pixels)
			modality, err := display.ModalityLUTFromObject(dataset)
			if err != nil {
				t.Fatal(err)
			}
			voi, err := display.VOIFromObject(dataset)
			if err != nil {
				t.Fatal(err)
			}
			if voi.LUT.FirstMapped != -1024 {
				t.Fatalf("FirstMapped = %d, want -1024", voi.LUT.FirstMapped)
			}
			bitsStored := 16
			if !tc.signed {
				bitsStored = 12
			}
			img, err := display.RenderGray(display.Frame{Rows: rows, Columns: columns, Pixels: pixels, Format: display.PixelFormat{BitsAllocated: 16, BitsStored: bitsStored, HighBit: bitsStored - 1, Signed: tc.signed, ByteOrder: binary.LittleEndian}, Modality: modality, VOI: voi})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, tc.name+".dcm")
			writeLUTOracleFile(t, path, dataset)
			output := filepath.Join(dir, tc.name+".pgm")
			if out, err := dcmtkoracle.Command(ctx, t, "within 1 output level", dcm2pnm, "+op", "+Wl", "1", path, output).CombinedOutput(); err != nil {
				t.Fatalf("dcm2pnm: %v\n%s", err, out)
			}
			got := readPNM(t, output)
			worst := 0
			for i, value := range img.Pix {
				worst = max(worst, abs(got[i]-int(value)))
			}
			t.Logf("max |diff| %d", worst)
			if worst > 1 {
				t.Errorf("differs from dcm2pnm by %d (tolerance 1)", worst)
			}
		})
	}
}

// signOracleDataset is a 16-bit MONOCHROME2 image with a 16-bit VOI LUT whose
// first value mapped is -1024, and a rescale intercept when given.
func signOracleDataset(signed bool, intercept string, descriptorVR core.VR, entries []uint16, rows, columns int, pixels []byte) *object.Object {
	us := func(tag core.Tag, value int) core.Element {
		return core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(value)))
	}
	text := func(tag core.Tag, vr core.VR, value string) core.Element {
		if len(value)%2 == 1 {
			if vr == core.VRUI {
				value += "\x00"
			} else {
				value += " "
			}
		}
		return core.NewRawElement(tag, vr, []byte(value))
	}
	bitsStored, representation := 12, 0
	if signed {
		bitsStored, representation = 16, 1
	}
	descriptor := binary.LittleEndian.AppendUint16(nil, uint16(len(entries)))
	descriptor = binary.LittleEndian.AppendUint16(descriptor, uint16(0xFC00))
	descriptor = binary.LittleEndian.AppendUint16(descriptor, 16)
	elements := []core.Element{
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.1028.1"),
		text(core.NewTag(0x0008, 0x0060), core.VRCS, "OT"),
		us(core.NewTag(0x0028, 0x0002), 1),
		text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		us(core.NewTag(0x0028, 0x0010), rows),
		us(core.NewTag(0x0028, 0x0011), columns),
		us(core.NewTag(0x0028, 0x0100), 16),
		us(core.NewTag(0x0028, 0x0101), bitsStored),
		us(core.NewTag(0x0028, 0x0102), bitsStored-1),
		us(core.NewTag(0x0028, 0x0103), representation),
	}
	if intercept != "" {
		elements = append(elements,
			text(core.NewTag(0x0028, 0x1052), core.VRDS, intercept),
			text(core.NewTag(0x0028, 0x1053), core.VRDS, "1"))
	}
	elements = append(elements,
		dicomtest.NewSequenceElement(core.NewTag(0x0028, 0x3010), core.DataSet{Elements: []core.Element{
			core.NewRawElement(core.NewTag(0x0028, 0x3002), descriptorVR, descriptor),
			core.NewRawElement(core.NewTag(0x0028, 0x3006), core.VROW, display.EncodeLUTData(entries, 16)),
		}}),
		core.NewRawElement(core.TagPixelData, core.VROW, pixels))
	return object.FromElements(elements, nil)
}
