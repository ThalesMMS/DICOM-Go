package display

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestCIELABLightness(t *testing.T) {
	// CIE 1976: L* = 116 (Y/Yn)^(1/3) - 16 above 0.008856, 903.3 Y/Yn below.
	tests := []struct {
		y, want float64
	}{
		{1, 100},
		{0, 0},
		{0.18, 116*math.Cbrt(0.18) - 16}, // 49.496, photographic mid-gray
		{0.5, 116*math.Cbrt(0.5) - 16},   // 76.069
		{0.001, 0.9033},
		{0.008856, 903.3 * 0.008856},
	}
	for _, tt := range tests {
		if got := CIELABLightness(tt.y); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("L*(%v) = %v, want %v", tt.y, got, tt.want)
		}
	}
	if got := CIELABLightness(0.18); math.Abs(got-49.496) > 0.001 {
		t.Errorf("L*(0.18) = %v, want 49.496", got)
	}
	// The two branches meet within 1e-4 L* at the threshold.
	if gap := math.Abs((116*math.Cbrt(cieEpsilon) - 16) - cieKappa*cieEpsilon); gap > 1e-4 {
		t.Errorf("branch gap %v", gap)
	}
	for l := 0.0; l <= 100; l += 0.25 {
		if back := CIELABLightness(CIELABRelativeLuminance(l)); math.Abs(back-l) > 1e-3 {
			t.Fatalf("L*(Y(%v)) = %v", l, back)
		}
	}
	for l := 0.25; l <= 100; l += 0.25 {
		if CIELABRelativeLuminance(l) <= CIELABRelativeLuminance(l-0.25) {
			t.Fatalf("Y(L*) not increasing at %v", l)
		}
	}
}

// A two-point curve interpolates linearly: L(ddl) = 0.5 + ddl·249.5/255.
const linearCurve = "max 255\n0 0.5\n255 250\n"

func TestCIELABLUTSpacesPValuesEquallyInLightness(t *testing.T) {
	curve, err := ParseCharacteristicCurve(strings.NewReader(linearCurve), DeviceMonitor)
	if err != nil {
		t.Fatal(err)
	}
	for _, bits := range []int{8, 10, 12} {
		lut, err := DisplayCalibration{Curve: curve}.CIELABLUT(bits)
		if err != nil {
			t.Fatal(err)
		}
		lMin := 903.3 * 0.5 / 250 // Y/Yn 0.002 is below ε: the linear branch
		if lut.Function != FunctionCIELAB || math.Abs(lut.MinLightness-lMin) > 1e-9 || lut.MaxLightness != 100 ||
			lut.MinLuminance != 0.5 || lut.MaxLuminance != 250 {
			t.Fatalf("%d bits: %+v", bits, lut)
		}
		count := 1 << bits
		step := 249.5 / 255
		for p := 0; p < count; p++ {
			lightness := lMin + float64(p)*(100-lMin)/float64(count-1)
			target := 250 * lightness / 903.3 // dark end: L* below κε ≈ 8
			if lightness > 8 {
				c := (lightness + 16) / 116
				target = 250 * c * c * c
			}
			// Nearest DDL of the linear curve; ties take the higher one.
			x := (target - 0.5) / step
			want := int(math.Floor(x))
			if x-float64(want) >= 0.5 {
				want++
			}
			want = max(0, min(255, want))
			if got := int(lut.Entries[p]); got != want {
				t.Fatalf("%d bits: P %d (L* %.3f, %.3f cd/m²) -> DDL %d, want %d", bits, p, lightness, target, got, want)
			}
		}
		if lut.Entries[0] != 0 || lut.Entries[count-1] != 255 {
			t.Fatalf("%d bits: ends %d..%d", bits, lut.Entries[0], lut.Entries[count-1])
		}
	}
}

func TestCIELABLUTDiffersFromGSDF(t *testing.T) {
	curve, err := ParseCharacteristicCurve(strings.NewReader(testMonitorCurve), DeviceMonitor)
	if err != nil {
		t.Fatal(err)
	}
	calibration := DisplayCalibration{Curve: curve}
	gsdf, err := calibration.LUT(FunctionGSDF, 8)
	if err != nil {
		t.Fatal(err)
	}
	cielab, err := calibration.LUT(FunctionCIELAB, 8)
	if err != nil {
		t.Fatal(err)
	}
	luminances, _ := calibration.Luminances()
	// L* of every entry rises steadily: no step exceeds what one DDL near
	// that target can explain, and the entries never decrease.
	differ := 0
	for p := 1; p < 256; p++ {
		if cielab.Entries[p] < cielab.Entries[p-1] {
			t.Fatalf("CIELAB LUT decreases at %d", p)
		}
		if cielab.Entries[p] != gsdf.Entries[p] {
			differ++
		}
	}
	if differ == 0 {
		t.Fatal("CIELAB and GSDF LUTs are identical")
	}
	// Mid-scale: CIELAB targets L* 50 of the range, a brighter luminance
	// than the GSDF's JND midpoint on this curve.
	mid := luminances[cielab.Entries[128]] / cielab.MaxLuminance
	if l := CIELABLightness(mid); math.Abs(l-(cielab.MinLightness+128*(100-cielab.MinLightness)/255)) > 1.5 {
		t.Fatalf("P 128 shows L* %.2f", l)
	}
	if gsdf.Entries[128] >= cielab.Entries[128] {
		t.Fatalf("GSDF mid DDL %d, CIELAB %d", gsdf.Entries[128], cielab.Entries[128])
	}
	if cielab.Function.String() != "CIELAB" || gsdf.Function.String() != "GSDF" {
		t.Fatal("function names")
	}
	if _, err := calibration.LUT(DisplayFunction(9), 8); !errors.Is(err, ErrInvalidCharacteristicCurve) {
		t.Fatalf("unknown function: %v", err)
	}
	if _, err := calibration.CIELABLUT(17); !errors.Is(err, ErrInvalidCharacteristicCurve) {
		t.Fatalf("17 bits: %v", err)
	}
}

func TestCIELABLUTPrinterDensityRange(t *testing.T) {
	// OD falls linearly from 3.0 at DDL 0 to 0.2 at DDL 255, so luminance
	// 10 + 2000·10^-OD rises with the DDL.
	curve, err := ParseCharacteristicCurve(strings.NewReader("max 255\n0 3.0\n255 0.2\n"), DevicePrinter)
	if err != nil {
		t.Fatal(err)
	}
	luminance := func(od float64) float64 { return DefaultReflectedAmbientLight + DefaultIllumination*math.Pow(10, -od) }
	full, err := DisplayCalibration{Curve: curve}.CIELABLUT(8)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(full.MinLuminance-luminance(3)) > 1e-9 || math.Abs(full.MaxLuminance-luminance(0.2)) > 1e-9 {
		t.Fatalf("range %v..%v", full.MinLuminance, full.MaxLuminance)
	}
	if full.Entries[0] != 0 || full.Entries[255] != 255 {
		t.Fatalf("ends %d..%d", full.Entries[0], full.Entries[255])
	}
	limited, err := DisplayCalibration{Curve: curve, MinDensity: 0.5, MaxDensity: 2.5}.CIELABLUT(8)
	if err != nil {
		t.Fatal(err)
	}
	// Only DDLs printing 0.5..2.5 OD are used: DDL = (3 - OD)·255/2.8.
	lowDDL, highDDL := int(math.Ceil(0.5*255/2.8)), int(math.Floor(2.5*255/2.8))
	for p, ddl := range limited.Entries {
		if int(ddl) < lowDDL-1 || int(ddl) > highDDL+1 {
			t.Fatalf("P %d -> DDL %d outside %d..%d", p, ddl, lowDDL, highDDL)
		}
	}
	if math.Abs(limited.MinLuminance-luminance(2.5)) > 1e-9 || math.Abs(limited.MaxLuminance-luminance(0.5)) > 1e-9 {
		t.Fatalf("limited range %v..%v", limited.MinLuminance, limited.MaxLuminance)
	}
}
