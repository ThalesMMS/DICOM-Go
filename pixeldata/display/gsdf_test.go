package display

import (
	"errors"
	"image"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestGSDFEndpointsAndInverse(t *testing.T) {
	// PS3.14 Table B-1: JND 1 is 0.0500 cd/m², JND 1023 is 3993.4 cd/m².
	if got := GSDFLuminance(1); math.Abs(got-0.05) > 0.0005 {
		t.Fatalf("L(1) = %v", got)
	}
	if got := GSDFLuminance(1023); math.Abs(got-3993.4) > 1 {
		t.Fatalf("L(1023) = %v", got)
	}
	for j := 1.0; j <= 1023; j += 7 {
		if back := GSDFJNDIndex(GSDFLuminance(j)); math.Abs(back-j) > 0.5 {
			t.Fatalf("j(L(%v)) = %v", j, back)
		}
	}
	for j := 2.0; j <= 1023; j++ {
		if GSDFLuminance(j) <= GSDFLuminance(j-1) {
			t.Fatalf("GSDF not increasing at %v", j)
		}
	}
}

const testMonitorCurve = `# test monitor
  max 255
  amb 0.5
# DDL lum
    0  0.5
  128  60
  255  250
`

func TestParseCharacteristicCurve(t *testing.T) {
	curve, err := ParseCharacteristicCurve(strings.NewReader(testMonitorCurve), DeviceMonitor)
	if err != nil {
		t.Fatal(err)
	}
	if curve.MaxDDL != 255 || curve.AmbientLight != 0.5 || len(curve.DDL) != 3 || curve.Values[1] != 60 {
		t.Fatalf("curve = %+v", curve)
	}
	values := curve.Interpolated()
	if len(values) != 256 || values[0] != 0.5 || values[128] != 60 || values[255] != 250 {
		t.Fatalf("spline misses its points: %v %v %v", values[0], values[128], values[255])
	}
	for _, bad := range []string{
		"max 255\n0 1\n",               // one point
		"0 1\n255 2\n",                 // no max
		"max 255\n10 1\n5 2\n",         // out of order
		"max 255\n0 1\n300 2\n",        // beyond max
		"max 255\nfoo 1\n0 1\n255 2\n", // unknown key
		"max 255\n0 -1\n255 2\n",       // negative
		"max 255\nord 3\n0 1\n255 2\n", // order needs points
	} {
		if _, err := ParseCharacteristicCurve(strings.NewReader(bad), DeviceMonitor); !errors.Is(err, ErrInvalidCharacteristicCurve) {
			t.Fatalf("%q: err = %v", bad, err)
		}
	}
}

func TestPolynomialFitReproducesPolynomial(t *testing.T) {
	curve := &CharacteristicCurve{Type: DevicePrinter, MaxDDL: 255, PolynomialOrder: 2}
	for ddl := 0; ddl <= 255; ddl += 15 {
		x := float64(ddl) / 255
		curve.DDL = append(curve.DDL, ddl)
		curve.Values = append(curve.Values, 3-2*x+0.5*x*x)
	}
	values := curve.Interpolated()
	for ddl, value := range values {
		x := float64(ddl) / 255
		if want := 3 - 2*x + 0.5*x*x; math.Abs(value-want) > 1e-9 {
			t.Fatalf("DDL %d: %v, want %v", ddl, value, want)
		}
	}
}

// A monitor already on the GSDF needs the identity LUT.
func TestGSDFLUTIdentityForConformantMonitor(t *testing.T) {
	jMin, jMax := GSDFJNDIndex(0.5), GSDFJNDIndex(300)
	curve := &CharacteristicCurve{Type: DeviceMonitor, MaxDDL: 255}
	for ddl := 0; ddl <= 255; ddl++ {
		curve.DDL = append(curve.DDL, ddl)
		curve.Values = append(curve.Values, GSDFLuminance(jMin+float64(ddl)*(jMax-jMin)/255))
	}
	lut, err := DisplayCalibration{Curve: curve, AmbientLight: -1}.GSDFLUT(8)
	if err != nil {
		t.Fatal(err)
	}
	for p, ddl := range lut.Entries {
		if int(ddl) != p {
			t.Fatalf("P-value %d -> DDL %d", p, ddl)
		}
	}
}

func TestGSDFLUTMonotonicWithAmbient(t *testing.T) {
	curve, err := ParseCharacteristicCurve(strings.NewReader(testMonitorCurve), DeviceMonitor)
	if err != nil {
		t.Fatal(err)
	}
	lut, err := DisplayCalibration{Curve: curve}.GSDFLUT(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lut.Entries) != 1024 || lut.Entries[0] != 0 || lut.Entries[1023] != 255 {
		t.Fatalf("endpoints %d %d", lut.Entries[0], lut.Entries[1023])
	}
	if lut.MinLuminance != 1 || lut.MaxLuminance != 250.5 {
		t.Fatalf("luminance range %v..%v, want ambient added", lut.MinLuminance, lut.MaxLuminance)
	}
	for p := 1; p < len(lut.Entries); p++ {
		if lut.Entries[p] < lut.Entries[p-1] {
			t.Fatalf("not monotonic at %d", p)
		}
	}
	// On a nearly linear monitor the GSDF spends half the P-values below
	// the middle DDL (perceptual steps are finer in the dark).
	if lut.Map(512) >= 128 {
		t.Fatalf("mid P-value maps to DDL %d", lut.Map(512))
	}
	if _, err := (DisplayCalibration{Curve: curve}).GSDFLUT(17); err == nil {
		t.Fatal("17 input bits accepted")
	}
}

func TestGSDFLUTPrinterDensityRange(t *testing.T) {
	curve := &CharacteristicCurve{Type: DevicePrinter, MaxDDL: 255, DDL: []int{0, 255}, Values: []float64{3.2, 0.1}}
	full, err := DisplayCalibration{Curve: curve}.GSDFLUT(8)
	if err != nil {
		t.Fatal(err)
	}
	limited, err := DisplayCalibration{Curve: curve, MinDensity: 0.5, MaxDensity: 2.5}.GSDFLUT(8)
	if err != nil {
		t.Fatal(err)
	}
	if full.Entries[0] != 0 || full.Entries[255] != 255 {
		t.Fatalf("full range endpoints %d %d", full.Entries[0], full.Entries[255])
	}
	if limited.Entries[0] == 0 || limited.Entries[255] == 255 {
		t.Fatalf("density limits ignored: %d %d", limited.Entries[0], limited.Entries[255])
	}
	// Reflected ambient light defaults to 10 cd/m² for printers.
	if want := 10 + 2000*math.Pow(10, -3.2); math.Abs(full.MinLuminance-want) > 1e-9 {
		t.Fatalf("min luminance %v, want %v", full.MinLuminance, want)
	}
}

func TestApplyGrayScalesDDL(t *testing.T) {
	lut := &DisplayLUT{InputBits: 8, MaxDDL: 1020, Entries: make([]uint16, 256)}
	for p := range lut.Entries {
		lut.Entries[p] = uint16(p * 4)
	}
	img := image.NewGray(image.Rect(0, 0, 2, 1))
	img.Pix[0], img.Pix[1] = 0, 255
	if err := lut.ApplyGray(img); err != nil {
		t.Fatal(err)
	}
	if img.Pix[0] != 0 || img.Pix[1] != 255 {
		t.Fatalf("pix = %v", img.Pix)
	}
	wide := &DisplayLUT{InputBits: 10, MaxDDL: 255, Entries: make([]uint16, 1024)}
	if err := wide.ApplyGray(img); err == nil {
		t.Fatal("10-bit LUT applied to 8-bit image")
	}
	gray16 := image.NewGray16(image.Rect(0, 0, 1, 1))
	gray16.Pix[0], gray16.Pix[1] = 0xFF, 0xFF
	for p := range wide.Entries {
		wide.Entries[p] = uint16(p / 4)
	}
	out := wide.ApplyGray16(gray16)
	if got := uint16(out.Pix[0])<<8 | uint16(out.Pix[1]); got != 65535 {
		t.Fatalf("16-bit DDL = %d", got)
	}
}

func TestLinODPresentationLUTIsLinearInDensity(t *testing.T) {
	density := PrintDensity{MinDensity: 0.2, MaxDensity: 3.0, Illumination: 2000, ReflectedAmbientLight: 10}
	lut, err := density.LinODPresentationLUT(8)
	if err != nil {
		t.Fatal(err)
	}
	if lut.Entries[0] != 65535 || lut.Entries[255] != 0 || lut.BitsPerEntry != 16 {
		t.Fatalf("endpoints %d %d", lut.Entries[0], lut.Entries[255])
	}
	for i, p := range lut.Entries {
		want := 0.2 + float64(i)*2.8/255
		if got := density.OpticalDensity(int(p), 65535); math.Abs(got-want) > 0.002 {
			t.Fatalf("input %d: OD %v, want %v", i, got, want)
		}
	}
}

func TestPresentationFromObjectLinOD(t *testing.T) {
	obj := object.FromElements([]core.Element{
		core.NewRawElement(tagPresentationLUTShape, core.VRCS, []byte("LIN OD")),
		core.NewRawElement(tagMinDensity, core.VRUS, []byte{10, 0}),
		core.NewRawElement(tagMaxDensity, core.VRUS, []byte{0x2C, 0x01}),
		core.NewRawElement(tagIllumination, core.VRUS, []byte{0xD0, 0x07}),
		core.NewRawElement(tagReflectedAmbientLight, core.VRUS, []byte{5, 0}),
	}, nil)
	density := PrintDensityFromObject(obj)
	if density != (PrintDensity{MinDensity: 0.1, MaxDensity: 3, Illumination: 2000, ReflectedAmbientLight: 5}) {
		t.Fatalf("density = %+v", density)
	}
	pres, err := PresentationFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !pres.LinOD || pres.LUT == nil || len(pres.LUT.Entries) != 256 {
		t.Fatalf("presentation = %+v", pres)
	}
	if got := PrintDensityFromObject(nil); got != DefaultPrintDensity {
		t.Fatalf("defaults = %+v", got)
	}
}

func TestRenderGray16MatchesRenderGray(t *testing.T) {
	frame := Frame{Rows: 1, Columns: 256, Format: PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11}, VOI: VOILUT{Center: 2048, Width: 4096}}
	frame.Pixels = make([]byte, 512)
	for i := 0; i < 256; i++ {
		v := uint16(i * 16)
		frame.Pixels[2*i], frame.Pixels[2*i+1] = uint8(v), uint8(v>>8)
	}
	gray, err := RenderGray(frame)
	if err != nil {
		t.Fatal(err)
	}
	gray16, err := RenderGray16(frame)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		wide := uint16(gray16.Pix[2*i])<<8 | uint16(gray16.Pix[2*i+1])
		if d := int(math.Round(float64(wide)/257)) - int(gray.Pix[i]); d < -1 || d > 1 {
			t.Fatalf("pixel %d: 16-bit %d vs 8-bit %d", i, wide, gray.Pix[i])
		}
	}
	// 16 bits keep distinctions 8 bits lose.
	distinct := map[uint16]bool{}
	for i := 0; i < 256; i++ {
		distinct[uint16(gray16.Pix[2*i])<<8|uint16(gray16.Pix[2*i+1])] = true
	}
	if len(distinct) != 256 {
		t.Fatalf("%d distinct 16-bit values", len(distinct))
	}
}

func TestPresentationApplyGray16(t *testing.T) {
	img := image.NewGray16(image.Rect(0, 0, 3, 1))
	for i, v := range []uint16{0, 32768, 65535} {
		img.Pix[2*i], img.Pix[2*i+1] = uint8(v>>8), uint8(v)
	}
	Presentation{Invert: true, Shutter: &Shutter{Rectangle: &RectShutter{Left: 1, Right: 2, Upper: 1, Lower: 1}, PresentationPValue: 1234, PresentationPValueDefined: true}}.ApplyGray16(img)
	got := []uint16{}
	for i := 0; i < 3; i++ {
		got = append(got, uint16(img.Pix[2*i])<<8|uint16(img.Pix[2*i+1]))
	}
	if got[0] != 65535 || got[1] != 32767 || got[2] != 1234 {
		t.Fatalf("pixels = %v", got)
	}
}

func TestLinODPresentationLUTForNeedsRange(t *testing.T) {
	if _, err := DefaultPrintDensity.LinODPresentationLUTFor(nil, 8); !errors.Is(err, ErrUnsupportedPresentationLUT) {
		t.Fatalf("err = %v", err)
	}
	curve := &CharacteristicCurve{Type: DevicePrinter, MaxDDL: 255, DDL: []int{0, 255}, Values: []float64{3.2, 0.1}}
	lut, err := DisplayCalibration{Curve: curve}.GSDFLUT(8)
	if err != nil {
		t.Fatal(err)
	}
	presentation, err := DefaultPrintDensity.LinODPresentationLUTFor(lut, 8)
	if err != nil {
		t.Fatal(err)
	}
	// The device reaches past 0.20..3.00 OD, so the P-values stay inside.
	if presentation.Entries[0] == 65535 || presentation.Entries[255] == 0 {
		t.Fatalf("endpoints %d %d", presentation.Entries[0], presentation.Entries[255])
	}
}
