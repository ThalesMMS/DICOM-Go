// Package dicomcolor converts between DICOM PCS colors and display RGB.
package dicomcolor

import "math"

const (
	d50WhiteX = 3457.0 / 3585.0
	d50WhiteZ = 2958.0 / 3585.0
)

// RGBToCIELab encodes an 8-bit sRGB color as DICOM PCS-encoded CIELab.
func RGBToCIELab(rgb [3]int) [3]uint16 {
	r := displaySRGBToLinear(float64(clampDisplayByte(rgb[0])) / 255)
	g := displaySRGBToLinear(float64(clampDisplayByte(rgb[1])) / 255)
	b := displaySRGBToLinear(float64(clampDisplayByte(rgb[2])) / 255)

	// CSS Color 4's linear sRGB to D65 XYZ matrix composed with the
	// Bradford D65-to-D50 adaptation. The row sums match the D50 white
	// point used for Lab so equal RGB channels stay neutral.
	// https://www.w3.org/TR/css-color-4/#color-conversion-code
	x50 := 0.43606574687427*r + 0.38515150959016*g + 0.14307841996514*b
	y50 := 0.22249317711057*r + 0.71688701309448*g + 0.06061980979495*b
	z50 := 0.01392392146317*r + 0.09708132423141*g + 0.71409935681588*b

	fx := displayLabF(x50 / d50WhiteX)
	fy := displayLabF(y50)
	fz := displayLabF(z50 / d50WhiteZ)
	return [3]uint16{
		scaleDisplayLabWord(116*fy-16, 0, 100),
		scaleDisplayLabWord(500*(fx-fy), -128, 127),
		scaleDisplayLabWord(200*(fy-fz), -128, 127),
	}
}

// CIELabToRGB converts DICOM PCS-encoded CIELab to 8-bit sRGB.
func CIELabToRGB(encoded [3]uint16) [3]int {
	l := float64(encoded[0]) * 100 / 65535
	a := float64(encoded[1])*255/65535 - 128
	b := float64(encoded[2])*255/65535 - 128
	fy := (l + 16) / 116
	fx := fy + a/500
	fz := fy - b/200
	x50 := d50WhiteX * displayLabFInverse(fx)
	y50 := displayLabFInverse(fy)
	z50 := d50WhiteZ * displayLabFInverse(fz)

	// CSS Color 4's Bradford D50-to-D65 adaptation composed with
	// the D65 XYZ to linear sRGB matrix.
	r := 3.13413585290012*x50 - 1.61738599801804*y50 - 0.49066221791110*z50
	g := -0.97879547655578*x50 + 1.91625437739599*y50 + 0.03344287339037*z50
	bl := 0.07195539255795*x50 - 0.22897675981518*y50 + 1.40538603511312*z50
	return [3]int{
		int(math.Round(255 * displayLinearToSRGB(r))),
		int(math.Round(255 * displayLinearToSRGB(g))),
		int(math.Round(255 * displayLinearToSRGB(bl))),
	}
}

func displaySRGBToLinear(value float64) float64 {
	if value <= 0.04045 {
		return value / 12.92
	}
	return math.Pow((value+0.055)/1.055, 2.4)
}

func displayLinearToSRGB(value float64) float64 {
	value = math.Max(0, math.Min(1, value))
	if value <= 0.0031308 {
		return 12.92 * value
	}
	return 1.055*math.Pow(value, 1/2.4) - 0.055
}

func displayLabF(value float64) float64 {
	const delta = 6.0 / 29.0
	if value > delta*delta*delta {
		return math.Cbrt(value)
	}
	return value/(3*delta*delta) + 4.0/29.0
}

func displayLabFInverse(value float64) float64 {
	const delta = 6.0 / 29.0
	if value > delta {
		return value * value * value
	}
	return 3 * delta * delta * (value - 4.0/29.0)
}

func scaleDisplayLabWord(value, minValue, maxValue float64) uint16 {
	value = math.Max(minValue, math.Min(maxValue, value))
	return uint16(math.Round((value - minValue) * 65535 / (maxValue - minValue)))
}

func clampDisplayByte(value int) int { return max(0, min(value, 255)) }
