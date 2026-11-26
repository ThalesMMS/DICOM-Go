package display

import (
	"fmt"
	"math"
)

// CIE 1976 lightness (CIE 15) with the published constants ε = 0.008856 and
// κ = 903.3, the values DCMTK's CIELAB display function also uses.
const (
	cieEpsilon = 0.008856
	cieKappa   = 903.3
)

// CIELABLightness returns the CIE 1976 lightness L* (0..100) of a relative
// luminance Y/Yn: 116·(Y/Yn)^(1/3) − 16 above ε, κ·Y/Yn below.
func CIELABLightness(relativeLuminance float64) float64 {
	if relativeLuminance > cieEpsilon {
		return 116*math.Cbrt(relativeLuminance) - 16
	}
	return cieKappa * relativeLuminance
}

// CIELABRelativeLuminance inverts CIELABLightness: ((L*+16)/116)³ when
// L*/κ exceeds ε, L*/κ below.
func CIELABRelativeLuminance(lightness float64) float64 {
	if linear := lightness / cieKappa; linear <= cieEpsilon {
		return linear
	}
	c := (lightness + 16) / 116
	return c * c * c
}

// CIELABLUT returns the display LUT that makes P-values 0..2^inputBits-1
// equally spaced in CIE 1976 lightness between the device's minimum
// luminance (L* of Lmin/Lmax) and its maximum (L* 100): a perceptual
// linearization offered beside the PS3.14 GSDF, which the Presentation LUT's
// P-values then address the same way. Each P-value is shown with the
// DDL whose luminance is closest to its target, found by a forward search
// that assumes luminance increases with DDL and keeps the higher DDL on a
// tie. The luminance range and viewing conditions are those of GSDFLUT; for
// printers with Min/Max Density only DDLs inside that range are used. This is
// the behavior of DCMTK's CIELAB display function (dcm2img +Dc).
func (c DisplayCalibration) CIELABLUT(inputBits int) (*DisplayLUT, error) {
	if inputBits < 1 || inputBits > 16 {
		return nil, fmt.Errorf("%w: input bits %d outside 1..16", ErrInvalidCharacteristicCurve, inputBits)
	}
	luminances, err := c.Luminances()
	if err != nil {
		return nil, err
	}
	low, high, err := c.luminanceRange()
	if err != nil {
		return nil, err
	}
	lMin := CIELABLightness(low / high)
	count := 1 << inputBits
	lut := &DisplayLUT{
		Function: FunctionCIELAB, InputBits: inputBits, MaxDDL: c.Curve.MaxDDL, Entries: make([]uint16, count),
		MinLuminance: low, MaxLuminance: high,
		MinJND:       clampFloat(GSDFJNDIndex(low), GSDFMinJND, GSDFMaxJND),
		MaxJND:       clampFloat(GSDFJNDIndex(high), GSDFMinJND, GSDFMaxJND),
		MinLightness: lMin, MaxLightness: 100,
	}
	first, last := 0, len(luminances)-1
	if c.Curve.Type == DevicePrinter {
		if c.MaxDensity > 0 {
			for first < last && luminances[first] < low {
				first++
			}
		}
		if c.MinDensity > 0 {
			for last > first && luminances[last] > high {
				last--
			}
		}
	}
	ddl := first
	for p := 0; p < count; p++ {
		target := high * CIELABRelativeLuminance(lMin+float64(p)*(100-lMin)/float64(count-1))
		for ddl < last && luminances[ddl] < target {
			ddl++
		}
		if ddl > first && math.Abs(luminances[ddl-1]-target) < math.Abs(luminances[ddl]-target) {
			ddl--
		}
		lut.Entries[p] = uint16(ddl)
	}
	return lut, nil
}
