package display_test

import (
	"bufio"
	"bytes"
	"context"
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

// TestGSDFAgainstDCMTK renders an 8-bit gradient with dcm2img through the
// monitor and printer characteristic curves shipped with DCMTK (with the GSDF
// and the CIELAB display functions, and the LIN OD presentation shape) and
// compares every output value with the display package. Enable with DICOMGO_DCMTK_INTEGRATION=1 and dcm2img on PATH; the
// curves are found next to the DCMTK installation or in DICOMGO_DCMTK_DATA.
func TestGSDFAgainstDCMTK(t *testing.T) {
	dcm2img := dcmtkoracle.Tool(t, "dcm2img")
	dataDir := os.Getenv("DICOMGO_DCMTK_DATA")
	if dataDir == "" {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dcm2img), "..", "share", "dcmtk*", "monitor.lut"))
		if len(matches) == 0 {
			t.Fatal("DCMTK monitor.lut not found; set DICOMGO_DCMTK_DATA")
		}
		dataDir = filepath.Dir(matches[len(matches)-1])
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "gradient.dcm")
	writeGradient(t, input)

	cases := []struct {
		name  string
		curve string
		typ   display.DeviceType
		args  []string
		linOD bool
		// function selects GSDF (default) or CIELAB (dcm2img +Dc).
		function display.DisplayFunction
		// tolerance is the largest DDL difference accepted: the GSDF LUTs
		// match DCMTK exactly; LIN OD differs by one DDL on some inputs.
		tolerance int
	}{
		{name: "monitor", curve: "monitor.lut", typ: display.DeviceMonitor, args: []string{"+Dm"}},
		{name: "monitor-cielab", curve: "monitor.lut", typ: display.DeviceMonitor, args: []string{"+Dm"}, function: display.FunctionCIELAB},
		{name: "printer-cielab", curve: "printer.lut", typ: display.DevicePrinter, args: []string{"+Dp"}, function: display.FunctionCIELAB},
		{name: "printer", curve: "printer.lut", typ: display.DevicePrinter, args: []string{"+Dp"}},
		{name: "printer-lin-od", curve: "printer.lut", typ: display.DevicePrinter, args: []string{"+Dp"}, linOD: true, tolerance: 1},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			curvePath := filepath.Join(dataDir, tc.curve)
			file, err := os.Open(curvePath)
			if err != nil {
				t.Fatal(err)
			}
			curve, err := display.ParseCharacteristicCurve(file, tc.typ)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			lut, err := display.DisplayCalibration{Curve: curve}.LUT(tc.function, 8)
			if err != nil {
				t.Fatal(err)
			}
			var presentation *display.LUT
			// LIN OD P-values keep 16 bits into a 16-bit display LUT.
			lut16, err := display.DisplayCalibration{Curve: curve}.GSDFLUT(16)
			if err != nil {
				t.Fatal(err)
			}
			ddl := func(v int) (int, int) {
				if presentation == nil {
					return v, int(lut.Map(v))
				}
				p := int(presentation.Entries[v])
				return p, int(lut16.Map(p))
			}
			if tc.linOD {
				// dcm2img +Pod without +Dn/+Dx: the default 0.20..3.00 OD
				// range, P-values over the printer's own range.
				density := display.DefaultPrintDensity
				density.Illumination = curve.Illumination
				density.ReflectedAmbientLight = curve.AmbientLight
				presentation, err = density.LinODPresentationLUTFor(lut16, 8)
				if err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(dir, tc.name+".pgm")
			args := append([]string{"+Ww", "127.5", "256"}, append(tc.args, curvePath)...)
			if tc.linOD {
				args = append(args, "+Pod")
			}
			if tc.function == display.FunctionCIELAB {
				args = append(args, "+Dc")
			}
			args = append(args, "+op", input, output)
			tolerance := dcmtkoracle.Exact
			if tc.tolerance > 0 {
				tolerance = fmt.Sprintf("within %d DDL", tc.tolerance)
			}
			if out, err := dcmtkoracle.Command(ctx, t, tolerance, dcm2img, args...).CombinedOutput(); err != nil {
				t.Fatalf("dcm2img %v: %v\n%s", args, err, out)
			}
			got := readPGM(t, output)
			worst, differing := 0, 0
			for v := 0; v < 256; v++ {
				_, want := ddl(v)
				diff := want - int(got[v])
				if diff < 0 {
					diff = -diff
				}
				if diff > 0 {
					differing++
				}
				worst = max(worst, diff)
			}
			t.Logf("%s: %d of 256 values differ, max |diff| %d", tc.name, differing, worst)
			if os.Getenv("DICOMGO_GSDF_DUMP") != "" {
				for v := 0; v < 256; v += 8 {
					p, want := ddl(v)
					t.Logf("v=%3d p=%5d go=%3d dcmtk=%3d", v, p, want, got[v])
				}
			}
			if worst > tc.tolerance {
				t.Fatalf("%s: DDLs differ from DCMTK by up to %d", tc.name, worst)
			}
		})
	}
}

func writeGradient(t *testing.T, path string) {
	t.Helper()
	pixels := make([]byte, 256)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	us := func(group, element uint16, value uint16) core.Element {
		return core.NewRawElement(core.NewTag(group, element), core.VRUS, []byte{byte(value), byte(value >> 8)})
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
		text(0x0008, 0x0018, core.VRUI, "1.2.826.0.1.3680043.10.1.973.1"),
		text(0x0008, 0x0060, core.VRCS, "OT"),
		us(0x0028, 0x0002, 1),
		text(0x0028, 0x0004, core.VRCS, "MONOCHROME2"),
		us(0x0028, 0x0010, 16),
		us(0x0028, 0x0011, 16),
		us(0x0028, 0x0100, 8),
		us(0x0028, 0x0101, 8),
		us(0x0028, 0x0102, 7),
		us(0x0028, 0x0103, 0),
		core.NewRawElement(core.TagPixelData, core.VROB, pixels),
	}, nil)
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readPGM reads an 8-bit binary PGM (P5).
func readPGM(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(bytes.NewReader(data))
	var fields []string
	for len(fields) < 4 {
		var token string
		if _, err := fmt.Fscan(reader, &token); err != nil {
			t.Fatalf("PGM header: %v", err)
		}
		if token[0] == '#' {
			reader.ReadString('\n')
			continue
		}
		fields = append(fields, token)
	}
	if fields[0] != "P5" || fields[3] != "255" {
		t.Fatalf("PGM header %v", fields)
	}
	width, _ := strconv.Atoi(fields[1])
	height, _ := strconv.Atoi(fields[2])
	reader.ReadByte()
	pixels := make([]byte, width*height)
	if _, err := reader.Read(pixels); err != nil {
		t.Fatal(err)
	}
	return pixels
}
