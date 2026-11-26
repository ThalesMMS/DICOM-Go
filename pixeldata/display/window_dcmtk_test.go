package display_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// TestWindowEdgesAgainstDCMTK renders an 8-bit ramp through narrow LINEAR and
// SIGMOID windows (width 1 is a threshold at c-0.5, PS3.3 C.11.2.1.2.1) with
// dcm2pnm and compares every pixel with RenderGray. Enable with
// DICOMGO_DCMTK_INTEGRATION=1 and dcm2pnm on PATH.
func TestWindowEdgesAgainstDCMTK(t *testing.T) {
	dcm2pnm := dcmtkoracle.Tool(t, "dcm2pnm")
	const columns, rows = 16, 16
	pixels := make([]byte, columns*rows)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ramp.dcm")
	writeLUTOracleFile(t, path, lutOracleDataset(false, 0, 0, nil, rows, columns, pixels))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, tc := range []struct {
		function      display.VOIFunction
		center, width float64
	}{
		{display.VOILinear, 100, 1},
		{display.VOILinear, 100.5, 1},
		{display.VOILinear, 100, 2},
		{display.VOILinear, 100, 3},
		{display.VOISigmoid, 100, 1},
		{display.VOISigmoid, 100, 4},
	} {
		name := fmt.Sprintf("%s-c%g-w%g", map[display.VOIFunction]string{display.VOILinear: "linear", display.VOISigmoid: "sigmoid"}[tc.function], tc.center, tc.width)
		t.Run(name, func(t *testing.T) {
			img, err := display.RenderGray(display.Frame{Rows: rows, Columns: columns, Pixels: pixels, Format: display.PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, VOI: display.VOILUT{Center: tc.center, Width: tc.width, Function: tc.function}})
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, name+".pgm")
			args := []string{"+op", "+Ww", fmt.Sprint(tc.center), fmt.Sprint(tc.width)}
			if tc.function == display.VOISigmoid {
				args = append(args, "+Wfs")
			}
			args = append(args, path, output)
			if out, err := dcmtkoracle.Command(ctx, t, "within 1 display level", dcm2pnm, args...).CombinedOutput(); err != nil {
				t.Fatalf("dcm2pnm %v: %v\n%s", args, err, out)
			}
			got := readPNM(t, output)
			worst := 0
			for i, value := range img.Pix {
				if diff := abs(got[i] - int(value)); diff > worst {
					worst = diff
					t.Logf("pixel %d: dcm2pnm %d, dicom-go %d", i, got[i], value)
				}
			}
			if worst > 1 {
				t.Errorf("differs from dcm2pnm by %d (tolerance 1)", worst)
			}
		})
	}
}
