package display

import (
	"bufio"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ErrInvalidCharacteristicCurve reports a device characteristic curve that
// cannot calibrate a display.
var ErrInvalidCharacteristicCurve = errors.New("dicom/display: invalid characteristic curve")

// GSDF limits (PS3.14 7.1): JND indices 1..1023 cover 0.05 to 3993.4 cd/m².
const (
	GSDFMinJND = 1
	GSDFMaxJND = 1023
)

// Default print viewing conditions (PS3.3 C.13.5): a light box of 2000 cd/m²
// and 10 cd/m² reflected ambient light.
const (
	DefaultIllumination          = 2000
	DefaultReflectedAmbientLight = 10
)

// GSDFLuminance returns the luminance in cd/m² of JND index j of the
// Grayscale Standard Display Function (PS3.14 Equation 1).
func GSDFLuminance(j float64) float64 {
	const (
		a = -1.3011877
		b = -2.5840191e-2
		c = 8.0242636e-2
		d = -1.0320229e-1
		e = 1.3646699e-1
		f = 2.8745620e-2
		g = -2.5468404e-2
		h = -3.1978977e-3
		k = 1.2992634e-4
		m = 1.3635334e-3
	)
	x := math.Log(j)
	x2 := x * x
	x3 := x2 * x
	x4 := x3 * x
	x5 := x4 * x
	return math.Pow(10, (a+c*x+e*x2+g*x3+m*x4)/(1+b*x+d*x2+f*x3+h*x4+k*x5))
}

// GSDFJNDIndex returns the JND index of luminance l in cd/m² (PS3.14
// Equation 2, the standard's polynomial inverse of GSDFLuminance, accurate to
// about half a JND).
func GSDFJNDIndex(l float64) float64 {
	coefficients := [...]float64{71.498068, 94.593053, 41.912053, 9.8247004, 0.28175407, -1.1878455, -0.18014349, 0.14710899, -0.017046845}
	x := math.Log10(l)
	out, power := 0.0, 1.0
	for _, coefficient := range coefficients {
		out += coefficient * power
		power *= x
	}
	return out
}

// DeviceType selects how a characteristic curve's values are read.
type DeviceType int

const (
	// DeviceMonitor curves hold luminance in cd/m² per DDL.
	DeviceMonitor DeviceType = iota
	// DevicePrinter curves hold optical density per DDL (film, paper or a
	// scanner's view of it); luminance comes from Illumination and Reflected
	// Ambient Light.
	DevicePrinter
)

// CharacteristicCurve is a measured device curve: the luminance (monitor) or
// optical density (printer) of some Digital Driving Levels, 0..MaxDDL. The
// text form is the common calibration file layout: "#" comments, "max",
// optional "amb" (ambient or reflected ambient light), "lum" (printer
// illumination) and "ord" (polynomial fitting order), then "DDL value" pairs.
type CharacteristicCurve struct {
	Type   DeviceType
	MaxDDL int
	DDL    []int
	Values []float64
	// AmbientLight and Illumination are the file's defaults (0 when absent).
	AmbientLight float64
	Illumination float64
	// PolynomialOrder, when positive, fits a least-squares polynomial of this
	// order to the points; otherwise a natural cubic spline interpolates them.
	PolynomialOrder int
}

// ParseCharacteristicCurve reads a characteristic curve in the text layout
// described on CharacteristicCurve.
func ParseCharacteristicCurve(r io.Reader, typ DeviceType) (*CharacteristicCurve, error) {
	curve := &CharacteristicCurve{Type: typ, MaxDDL: -1}
	scanner := bufio.NewScanner(r)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("%w: line %d: want two fields", ErrInvalidCharacteristicCurve, line)
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("%w: line %d: bad value %q", ErrInvalidCharacteristicCurve, line, fields[1])
		}
		switch strings.ToLower(fields[0]) {
		case "max":
			curve.MaxDDL = int(value)
		case "amb":
			curve.AmbientLight = value
		case "lum":
			curve.Illumination = value
		case "ord":
			curve.PolynomialOrder = int(value)
		default:
			ddl, err := strconv.Atoi(fields[0])
			if err != nil {
				return nil, fmt.Errorf("%w: line %d: unknown key %q", ErrInvalidCharacteristicCurve, line, fields[0])
			}
			curve.DDL = append(curve.DDL, ddl)
			curve.Values = append(curve.Values, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := curve.Validate(); err != nil {
		return nil, err
	}
	return curve, nil
}

// Validate checks the curve's range and points.
func (c *CharacteristicCurve) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: nil curve", ErrInvalidCharacteristicCurve)
	}
	if c.MaxDDL < 1 || c.MaxDDL > 65535 {
		return fmt.Errorf("%w: maximum DDL %d outside 1..65535", ErrInvalidCharacteristicCurve, c.MaxDDL)
	}
	if len(c.DDL) != len(c.Values) || len(c.DDL) < 2 {
		return fmt.Errorf("%w: need at least two DDL/value pairs", ErrInvalidCharacteristicCurve)
	}
	for i, ddl := range c.DDL {
		if ddl < 0 || ddl > c.MaxDDL || (i > 0 && ddl <= c.DDL[i-1]) {
			return fmt.Errorf("%w: DDL %d out of order or range", ErrInvalidCharacteristicCurve, ddl)
		}
		if c.Values[i] < 0 {
			return fmt.Errorf("%w: negative value at DDL %d", ErrInvalidCharacteristicCurve, ddl)
		}
	}
	if c.PolynomialOrder < 0 || (c.PolynomialOrder > 0 && c.PolynomialOrder >= len(c.DDL)) {
		return fmt.Errorf("%w: polynomial order %d needs more points", ErrInvalidCharacteristicCurve, c.PolynomialOrder)
	}
	return nil
}

// Interpolated returns the curve value of every DDL 0..MaxDDL, from the
// fitted polynomial or a natural cubic spline through the points (clamped to
// the end values outside the measured range).
func (c *CharacteristicCurve) Interpolated() []float64 {
	out := make([]float64, c.MaxDDL+1)
	xs := make([]float64, len(c.DDL))
	for i, ddl := range c.DDL {
		xs[i] = float64(ddl)
	}
	if c.PolynomialOrder > 0 {
		coefficients := polynomialFit(xs, c.Values, c.PolynomialOrder, float64(c.MaxDDL))
		for ddl := range out {
			x := float64(ddl) / float64(c.MaxDDL)
			value, power := 0.0, 1.0
			for _, coefficient := range coefficients {
				value += coefficient * power
				power *= x
			}
			out[ddl] = math.Max(0, value)
		}
		return out
	}
	second := naturalSpline(xs, c.Values)
	for ddl := range out {
		x := float64(ddl)
		switch {
		case x <= xs[0]:
			out[ddl] = c.Values[0]
		case x >= xs[len(xs)-1]:
			out[ddl] = c.Values[len(xs)-1]
		default:
			i := sort.SearchFloat64s(xs, x)
			if xs[i] == x {
				out[ddl] = c.Values[i]
				continue
			}
			lo, hi := i-1, i
			span := xs[hi] - xs[lo]
			a := (xs[hi] - x) / span
			b := (x - xs[lo]) / span
			out[ddl] = math.Max(0, a*c.Values[lo]+b*c.Values[hi]+((a*a*a-a)*second[lo]+(b*b*b-b)*second[hi])*span*span/6)
		}
	}
	return out
}

// naturalSpline returns the second derivatives of the natural cubic spline
// through (xs, ys).
func naturalSpline(xs, ys []float64) []float64 {
	n := len(xs)
	second := make([]float64, n)
	u := make([]float64, n)
	for i := 1; i < n-1; i++ {
		sig := (xs[i] - xs[i-1]) / (xs[i+1] - xs[i-1])
		p := sig*second[i-1] + 2
		second[i] = (sig - 1) / p
		slope := (ys[i+1]-ys[i])/(xs[i+1]-xs[i]) - (ys[i]-ys[i-1])/(xs[i]-xs[i-1])
		u[i] = (6*slope/(xs[i+1]-xs[i-1]) - sig*u[i-1]) / p
	}
	second[n-1] = 0
	for i := n - 2; i >= 0; i-- {
		second[i] = second[i]*second[i+1] + u[i]
	}
	return second
}

// polynomialFit returns the least-squares polynomial coefficients (lowest
// order first) of ys over xs scaled by 1/scale, solved with the normal
// equations by Gaussian elimination with partial pivoting.
func polynomialFit(xs, ys []float64, order int, scale float64) []float64 {
	n := order + 1
	matrix := make([][]float64, n)
	for i := range matrix {
		matrix[i] = make([]float64, n+1)
	}
	for k, x := range xs {
		x /= scale
		powers := make([]float64, 2*n)
		powers[0] = 1
		for p := 1; p < len(powers); p++ {
			powers[p] = powers[p-1] * x
		}
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				matrix[i][j] += powers[i+j]
			}
			matrix[i][n] += powers[i] * ys[k]
		}
	}
	for col := 0; col < n; col++ {
		pivot := col
		for row := col + 1; row < n; row++ {
			if math.Abs(matrix[row][col]) > math.Abs(matrix[pivot][col]) {
				pivot = row
			}
		}
		matrix[col], matrix[pivot] = matrix[pivot], matrix[col]
		if matrix[col][col] == 0 {
			continue
		}
		for row := 0; row < n; row++ {
			if row == col {
				continue
			}
			factor := matrix[row][col] / matrix[col][col]
			for j := col; j <= n; j++ {
				matrix[row][j] -= factor * matrix[col][j]
			}
		}
	}
	out := make([]float64, n)
	for i := range out {
		if matrix[i][i] != 0 {
			out[i] = matrix[i][n] / matrix[i][i]
		}
	}
	return out
}

// DisplayCalibration describes a device and its viewing conditions.
type DisplayCalibration struct {
	Curve *CharacteristicCurve
	// AmbientLight is the ambient light added to a monitor's measured
	// luminance, or the reflected ambient light of a print, in cd/m². Zero
	// uses the curve's value, else 0 for monitors and
	// DefaultReflectedAmbientLight for printers; a negative value means none.
	AmbientLight float64
	// Illumination is a print's light box luminance in cd/m² (printers
	// only); zero uses the curve's value or DefaultIllumination.
	Illumination float64
	// MinDensity and MaxDensity restrict a printer's density range in
	// optical density; zero uses the device range.
	MinDensity float64
	MaxDensity float64
}

func (c DisplayCalibration) ambient() float64 {
	switch {
	case c.AmbientLight > 0:
		return c.AmbientLight
	case c.AmbientLight < 0:
		return 0
	case c.Curve != nil && c.Curve.AmbientLight > 0:
		return c.Curve.AmbientLight
	case c.Curve != nil && c.Curve.Type == DevicePrinter:
		return DefaultReflectedAmbientLight
	}
	return 0
}

func (c DisplayCalibration) illumination() float64 {
	if c.Illumination > 0 {
		return c.Illumination
	}
	if c.Curve != nil && c.Curve.Illumination > 0 {
		return c.Curve.Illumination
	}
	return DefaultIllumination
}

// Luminances returns the luminance of every DDL 0..MaxDDL under the viewing
// conditions: measured luminance plus ambient light for monitors, and
// ambient + illumination * 10^-OD for printers.
func (c DisplayCalibration) Luminances() ([]float64, error) {
	if err := c.Curve.Validate(); err != nil {
		return nil, err
	}
	values := c.Curve.Interpolated()
	out := make([]float64, len(values))
	for ddl, value := range values {
		out[ddl] = c.luminance(value)
	}
	return out, nil
}

// luminance converts a curve value (luminance or optical density) to the
// luminance seen under the viewing conditions.
func (c DisplayCalibration) luminance(value float64) float64 {
	if c.Curve.Type == DevicePrinter {
		return c.ambient() + c.illumination()*math.Pow(10, -value)
	}
	return value + c.ambient()
}

// luminanceRange returns the luminance range a calibration spans: the
// measured minimum and maximum under the viewing conditions, restricted for
// printers by MinDensity and MaxDensity.
func (c DisplayCalibration) luminanceRange() (low, high float64, err error) {
	low, high = math.Inf(1), math.Inf(-1)
	for _, value := range c.Curve.Values {
		l := c.luminance(value)
		low, high = math.Min(low, l), math.Max(high, l)
	}
	if c.Curve.Type == DevicePrinter {
		if c.MaxDensity > 0 {
			low = math.Max(low, c.ambient()+c.illumination()*math.Pow(10, -c.MaxDensity))
		}
		if c.MinDensity > 0 {
			high = math.Min(high, c.ambient()+c.illumination()*math.Pow(10, -c.MinDensity))
		}
	}
	if !(high > low) || low <= 0 {
		return 0, 0, fmt.Errorf("%w: luminance range %.4g..%.4g cd/m²", ErrInvalidCharacteristicCurve, low, high)
	}
	return low, high, nil
}

// DisplayFunction is the perceptual scale a DisplayLUT linearizes.
type DisplayFunction int

const (
	// FunctionGSDF spaces P-values equally in just-noticeable differences of
	// the Grayscale Standard Display Function (PS3.14).
	FunctionGSDF DisplayFunction = iota
	// FunctionCIELAB spaces P-values equally in CIE 1976 lightness L*.
	FunctionCIELAB
)

// String names the function as settings and diagnostics show it.
func (f DisplayFunction) String() string {
	if f == FunctionCIELAB {
		return "CIELAB"
	}
	return "GSDF"
}

// DisplayLUT maps P-values (PS3.14: perceptually linear presentation values)
// to the Digital Driving Levels that display them on a calibrated device,
// following the Grayscale Standard Display Function or CIELAB lightness.
type DisplayLUT struct {
	Function  DisplayFunction
	InputBits int
	MaxDDL    int
	// Entries holds the DDL of every P-value 0..2^InputBits-1.
	Entries []uint16
	// Luminance range and JND range the device reproduces.
	MinLuminance, MaxLuminance float64
	MinJND, MaxJND             float64
	// MinLightness and MaxLightness are the L* range of a CIELAB LUT: the
	// minimum luminance relative to the maximum, and 100.
	MinLightness, MaxLightness float64
}

// LUT returns the display LUT of the given function.
func (c DisplayCalibration) LUT(function DisplayFunction, inputBits int) (*DisplayLUT, error) {
	switch function {
	case FunctionGSDF:
		return c.GSDFLUT(inputBits)
	case FunctionCIELAB:
		return c.CIELABLUT(inputBits)
	}
	return nil, fmt.Errorf("%w: unknown display function %d", ErrInvalidCharacteristicCurve, function)
}

// GSDFLUT returns the display LUT that makes P-values 0..2^inputBits-1 equally
// spaced in JND index between the device's minimum and maximum measured
// luminance, each shown with the DDL whose interpolated luminance is closest.
// This reproduces DCMTK's dcm2img +Dm/+Dp output for its sample curves.
func (c DisplayCalibration) GSDFLUT(inputBits int) (*DisplayLUT, error) {
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
	jMin := clampFloat(GSDFJNDIndex(low), GSDFMinJND, GSDFMaxJND)
	jMax := clampFloat(GSDFJNDIndex(high), GSDFMinJND, GSDFMaxJND)
	count := 1 << inputBits
	lut := &DisplayLUT{Function: FunctionGSDF, InputBits: inputBits, MaxDDL: c.Curve.MaxDDL, Entries: make([]uint16, count), MinLuminance: low, MaxLuminance: high, MinJND: jMin, MaxJND: jMax}
	// Sort DDLs by luminance for a binary search; ties keep the lower DDL.
	order := make([]int, len(luminances))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return luminances[order[i]] < luminances[order[j]] })
	for p := 0; p < count; p++ {
		target := GSDFLuminance(jMin + float64(p)*(jMax-jMin)/float64(count-1))
		i := sort.Search(len(order), func(i int) bool { return luminances[order[i]] >= target })
		best := order[min(i, len(order)-1)]
		if i > 0 && (i == len(order) || target-luminances[order[i-1]] <= luminances[order[i]]-target) {
			best = order[i-1]
		}
		lut.Entries[p] = uint16(best)
	}
	return lut, nil
}

// Map returns the DDL of a P-value (clamped to the input range).
func (l *DisplayLUT) Map(p int) uint16 {
	if l == nil || len(l.Entries) == 0 {
		return 0
	}
	return l.Entries[max(0, min(p, len(l.Entries)-1))]
}

// ApplyGray maps an 8-bit P-value image to device driving levels in place,
// scaled to 0..255 when the device has another DDL range. The LUT must have
// 8 input bits.
func (l *DisplayLUT) ApplyGray(img *image.Gray) error {
	if l == nil || img == nil {
		return nil
	}
	if l.InputBits != 8 {
		return fmt.Errorf("%w: ApplyGray needs an 8-bit LUT, have %d bits", ErrInvalidCharacteristicCurve, l.InputBits)
	}
	var table [256]uint8
	for p := range table {
		table[p] = uint8(scaleDDL(l.Entries[p], l.MaxDDL, 255))
	}
	for i, value := range img.Pix {
		img.Pix[i] = table[value]
	}
	return nil
}

// ApplyGray16 returns the DDLs of a 16-bit P-value image, scaled to
// 0..65535. The input is reduced to the LUT's input bits first.
func (l *DisplayLUT) ApplyGray16(img *image.Gray16) *image.Gray16 {
	if l == nil || img == nil {
		return img
	}
	bounds := img.Bounds()
	out := image.NewGray16(bounds)
	shift := uint(16 - l.InputBits)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			i := img.PixOffset(x, y)
			p := int(uint16(img.Pix[i])<<8|uint16(img.Pix[i+1])) >> shift
			ddl := scaleDDL(l.Entries[p], l.MaxDDL, 65535)
			o := out.PixOffset(x, y)
			out.Pix[o], out.Pix[o+1] = uint8(ddl>>8), uint8(ddl)
		}
	}
	return out
}

func scaleDDL(ddl uint16, maxDDL, maxOut int) int {
	if maxDDL == maxOut || maxDDL <= 0 {
		return int(ddl)
	}
	return int(math.Round(float64(ddl) * float64(maxOut) / float64(maxDDL)))
}

// PrintDensity are the print viewing conditions of Presentation LUT Shape
// LIN OD (PS3.3 C.11.6.1.2) and the Basic Film Box: optical densities and
// light levels in cd/m².
type PrintDensity struct {
	// MinDensity and MaxDensity default to 0.20 and 3.00 OD when unset or
	// inconsistent, and Illumination to DefaultIllumination.
	MinDensity   float64
	MaxDensity   float64
	Illumination float64
	// ReflectedAmbientLight is the ambient light reflected by the film; a
	// negative value uses DefaultReflectedAmbientLight.
	ReflectedAmbientLight float64
}

// DefaultPrintDensity is the typical film range with the default viewing
// conditions: 0.20 to 3.00 OD, 2000 cd/m², 10 cd/m².
var DefaultPrintDensity = PrintDensity{MinDensity: 0.20, MaxDensity: 3.00, Illumination: DefaultIllumination, ReflectedAmbientLight: DefaultReflectedAmbientLight}

func (d PrintDensity) normalized() PrintDensity {
	if d.Illumination <= 0 {
		d.Illumination = DefaultIllumination
	}
	if d.ReflectedAmbientLight < 0 {
		d.ReflectedAmbientLight = DefaultReflectedAmbientLight
	}
	if d.MaxDensity <= 0 {
		d.MaxDensity = DefaultPrintDensity.MaxDensity
	}
	if d.MinDensity < 0 || d.MinDensity >= d.MaxDensity {
		d.MinDensity = DefaultPrintDensity.MinDensity
	}
	return d
}

// Luminance returns the luminance of optical density od under d.
func (d PrintDensity) Luminance(od float64) float64 {
	d = d.normalized()
	return d.ReflectedAmbientLight + d.Illumination*math.Pow(10, -od)
}

// LinODPresentationLUT returns the Presentation LUT of shape LIN OD for
// inputBits-wide input: input values are proportional to optical density,
// from MinDensity (input 0) to MaxDensity (maximum input), and each entry is
// the 16-bit P-value that the GSDF displays at that density's luminance, with
// the P-value range spanning MinDensity..MaxDensity (the Film Box density
// range). Larger inputs therefore print darker, as in DCMTK.
func (d PrintDensity) LinODPresentationLUT(inputBits int) (*LUT, error) {
	d = d.normalized()
	return d.linODLUT(inputBits, GSDFJNDIndex(d.Luminance(d.MaxDensity)), GSDFJNDIndex(d.Luminance(d.MinDensity)))
}

// LinODPresentationLUTFor is LinODPresentationLUT for a calibrated printer
// whose P-values span the display LUT's own JND range, so densities outside
// MinDensity..MaxDensity of the device remain reachable (dcm2img +Pod with a
// printer curve).
func (d PrintDensity) LinODPresentationLUTFor(display *DisplayLUT, inputBits int) (*LUT, error) {
	if display == nil || !(display.MaxJND > display.MinJND) {
		return nil, fmt.Errorf("%w: display LUT without a JND range", ErrUnsupportedPresentationLUT)
	}
	return d.normalized().linODLUT(inputBits, display.MinJND, display.MaxJND)
}

func (d PrintDensity) linODLUT(inputBits int, jMin, jMax float64) (*LUT, error) {
	if inputBits < 1 || inputBits > 16 {
		return nil, fmt.Errorf("%w: input bits %d outside 1..16", ErrUnsupportedPresentationLUT, inputBits)
	}
	count := 1 << inputBits
	entries := make([]uint16, count)
	for i := range entries {
		od := d.MinDensity + float64(i)*(d.MaxDensity-d.MinDensity)/float64(count-1)
		unit := (GSDFJNDIndex(d.Luminance(od)) - jMin) / (jMax - jMin)
		entries[i] = uint16(math.Round(clampFloat(unit, 0, 1) * 65535))
	}
	return NewLUT([]int{count & 0xFFFF, 0, 16}, entries)
}

// OpticalDensity returns the optical density a P-value (0..maxP) is printed
// at on a GSDF-calibrated printer under d.
func (d PrintDensity) OpticalDensity(p, maxP int) float64 {
	d = d.normalized()
	if maxP <= 0 {
		return d.MaxDensity
	}
	jMin := GSDFJNDIndex(d.Luminance(d.MaxDensity))
	jMax := GSDFJNDIndex(d.Luminance(d.MinDensity))
	l := GSDFLuminance(jMin + float64(p)*(jMax-jMin)/float64(maxP))
	return -math.Log10((l - d.ReflectedAmbientLight) / d.Illumination)
}
