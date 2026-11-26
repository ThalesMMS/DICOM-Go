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
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestColorAgainstDCMTK renders synthetic YBR_PARTIAL_422, 12-bit RGB and
// 16-bit YBR_FULL images with dcm2img (8-bit binary and 16-bit ASCII PPM)
// and compares every channel with RenderColor and RenderColor16. Enable with
// DICOMGO_DCMTK_INTEGRATION=1 and dcm2img on PATH.
func TestColorAgainstDCMTK(t *testing.T) {
	dcm2img := dcmtkoracle.Tool(t, "dcm2img")
	const columns, rows = 16, 8
	pixels := columns * rows
	// Tolerances are in output units (8-bit and 16-bit). The partial-range
	// conversion uses the exact inverse of the PS3.3 C.7.6.3.1.2 matrix
	// (Y scale 1.1644); DCMTK uses slightly different constants (about
	// 1.1631), up to two 8-bit steps apart. DCMTK also rounds its YBR_FULL
	// constants (a few 16-bit units).
	cases := []struct {
		name        string
		frame       display.ColorFrame
		tol8, tol16 int
	}{
		{name: "ybr-partial-422", frame: display.ColorFrame{Photometric: "YBR_PARTIAL_422", Format: display.PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, Pixels: ybr422Samples(pixels)}, tol8: 2, tol16: 2 * 257},
		{name: "rgb-12bit", frame: display.ColorFrame{Photometric: "RGB", Format: display.PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11}, Pixels: rgb12Samples(pixels)}, tol8: 1, tol16: 1},
		{name: "ybr-full-16bit-planar", frame: display.ColorFrame{Photometric: "YBR_FULL", PlanarConfiguration: 1, Format: display.PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15}, Pixels: ybr16Samples(pixels)}, tol8: 1, tol16: 8},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := tc.frame
			frame.Rows, frame.Columns, frame.SamplesPerPixel = rows, columns, 3
			path := filepath.Join(dir, tc.name+".dcm")
			writeColorDICOM(t, path, frame)
			want8, err := display.RenderColor(frame)
			if err != nil {
				t.Fatal(err)
			}
			want16, err := display.RenderColor16(frame)
			if err != nil {
				t.Fatal(err)
			}
			for _, depth := range []int{8, 16} {
				output := filepath.Join(dir, fmt.Sprintf("%s-%d.ppm", tc.name, depth))
				flag, tol := "+op", tc.tol8
				if depth == 16 {
					flag, tol = "+opw", tc.tol16
				}
				tolerance := fmt.Sprintf("within %d per channel (%d-bit output)", tol, depth)
				if out, err := dcmtkoracle.Command(ctx, t, tolerance, dcm2img, flag, path, output).CombinedOutput(); err != nil {
					t.Fatalf("dcm2img %s: %v\n%s", flag, err, out)
				}
				got, maxValue := readPPM(t, output)
				worst := 0
				for i := 0; i < pixels; i++ {
					for c := 0; c < 3; c++ {
						var want int
						if depth == 8 {
							want = int(want8.Pix[4*i+c])
						} else {
							want = int(uint16(want16.Pix[8*i+2*c])<<8 | uint16(want16.Pix[8*i+2*c+1]))
						}
						value := got[3*i+c]
						if depth == 16 && maxValue != 65535 {
							value = int(float64(value) * 65535 / float64(maxValue))
						}
						worst = max(worst, abs(want-value))
					}
				}
				t.Logf("%s %d-bit: max |diff| %d (DCMTK max value %d)", tc.name, depth, worst, maxValue)
				if worst > tol {
					t.Errorf("%s %d-bit differs from DCMTK by %d (tolerance %d)", tc.name, depth, worst, tol)
				}
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

// ybr422Samples is 4:2:2 partial-range data: two lumas then Cb and Cr per
// pixel pair, sweeping the legal ranges.
func ybr422Samples(pixels int) []byte {
	out := make([]byte, 0, 2*pixels)
	for pair := 0; pair < pixels/2; pair++ {
		y := 16 + (pair*29)%220
		out = append(out, byte(y), byte(16+(pair*53)%220), byte(16+(pair*37)%225), byte(16+(pair*71)%225))
	}
	return out
}

func rgb12Samples(pixels int) []byte {
	out := make([]byte, 0, 6*pixels)
	for i := 0; i < pixels; i++ {
		for c := 0; c < 3; c++ {
			out = binary.LittleEndian.AppendUint16(out, uint16((i*(97+c*31)+c*1000)%4096))
		}
	}
	return out
}

func ybr16Samples(pixels int) []byte {
	out := make([]byte, 0, 6*pixels)
	for c := 0; c < 3; c++ {
		for i := 0; i < pixels; i++ {
			value := 20000 + (i*431)%25000
			if c > 0 {
				value = 32768 + ((i*(c*977))%12000 - 6000)
			}
			out = binary.LittleEndian.AppendUint16(out, uint16(value))
		}
	}
	return out
}

func writeColorDICOM(t *testing.T, path string, frame display.ColorFrame) {
	t.Helper()
	us := func(group, element uint16, value int) core.Element {
		return core.NewRawElement(core.NewTag(group, element), core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(value)))
	}
	text := func(group, element uint16, vr core.VR, value string) core.Element {
		if len(value)%2 == 1 {
			if vr == core.VRUI {
				value += "\x00"
			} else {
				value += " "
			}
		}
		return core.NewRawElement(core.NewTag(group, element), vr, []byte(value))
	}
	vr := core.VROB
	if frame.Format.BitsAllocated == 16 {
		vr = core.VROW
	}
	dataset := object.FromElements([]core.Element{
		text(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(0x0008, 0x0018, core.VRUI, "1.2.826.0.1.3680043.10.1.975.1"),
		text(0x0008, 0x0060, core.VRCS, "OT"),
		us(0x0028, 0x0002, 3),
		text(0x0028, 0x0004, core.VRCS, frame.Photometric),
		us(0x0028, 0x0006, frame.PlanarConfiguration),
		us(0x0028, 0x0010, frame.Rows),
		us(0x0028, 0x0011, frame.Columns),
		us(0x0028, 0x0100, frame.Format.BitsAllocated),
		us(0x0028, 0x0101, frame.Format.BitsStored),
		us(0x0028, 0x0102, frame.Format.HighBit),
		us(0x0028, 0x0103, 0),
		core.NewRawElement(core.TagPixelData, vr, frame.Pixels),
	}, nil)
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readPPM reads a binary (P6, 8-bit) or ASCII (P3) PPM and its maximum value.
func readPPM(t *testing.T, path string) ([]int, int) {
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
				t.Fatalf("PPM: %v", err)
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
	maxValue, _ := strconv.Atoi(token())
	out := make([]int, 3*width*height)
	switch magic {
	case "P6":
		reader.ReadByte()
		raw := make([]byte, len(out))
		if _, err := readFullPPM(reader, raw); err != nil {
			t.Fatal(err)
		}
		for i, b := range raw {
			out[i] = int(b)
		}
	case "P3":
		for i := range out {
			out[i], _ = strconv.Atoi(token())
		}
	default:
		t.Fatalf("PPM magic %q", magic)
	}
	return out, maxValue
}

func readFullPPM(reader *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := reader.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
