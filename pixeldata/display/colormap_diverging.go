package display

import "math"

// CoolWarmLUT returns a diverging blue → neutral → red palette for signed
// data such as fusion difference images (#1069): index 0 is the most
// negative value, 128 about zero and 255 the most positive. The endpoints
// and midpoint are those of Moreland's "cool to warm" diverging map (2009);
// the ramps between them are interpolated in CIELAB, so equal index steps
// read as nearly equal colour differences on either side of the midpoint.
func CoolWarmLUT() *PaletteColorLUT {
	cool := srgb8ToLab(59, 76, 192)
	neutral := srgb8ToLab(221, 221, 221)
	warm := srgb8ToLab(180, 4, 38)
	var red, green, blue [256]uint8
	for i := range red {
		var lab [3]float64
		if t := float64(i) / 255; t <= 0.5 {
			lab = lerpLab(cool, neutral, t/0.5)
		} else {
			lab = lerpLab(neutral, warm, (t-0.5)/0.5)
		}
		red[i], green[i], blue[i] = labToSRGB8(lab)
	}
	return &PaletteColorLUT{
		Red:   uint8RampLUT(func(i int) uint8 { return red[i] }),
		Green: uint8RampLUT(func(i int) uint8 { return green[i] }),
		Blue:  uint8RampLUT(func(i int) uint8 { return blue[i] }),
	}
}

func lerpLab(a, b [3]float64, t float64) [3]float64 {
	return [3]float64{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t, a[2] + (b[2]-a[2])*t}
}

// D65 reference white.
const labWhiteX, labWhiteY, labWhiteZ = 0.95047, 1.0, 1.08883

func srgb8ToLab(r, g, b uint8) [3]float64 {
	lin := func(c uint8) float64 {
		v := float64(c) / 255
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	R, G, B := lin(r), lin(g), lin(b)
	x := (0.4124564*R + 0.3575761*G + 0.1804375*B) / labWhiteX
	y := (0.2126729*R + 0.7151522*G + 0.0721750*B) / labWhiteY
	z := (0.0193339*R + 0.1191920*G + 0.9503041*B) / labWhiteZ
	f := func(t float64) float64 {
		if t > 216.0/24389 {
			return math.Cbrt(t)
		}
		return (24389.0/27*t + 16) / 116
	}
	fx, fy, fz := f(x), f(y), f(z)
	return [3]float64{116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)}
}

func labToSRGB8(lab [3]float64) (uint8, uint8, uint8) {
	fy := (lab[0] + 16) / 116
	fx := fy + lab[1]/500
	fz := fy - lab[2]/200
	inv := func(f float64) float64 {
		if f3 := f * f * f; f3 > 216.0/24389 {
			return f3
		}
		return (116*f - 16) / (24389.0 / 27)
	}
	x, y, z := inv(fx)*labWhiteX, inv(fy)*labWhiteY, inv(fz)*labWhiteZ
	R := 3.2404542*x - 1.5371385*y - 0.4985314*z
	G := -0.9692660*x + 1.8760108*y + 0.0415560*z
	B := 0.0556434*x - 0.2040259*y + 1.0572252*z
	enc := func(v float64) uint8 {
		v = math.Max(0, math.Min(1, v))
		if v <= 0.0031308 {
			v *= 12.92
		} else {
			v = 1.055*math.Pow(v, 1/2.4) - 0.055
		}
		return uint8(math.Round(v * 255))
	}
	return enc(R), enc(G), enc(B)
}
