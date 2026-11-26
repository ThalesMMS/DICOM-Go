package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/gsps"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

func precisionImage(t *testing.T, values []uint16) *object.File {
	t.Helper()
	file := ctSeries(t, 1, nil)[0]
	for tag, v := range map[core.Tag]uint16{core.NewTag(0x0028, 0x0010): 1, core.NewTag(0x0028, 0x0011): uint16(len(values)), core.NewTag(0x0028, 0x0101): 16, core.NewTag(0x0028, 0x0102): 15, core.NewTag(0x0028, 0x0103): 0} {
		file.Dataset.Put(dicomtest.NewUShortElement(tag, v))
	}
	file.Dataset.Remove(core.NewTag(0x0028, 0x1052))
	file.Dataset.Remove(core.NewTag(0x0028, 0x1053))
	data := make([]byte, 2*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint16(data[2*i:], v)
	}
	file.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, data))
	return file
}

func addOverlay(t *testing.T, obj *object.Object, origin int) {
	t.Helper()
	for tag, v := range map[core.Tag]uint16{core.NewTag(0x6000, 0x0010): 1, core.NewTag(0x6000, 0x0011): 4, core.NewTag(0x6000, 0x0100): 1, core.NewTag(0x6000, 0x0102): 0} {
		obj.Put(dicomtest.NewUShortElement(tag, v))
	}
	obj.Put(dicomtest.NewStringElement(core.NewTag(0x6000, 0x0040), core.VRCS, "G"))
	obj.Put(core.NewRawElement(core.NewTag(0x6000, 0x0050), core.VRSS, []byte{1, 0, 1, 0}))
	obj.Put(dicomtest.NewUShortElement(core.NewTag(0x6000, 0x0051), uint16(origin)))
	obj.Put(core.NewRawElement(core.NewTag(0x6000, 0x3000), core.VROW, []byte{3, 0}))
}

func TestOverlaysPresentationOrderAndPrecision(t *testing.T) {
	dir := t.TempDir()
	file := precisionImage(t, []uint16{0, 1, 2, 3})
	addOverlay(t, file.Dataset, 1)
	file.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x2050, 0x0020), core.VRCS, "INVERSE"))
	file.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0018, 0x1600), core.VRCS, "RECTANGULAR"))
	for tag, v := range map[core.Tag]uint16{core.NewTag(0x0018, 0x1602): 2, core.NewTag(0x0018, 0x1604): 4, core.NewTag(0x0018, 0x1606): 1, core.NewTag(0x0018, 0x1608): 1, core.NewTag(0x0018, 0x1622): 0x1234} {
		file.Dataset.Put(dicomtest.NewUShortElement(tag, v))
	}
	in := writeFile(t, dir, "image.dcm", file)
	out := filepath.Join(dir, "out.png")
	if code, _, stderr := runTool("-bits", "16", "-window", "32768,65536", "-presentation", in, out); code != 0 {
		t.Fatal(stderr)
	}
	g := readPNG(t, out).(*image.Gray16)
	if g.Gray16At(0, 0).Y != 0x1234 || g.Gray16At(1, 0).Y != 65534 {
		t.Fatalf("shutter/LUT %v", g.Pix)
	}
	if code, _, stderr := runTool("-bits", "16", "-window", "32768,65536", "-presentation", "-overlays", in, out); code != 0 {
		t.Fatal(stderr)
	}
	g = readPNG(t, out).(*image.Gray16)
	if g.Gray16At(0, 0).Y != 65535 || g.Gray16At(1, 0).Y != 65535 || g.Gray16At(2, 0).Y != 65533 {
		t.Fatalf("image presentation order %v", g.Pix)
	}
	// Plane coverage must follow Image Frame Origin instead of burning frame 1.
	metadata, err := pixeldata.ExtractMetadata(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Put(dicomtest.NewUShortElement(core.NewTag(0x6000, 0x0051), 2))
	opts := options{voi: 1, presentationOptions: presentationOptions{bits: 16, overlays: true}, window: &display.VOILUT{Center: 32768, Width: 65536}}
	data, _ := file.Dataset.GetRaw(core.TagPixelData)
	for i, want := range []uint16{0, 65535} {
		img, err := renderFrame(file.Dataset, metadata, data, i, binary.LittleEndian, opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := img.(*image.Gray16).Gray16At(0, 0).Y; got != want {
			t.Fatalf("frame %d first sample %d want %d", i, got, want)
		}
	}
}

func TestLinODUsesSixteenBitInputForWideOutput(t *testing.T) {
	dir := t.TempDir()
	file := precisionImage(t, []uint16{32700, 32701, 32750, 33000})
	file.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x2050, 0x0020), core.VRCS, "LIN OD"))
	out := filepath.Join(dir, "linod.png")
	if code, _, stderr := runTool("-bits", "16", "-presentation", "-window", "32768,65536", writeFile(t, dir, "image.dcm", file), out); code != 0 {
		t.Fatal(stderr)
	}
	g := readPNG(t, out).(*image.Gray16)
	lut, err := display.PrintDensityFromObject(file.Dataset).LinODPresentationLUT(16)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range []uint16{32700, 32701, 32750, 33000} {
		if got := g.Gray16At(i, 0).Y; got != lut.Entries[v] {
			t.Fatalf("LIN OD pixel %d=%d want %d", i, got, lut.Entries[v])
		}
	}
	if g.Gray16At(0, 0).Y == g.Gray16At(2, 0).Y {
		t.Fatal("LIN OD reduced the 16-bit input to an eight-bit index")
	}
}

func TestGSPSReferenceVOIActivationShutterAndGeometry(t *testing.T) {
	dir := t.TempDir()
	file := precisionImage(t, []uint16{0, 1, 2, 3})
	addOverlay(t, file.Dataset, 1)
	state, err := gsps.FromImage(file.Dataset, gsps.FromImageOptions{IgnoreShutter: true, VOI: gsps.VOIIgnore})
	if err != nil {
		t.Fatal(err)
	}
	state.SoftcopyVOI = gsps.SoftcopyVOI{WindowCenter: 2, WindowWidth: 4}
	state.Shutter = &display.Shutter{Rectangle: &display.RectShutter{Left: 2, Right: 4, Upper: 1, Lower: 1}, PresentationValue: 7, PresentationPValue: 7 * 257, PresentationPValueDefined: true}
	state.SpatialTransform = gsps.SpatialTransform{RotationDegrees: 90}
	stateFile, err := gsps.Write(state)
	if err != nil {
		t.Fatal(err)
	}
	ps := writeFile(t, dir, "state.dcm", stateFile)
	in := writeFile(t, dir, "image.dcm", file)
	out := filepath.Join(dir, "out.png")
	if code, _, stderr := runTool("-gsps", ps, in, out); code != 0 {
		t.Fatal(stderr)
	}
	g := readPNG(t, out).(*image.Gray)
	if g.Bounds() != image.Rect(0, 0, 1, 4) || !bytes.Equal(g.Pix, []byte{7, 255, 170, 255}) {
		t.Fatalf("GSPS order/reference/window/rotation: bounds %v pixels %v", g.Bounds(), g.Pix)
	}
	if code, _, stderr := runTool("-gsps", ps, "-flip", "vertical", in, out); code != 0 {
		t.Fatal(stderr)
	}
	g = readPNG(t, out).(*image.Gray)
	if !bytes.Equal(g.Pix, []byte{255, 170, 255, 7}) {
		t.Fatalf("post-GSPS flip %v", g.Pix)
	}
	layerValue := uint16(0x4d00)
	state.GraphicLayers = append(state.GraphicLayers, gsps.GraphicLayer{Name: "MARK", Order: 2, RecommendedGrayscale: &layerValue})
	state.GraphicAnnotations = []gsps.GraphicAnnotation{{LayerName: "MARK", GraphicType: gsps.GraphicTypePolyline, GraphicUnits: gsps.AnnotationUnitsPixel, Polyline: []gsps.Point2D{{X: 2.5, Y: .5}, {X: 3.5, Y: .5}}}}
	annotated, err := gsps.Write(state)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runTool("-gsps", writeFile(t, dir, "annotated.dcm", annotated), in, out); code != 0 {
		t.Fatal(stderr)
	}
	g = readPNG(t, out).(*image.Gray)
	if g.GrayAt(0, 2).Y != 77 || g.GrayAt(0, 3).Y != 77 {
		t.Fatalf("GSPS rotated annotation/layer value %v", g.Pix)
	}
	state.ReferencedImages[0].SOPInstanceUID = "1.2.3.999"
	bad, err := gsps.Write(state)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runTool("-gsps", writeFile(t, dir, "bad-reference.dcm", bad), in, out); code != 1 || !strings.Contains(stderr, "missing reference") {
		t.Fatalf("reference refusal %d %s", code, stderr)
	}
	if code, _, _ := runTool("-gsps", ps, in, ps); code != 1 {
		t.Fatal("GSPS source overwritten")
	}
	if code, _, _ := runTool("-gsps", in, in, out); code != 1 {
		t.Fatal("image accepted as GSPS")
	}
}

func TestAutoWindowStrategiesAndRegion(t *testing.T) {
	file := precisionImage(t, []uint16{0, 1, 2, 3, 4, 100})
	metadata, err := pixeldata.ExtractMetadata(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := file.Dataset.GetRaw(core.TagPixelData)
	frame := display.Frame{Rows: 1, Columns: 6, Pixels: data, Format: display.PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15}}
	for _, tc := range []struct {
		name          string
		p             presentationOptions
		center, width float64
	}{
		{"min-max", presentationOptions{autoWindow: "min-max"}, 50.5, 101},
		{"ignore", presentationOptions{autoWindow: "ignore-extremes"}, 3, 4},
		{"histogram", presentationOptions{autoWindow: "histogram", histogramIgnore: 1.0 / 6}, 51, 100},
		{"region", presentationOptions{autoWindow: "min-max", region: image.Rect(1, 0, 4, 1)}, 2.5, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			voi, err := selectVOI(file.Dataset, metadata, frame, 0, options{presentationOptions: tc.p})
			if err != nil {
				t.Fatal(err)
			}
			if voi.Center != tc.center || voi.Width != tc.width {
				t.Fatalf("%+v want %g,%g", voi, tc.center, tc.width)
			}
		})
	}
	file.Dataset.Put(dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0120), 0))
	voi, err := selectVOI(file.Dataset, metadata, frame, 0, options{presentationOptions: presentationOptions{autoWindow: "min-max"}})
	if err != nil || voi.Center != 51 || voi.Width != 100 {
		t.Fatalf("padding %v %v", voi, err)
	}
	if _, err := selectVOI(file.Dataset, metadata, frame, 0, options{presentationOptions: presentationOptions{autoWindow: "min-max", region: image.Rect(5, 0, 7, 1)}}); err == nil {
		t.Fatal("out-of-frame region accepted")
	}
	frame.Modality.LUT = &display.LUT{Entries: []uint16{1, 2}, BitsPerEntry: 16}
	if _, err := selectVOI(file.Dataset, metadata, frame, 0, options{presentationOptions: presentationOptions{autoWindow: "histogram"}}); err == nil {
		t.Fatal("nonlinear modality silently approximated")
	}
}

func TestCalibratedFunctionsMonitorPrinterAnd16Bits(t *testing.T) {
	dir := t.TempDir()
	in := writeFile(t, dir, "image.dcm", precisionImage(t, []uint16{0, 1, 16384, 32768, 65535}))
	curve := filepath.Join(dir, "monitor.txt")
	if err := os.WriteFile(curve, []byte("max 65535\n0 0.05\n65535 1000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var outputs [][]byte
	for _, fn := range []string{"gsdf", "cielab"} {
		out := filepath.Join(dir, fn+".png")
		if code, _, stderr := runTool("-bits", "16", "-window", "32768,65536", "-monitor-curve", curve, "-display-function", fn, in, out); code != 0 {
			t.Fatal(stderr)
		}
		g := readPNG(t, out).(*image.Gray16)
		outputs = append(outputs, append([]byte(nil), g.Pix...))
		if g.Gray16At(0, 0).Y > 2 || g.Gray16At(4, 0).Y < 65000 || g.Gray16At(2, 0).Y%257 == 0 {
			t.Fatalf("calibration precision/range %v", g.Pix)
		}
	}
	if bytes.Equal(outputs[0], outputs[1]) {
		t.Fatal("display function selection had no effect")
	}
	printer := filepath.Join(dir, "printer.txt")
	if err := os.WriteFile(printer, []byte("max 255\n0 3\n255 0.2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"gsdf", "cielab"} {
		out := filepath.Join(dir, "print-"+fn+".png")
		if code, _, stderr := runTool("-printer-curve", printer, "-display-function", fn, "-display-ambient", "10", "-display-illumination", "2000", in, out); code != 0 {
			t.Fatal(stderr)
		}
		g := readPNG(t, out).(*image.Gray)
		if g.GrayAt(0, 0).Y >= g.GrayAt(4, 0).Y {
			t.Fatalf("printer density curve not used: %v", g.Pix)
		}
	}
	if code, _, _ := runTool("-monitor-curve", curve, in, curve); code != 1 {
		t.Fatal("curve overwritten")
	}
	bad := filepath.Join(dir, "bad.txt")
	_ = os.WriteFile(bad, []byte("max 0\n"), 0600)
	if code, _, _ := runTool("-monitor-curve", bad, in, filepath.Join(dir, "bad.png")); code != 1 {
		t.Fatal("invalid curve accepted")
	}
	_ = os.WriteFile(bad, []byte("max 255\n0 0.2\n255 3\n"), 0600)
	if code, _, stderr := runTool("-printer-curve", bad, "-display-function", "cielab", in, filepath.Join(dir, "bad.png")); code != 1 || !strings.Contains(stderr, "nondecreasing") {
		t.Fatalf("CIELAB incompatible curve: %d %s", code, stderr)
	}
}

func TestPresentationFlagValidation(t *testing.T) {
	for _, flags := range [][]string{{"-bits", "16", "-format", "jpeg"}, {"-bits", "12"}, {"-gsps", "p.dcm", "-bits", "16"}, {"-gsps", "p.dcm", "-window", "1,1"}, {"-gsps", "p.dcm", "-overlays"}, {"-auto-window", "bad"}, {"-auto-window", "histogram", "-histogram-ignore", "NaN"}, {"-histogram-ignore", ".1"}, {"-window-region", "0,0,0,1"}, {"-window-region", "0,0,2,1", "-voi", "1"}, {"-auto-window", "min-max", "-min-max"}, {"-rotate", "45"}, {"-flip", "diagonal"}, {"-scale", "Inf"}, {"-scale", "0"}, {"-size", "2,2", "-scale", "1"}, {"-size", "67108864,2"}, {"-monitor-curve", "m", "-printer-curve", "p"}, {"-display-function", "gsdf"}, {"-monitor-curve", "m", "-display-function", "unknown"}, {"-monitor-curve", "m", "-display-illumination", "10"}} {
		args := append(append([]string(nil), flags...), "in.dcm", "out.png")
		if code, _, _ := runTool(args...); code != 2 {
			t.Fatalf("%v code %d want usage refusal", flags, code)
		}
	}
}

func TestAuxiliaryInputsAndUnsupportedColorAreRefused(t *testing.T) {
	dir := t.TempDir()
	curve := filepath.Join(dir, "large-curve.txt")
	f, err := os.Create(curve)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((1 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := readPresentationFile(curve, 1<<20); err == nil {
		t.Fatal("oversized curve read")
	}
	if _, err := readPresentationFile(dir, 1<<20); err == nil {
		t.Fatal("directory accepted")
	}
	var text strings.Builder
	text.WriteString("max 65535\n")
	for i := 0; i < 4097; i++ {
		fmt.Fprintf(&text, "%d %g\n", i, float64(i)+.05)
	}
	if err := os.WriteFile(curve, []byte(text.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadPresentationOptions(&options{presentationOptions: presentationOptions{monitorCurve: curve, bits: 8}}); err == nil || !strings.Contains(err.Error(), "4096") {
		t.Fatalf("curve point cap %v", err)
	}
	text.Reset()
	text.WriteString("max 255\nord 9\n")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&text, "%d %g\n", i*25, float64(i)+.05)
	}
	if err := os.WriteFile(curve, []byte(text.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadPresentationOptions(&options{presentationOptions: presentationOptions{monitorCurve: curve, bits: 8}}); err == nil || !strings.Contains(err.Error(), "order 8") {
		t.Fatalf("curve fit order cap %v", err)
	}
	file := precisionImage(t, []uint16{0, 1, 2, 3})
	file.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "RGB"))
	meta, err := pixeldata.ExtractMetadata(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []presentationOptions{{bits: 16}, {overlays: true}, {presentation: true}, {autoWindow: "min-max"}} {
		if _, err := renderFrame(file.Dataset, meta, []byte{0, 1, 2, 3, 4, 5, 6, 7}, 0, binary.LittleEndian, options{presentationOptions: p}); err == nil || !strings.Contains(err.Error(), "color") {
			t.Fatalf("color option %+v: %v", p, err)
		}
	}
}
