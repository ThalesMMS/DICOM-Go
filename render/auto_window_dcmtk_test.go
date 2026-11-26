package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestAutoWindowAgainstDCMTK compares every strategy with the window dcm2img
// reports in its debug log (+Wm, +Wn, +Wh, +Wr) on a synthetic MR-like image
// with background, outliers and a saturated pixel. Enable with
// DICOMGO_DCMTK_INTEGRATION=1 and dcm2img on PATH.
func TestAutoWindowAgainstDCMTK(t *testing.T) {
	dcm2img := dcmtkoracle.Tool(t, "dcm2img")
	const size = 64
	values := make([]uint16, size*size)
	for i := range values {
		switch {
		case i < size:
			values[i] = 0 // background row
		case i%97 == 0:
			values[i] = uint16(100 + i%50) // outliers
		case i == 200:
			values[i] = 4000 // saturated
		default:
			values[i] = uint16(1000 + (i*37)%900)
		}
	}
	frame := autoWindowFrame(size, values)
	path := filepath.Join(t.TempDir(), "mr.dcm")
	writeAutoWindowDICOM(t, path, size, frame.PixelBytes)
	cases := []struct {
		args []string
		opts AutoWindowOptions
	}{
		{[]string{"+Wm"}, AutoWindowOptions{}},
		{[]string{"+Wn"}, AutoWindowOptions{Strategy: AutoWindowMinMaxIgnoreExtremes}},
		{[]string{"+Wh", "0"}, AutoWindowOptions{Strategy: AutoWindowHistogram}},
		{[]string{"+Wh", "1"}, AutoWindowOptions{Strategy: AutoWindowHistogram, HistogramIgnore: 0.01}},
		{[]string{"+Wh", "5"}, AutoWindowOptions{Strategy: AutoWindowHistogram, HistogramIgnore: 0.05}},
		{[]string{"+Wh", "10"}, AutoWindowOptions{Strategy: AutoWindowHistogram, HistogramIgnore: 0.10}},
		{[]string{"+Wr", "10", "10", "20", "20"}, AutoWindowOptions{Region: image.Rect(10, 10, 30, 30)}},
		{[]string{"+Wr", "0", "0", "64", "3"}, AutoWindowOptions{Region: image.Rect(0, 0, 64, 3)}},
	}
	pattern := regexp.MustCompile(`window center = ([-0-9.e+]+), width = ([-0-9.e+]+)`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, tc := range cases {
		args := append([]string{"-d"}, tc.args...)
		args = append(args, path, filepath.Join(t.TempDir(), "out.pgm"))
		output, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Exact, dcm2img, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("dcm2img %v: %v\n%s", args, err, output)
		}
		match := pattern.FindSubmatch(output)
		if match == nil {
			t.Fatalf("dcm2img %v logged no window:\n%s", tc.args, output)
		}
		center, _ := strconv.ParseFloat(string(match[1]), 64)
		width, _ := strconv.ParseFloat(string(match[2]), 64)
		got, ok := AutoWindowWith(frame, tc.opts)
		if !ok || got.Center != center || got.Width != width {
			t.Errorf("%v: got %+v %v, DCMTK center %v width %v", tc.args, got, ok, center, width)
		}
	}
}

func writeAutoWindowDICOM(t *testing.T, path string, size int, pixels []byte) {
	t.Helper()
	us := func(group, element, value uint16) core.Element {
		return core.NewRawElement(core.NewTag(group, element), core.VRUS, binary.LittleEndian.AppendUint16(nil, value))
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
	dataset := object.FromElements([]core.Element{
		text(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(0x0008, 0x0018, core.VRUI, "1.2.826.0.1.3680043.10.1.974.1"),
		text(0x0008, 0x0060, core.VRCS, "MR"),
		us(0x0028, 0x0002, 1),
		text(0x0028, 0x0004, core.VRCS, "MONOCHROME2"),
		us(0x0028, 0x0010, uint16(size)),
		us(0x0028, 0x0011, uint16(size)),
		us(0x0028, 0x0100, 16),
		us(0x0028, 0x0101, 12),
		us(0x0028, 0x0102, 11),
		us(0x0028, 0x0103, 0),
		core.NewRawElement(core.TagPixelData, core.VROW, pixels),
	}, nil)
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
