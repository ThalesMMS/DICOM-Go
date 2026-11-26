package icc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/iccprofile"
)

// Synthetic test profiles. They are built here from published colorimetry
// (Adobe RGB (1998) primaries adapted to D50, a 2.2 gamma) so no third-party
// profile file is committed.

type testTag struct {
	signature string
	value     []byte
}

func buildProfile(version uint32, class, space, pcs string, tags []testTag) []byte {
	tableEnd := 128 + 4 + 12*len(tags)
	size := tableEnd
	for i := range tags {
		tags[i].value = append(tags[i].value, make([]byte, pad4(len(tags[i].value))-len(tags[i].value))...)
		size += len(tags[i].value)
	}
	out := make([]byte, size)
	binary.BigEndian.PutUint32(out[0:], uint32(size))
	binary.BigEndian.PutUint32(out[8:], version)
	copy(out[12:], class)
	copy(out[16:], space)
	copy(out[20:], pcs)
	copy(out[36:], "acsp")
	for i, v := range D50 {
		binary.BigEndian.PutUint32(out[68+4*i:], uint32(fixed(v)))
	}
	binary.BigEndian.PutUint32(out[128:], uint32(len(tags)))
	at := tableEnd
	for i, tag := range tags {
		entry := out[132+12*i:]
		copy(entry, tag.signature)
		binary.BigEndian.PutUint32(entry[4:], uint32(at))
		binary.BigEndian.PutUint32(entry[8:], uint32(len(tag.value)))
		copy(out[at:], tag.value)
		at += len(tag.value)
	}
	return out
}

func fixed(v float64) int32 { return int32(math.Round(v * 65536)) }

func xyzTag(x, y, z float64) []byte {
	out := make([]byte, 20)
	copy(out, "XYZ ")
	for i, v := range []float64{x, y, z} {
		binary.BigEndian.PutUint32(out[8+4*i:], uint32(fixed(v)))
	}
	return out
}

func gammaTag(g float64) []byte {
	out := make([]byte, 14)
	copy(out, "curv")
	binary.BigEndian.PutUint32(out[8:], 1)
	binary.BigEndian.PutUint16(out[12:], uint16(math.Round(g*256)))
	return out
}

func paraTag(kind uint16, params ...float64) []byte {
	out := make([]byte, 12+4*len(params))
	copy(out, "para")
	binary.BigEndian.PutUint16(out[8:], kind)
	for i, v := range params {
		binary.BigEndian.PutUint32(out[12+4*i:], uint32(fixed(v)))
	}
	return out
}

func descTag(text string) []byte {
	out := make([]byte, 12+len(text)+1+12+67)
	copy(out, "desc")
	binary.BigEndian.PutUint32(out[8:], uint32(len(text)+1))
	copy(out[12:], text)
	return out
}

// Adobe RGB (1998) colorants adapted to D50 and its 563/256 gamma.
var adobeColorants = [3][3]float64{
	{0.6097412, 0.2052765, 0.1491852},
	{0.3111115, 0.6256714, 0.0632172},
	{0.0194702, 0.0608673, 0.7445679},
}

const adobeGamma = 563.0 / 256

func adobeMatrixProfile() []byte {
	c := adobeColorants
	return buildProfile(0x02100000, "mntr", "RGB ", "XYZ ", []testTag{
		{"desc", descTag("Synthetic wide gamut")},
		{"wtpt", xyzTag(D50[0], D50[1], D50[2])},
		{"rXYZ", xyzTag(c[0][0], c[1][0], c[2][0])},
		{"gXYZ", xyzTag(c[0][1], c[1][1], c[2][1])},
		{"bXYZ", xyzTag(c[0][2], c[1][2], c[2][2])},
		{"rTRC", gammaTag(adobeGamma)},
		{"gTRC", gammaTag(adobeGamma)},
		{"bTRC", gammaTag(adobeGamma)},
	})
}

func adobeXYZ(r, g, b float64) [3]float64 {
	lin := [3]float64{math.Pow(r, adobeGamma), math.Pow(g, adobeGamma), math.Pow(b, adobeGamma)}
	return mul3(adobeColorants, lin)
}

func xyzToLab(xyz [3]float64) [3]float64 {
	f := func(t float64) float64 {
		if t > math.Pow(6.0/29, 3) {
			return math.Cbrt(t)
		}
		return t/(3*(6.0/29)*(6.0/29)) + 4.0/29
	}
	fx, fy, fz := f(xyz[0]/D50[0]), f(xyz[1]/D50[1]), f(xyz[2]/D50[2])
	return [3]float64{116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)}
}

// lut16LabProfile samples the wide-gamut conversion with a red/blue
// cross-talk into a 9-point lut16 to Lab (legacy 16-bit Lab encoding).
func lut16LabProfile() []byte {
	const grid = 9
	data := make([]byte, 52)
	copy(data, "mft2")
	data[8], data[9], data[10] = 3, 3, grid
	for i := range 3 {
		binary.BigEndian.PutUint32(data[12+16*i:], uint32(fixed(1)))
	}
	binary.BigEndian.PutUint16(data[48:], 2)
	binary.BigEndian.PutUint16(data[50:], 2)
	put := func(v float64) {
		v = math.Max(0, math.Min(65535, math.Round(v)))
		data = binary.BigEndian.AppendUint16(data, uint16(v))
	}
	for range 3 {
		put(0)
		put(65535)
	}
	for i := range grid {
		for j := range grid {
			for k := range grid {
				r, g, b := float64(i)/(grid-1), float64(j)/(grid-1), float64(k)/(grid-1)
				lab := xyzToLab(adobeXYZ(0.9*r+0.1*b, g, 0.95*b+0.05*r))
				put(lab[0] / 100 * 65280)
				put((lab[1] + 128) / 255 * 65280)
				put((lab[2] + 128) / 255 * 65280)
			}
		}
	}
	for range 3 {
		put(0)
		put(65535)
	}
	return buildProfile(0x02100000, "scnr", "RGB ", "Lab ", []testTag{
		{"desc", descTag("Synthetic lut16")},
		{"wtpt", xyzTag(D50[0], D50[1], D50[2])},
		{"A2B0", data},
	})
}

// lutAToBXYZProfile is a lutAToB with A curves (gamma 1.8), a 5-point
// 16-bit CLUT with cross-talk, M curves, the wide-gamut matrix and identity
// B curves, to XYZ.
func lutAToBXYZProfile() []byte {
	const grid = 5
	element := func(b []byte) []byte { return append(b, make([]byte, pad4(len(b))-len(b))...) }
	var aCurves, mCurves, bCurves []byte
	for range 3 {
		aCurves = append(aCurves, element(paraTag(0, 1.8))...)
		mCurves = append(mCurves, element(paraTag(0, adobeGamma/1.8))...)
		bCurves = append(bCurves, element([]byte("curv\x00\x00\x00\x00\x00\x00\x00\x00"))...)
	}
	clutData := make([]byte, 20)
	for i := range 3 {
		clutData[i] = grid
	}
	clutData[16] = 2
	for i := range grid {
		for j := range grid {
			for k := range grid {
				r, g, b := float64(i)/(grid-1), float64(j)/(grid-1), float64(k)/(grid-1)
				for _, v := range []float64{0.92*r + 0.08*g, g, 0.97*b + 0.03*r} {
					clutData = binary.BigEndian.AppendUint16(clutData, uint16(math.Round(v*65535)))
				}
			}
		}
	}
	matrix := make([]byte, 0, 48)
	scale := 32768.0 / 65535
	for _, row := range adobeColorants {
		for _, v := range row {
			matrix = binary.BigEndian.AppendUint32(matrix, uint32(fixed(v*scale)))
		}
	}
	matrix = append(matrix, make([]byte, 12)...)
	header := make([]byte, 32)
	copy(header, "mAB ")
	header[8], header[9] = 3, 3
	body := header
	offsets := map[string]int{}
	for _, part := range []struct {
		name string
		data []byte
	}{{"B", bCurves}, {"matrix", matrix}, {"M", mCurves}, {"CLUT", element(clutData)}, {"A", aCurves}} {
		offsets[part.name] = len(body)
		body = append(body, part.data...)
	}
	for i, name := range []string{"B", "matrix", "M", "CLUT", "A"} {
		binary.BigEndian.PutUint32(body[12+4*i:], uint32(offsets[name]))
	}
	return buildProfile(0x04300000, "scnr", "RGB ", "XYZ ", []testTag{
		{"desc", descTag("Synthetic lutAToB")},
		{"wtpt", xyzTag(D50[0], D50[1], D50[2])},
		{"A2B0", body},
	})
}

// The samples map to the sRGB values Little CMS 2.19 transicc gives
// (perceptual, -c0, rounded); TestTransformMatchesLittleCMS regenerates the
// comparison.
var testProfiles = []struct {
	name    string
	build   func() []byte
	source  string
	samples [][6]int // device RGB -> sRGB
}{
	{"matrix", adobeMatrixProfile, "matrix/TRC", [][6]int{
		{0, 0, 0, 0, 0, 0},
		{255, 255, 255, 255, 255, 255},
		{128, 128, 128, 129, 129, 129},
		{200, 100, 50, 227, 100, 42},
		{40, 160, 90, 0, 161, 85},
		{90, 60, 200, 99, 57, 205},
		{255, 0, 0, 255, 0, 0},
		{120, 200, 220, 51, 201, 222},
	}},
	{"lut16-lab", lut16LabProfile, "AToB0", [][6]int{
		{0, 0, 0, 0, 0, 0},
		{255, 255, 255, 255, 255, 255},
		{128, 128, 128, 129, 129, 129},
		{200, 100, 50, 209, 100, 52},
		{40, 160, 90, 0, 162, 82},
		{90, 60, 200, 114, 58, 199},
		{255, 0, 0, 255, 0, 5},
		{120, 200, 220, 83, 201, 217},
	}},
	{"lutAToB-xyz", lutAToBXYZProfile, "AToB0", [][6]int{
		{0, 0, 0, 0, 0, 0},
		{255, 255, 255, 255, 255, 255},
		{128, 128, 128, 128, 128, 128},
		{200, 100, 50, 219, 99, 50},
		{40, 160, 90, 0, 161, 82},
		{90, 60, 200, 95, 55, 202},
		{255, 0, 0, 255, 0, 27},
		{120, 200, 220, 75, 201, 219},
	}},
}

func TestParseAndTransformSyntheticProfiles(t *testing.T) {
	for _, tc := range testProfiles {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Parse(append(tc.build(), 0)) // DICOM even-length padding
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !strings.HasPrefix(p.Description(), "Synthetic") {
				t.Fatalf("Description = %q", p.Description())
			}
			transform, err := NewSRGBTransform(p)
			if err != nil {
				t.Fatalf("NewSRGBTransform: %v", err)
			}
			if transform.Source != tc.source || transform.Identity() {
				t.Fatalf("Source = %q identity %v", transform.Source, transform.Identity())
			}
			for _, s := range tc.samples {
				r, g, b := transform.Convert(float64(s[0])/255, float64(s[1])/255, float64(s[2])/255)
				got := [3]int{int(math.Round(r * 255)), int(math.Round(g * 255)), int(math.Round(b * 255))}
				for c := range 3 {
					if d := got[c] - s[3+c]; d > 1 || d < -1 {
						t.Fatalf("%v -> %v, want %v", s[:3], got, s[3:])
					}
				}
			}
		})
	}
}

func TestSRGBProfileIsIdentity(t *testing.T) {
	p, err := Parse(iccprofile.SRGB())
	if err != nil {
		t.Fatal(err)
	}
	transform, err := NewSRGBTransform(p)
	if err != nil {
		t.Fatal(err)
	}
	if !transform.Identity() {
		t.Fatal("the sRGB profile is not recognized as identity")
	}
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	copy(img.Pix, []byte{10, 20, 30, 255})
	if out := transform.Apply(img).(*image.RGBA); out.Pix[0] != 10 || out.Pix[2] != 30 {
		t.Fatalf("identity changed pixels: %v", out.Pix)
	}
}

func TestApplyConvertsImagesInPlace(t *testing.T) {
	p, err := Parse(adobeMatrixProfile())
	if err != nil {
		t.Fatal(err)
	}
	transform, err := NewSRGBTransform(p)
	if err != nil {
		t.Fatal(err)
	}
	rgba := image.NewRGBA(image.Rect(0, 0, 2, 1))
	copy(rgba.Pix, []byte{200, 100, 50, 255, 40, 160, 90, 128})
	transform.Apply(rgba)
	if want := []byte{227, 100, 42, 255, 0, 161, 85, 128}; !nearBytes(rgba.Pix, want, 1) {
		t.Fatalf("RGBA = %v, want %v", rgba.Pix, want)
	}
	wide := image.NewRGBA64(image.Rect(0, 0, 1, 1))
	copy(wide.Pix, []byte{200, 200, 100, 100, 50, 50, 255, 255})
	transform.Apply(wide)
	if got := []byte{wide.Pix[0], wide.Pix[2], wide.Pix[4]}; !nearBytes(got, []byte{227, 100, 42}, 1) {
		t.Fatalf("RGBA64 = %v", wide.Pix)
	}
}

func nearBytes(got, want []byte, tolerance int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if d := int(got[i]) - int(want[i]); d > tolerance || d < -tolerance {
			return false
		}
	}
	return true
}

func TestParseRejectsMalformedProfiles(t *testing.T) {
	good := adobeMatrixProfile()
	mutate := func(f func([]byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"short", good[:100], ErrInvalidProfile},
		{"size beyond data", mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b, uint32(len(b)+4)); return b }), ErrInvalidProfile},
		{"no acsp", mutate(func(b []byte) []byte { copy(b[36:], "xxxx"); return b }), ErrInvalidProfile},
		{"version", mutate(func(b []byte) []byte { b[8] = 9; return b }), ErrInvalidProfile},
		{"tag count", mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[128:], 5000); return b }), ErrInvalidProfile},
		{"tag outside", mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[132+8:], 1<<20); return b }), ErrInvalidProfile},
		{"duplicate tag", mutate(func(b []byte) []byte { copy(b[132+12:], "desc"); return b }), ErrInvalidProfile},
		{"bad PCS", mutate(func(b []byte) []byte { copy(b[20:], "CMYK"); return b }), ErrInvalidProfile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.data); !errors.Is(err, tt.want) {
				t.Fatalf("Parse = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNewSRGBTransformRefusesUnsupportedProfiles(t *testing.T) {
	gray := buildProfile(0x04300000, "mntr", "GRAY", "XYZ ", []testTag{{"kTRC", gammaTag(2.2)}})
	link := buildProfile(0x04300000, "link", "RGB ", "RGB ", []testTag{{"A2B0", []byte("mft2\x00\x00\x00\x00")}})
	noTRC := buildProfile(0x04300000, "mntr", "RGB ", "XYZ ", []testTag{{"rXYZ", xyzTag(1, 0, 0)}})
	for name, data := range map[string][]byte{"gray": gray, "link": link, "missing TRC": noTRC} {
		t.Run(name, func(t *testing.T) {
			p, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if _, err := NewSRGBTransform(p); err == nil {
				t.Fatal("NewSRGBTransform succeeded")
			}
		})
	}
}

// TestTransformMatchesLittleCMS is opt-in (DICOMGO_ICC_ORACLE=1): it
// converts a colour grid with Little CMS 2's transicc (perceptual intent to
// its built-in sRGB, bounded, no precalculated device link so that lcms
// evaluates the profile exactly) and requires every sample within 0.25 of an
// 8-bit level.
func TestTransformMatchesLittleCMS(t *testing.T) {
	if os.Getenv("DICOMGO_ICC_ORACLE") != "1" {
		t.Skip("set DICOMGO_ICC_ORACLE=1 to compare with Little CMS transicc")
	}
	var grid [][3]int
	for _, r := range []int{0, 37, 90, 128, 180, 230, 255} {
		for _, g := range []int{0, 60, 128, 200, 255} {
			for _, b := range []int{0, 45, 128, 210, 255} {
				grid = append(grid, [3]int{r, g, b})
			}
		}
	}
	for _, tc := range testProfiles {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.icc")
			if err := os.WriteFile(path, tc.build(), 0o600); err != nil {
				t.Fatal(err)
			}
			want := littleCMS(t, path, grid)
			p, err := Parse(tc.build())
			if err != nil {
				t.Fatal(err)
			}
			transform, err := NewSRGBTransform(p)
			if err != nil {
				t.Fatal(err)
			}
			worst := 0.0
			for i, c := range grid {
				r, g, b := transform.Convert(float64(c[0])/255, float64(c[1])/255, float64(c[2])/255)
				for k, v := range []float64{r, g, b} {
					d := math.Abs(v*255 - want[i][k])
					worst = max(worst, d)
					if d > 0.25 {
						t.Errorf("%v channel %d = %.3f, Little CMS %.3f", c, k, v*255, want[i][k])
					}
				}
			}
			t.Logf("worst difference %.4f of an 8-bit level", worst)
		})
	}
}

func littleCMS(t *testing.T, profile string, rgb [][3]int) [][3]float64 {
	t.Helper()
	var input strings.Builder
	for _, c := range rgb {
		fmt.Fprintf(&input, "%d %d %d\n", c[0], c[1], c[2])
	}
	cmd := exec.Command("transicc", "-s", "-c0", "-t0", "-n", "-i"+profile, "-o*sRGB")
	cmd.Stdin = strings.NewReader(input.String())
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("transicc: %v", err)
	}
	var out [][3]float64
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		var v [3]float64
		for i, field := range fields {
			if v[i], err = strconv.ParseFloat(field, 64); err != nil {
				t.Fatalf("transicc output %q", line)
			}
		}
		out = append(out, v)
	}
	if len(out) != len(rgb) {
		t.Fatalf("transicc returned %d colours for %d", len(out), len(rgb))
	}
	return out
}

func TestMatrixTRCBuildRoundTrips(t *testing.T) {
	p, err := Parse(AdobeRGB1998.Build())
	if err != nil {
		t.Fatal(err)
	}
	if p.Description() != AdobeRGB1998.Description {
		t.Fatalf("Description = %q", p.Description())
	}
	transform, err := NewSRGBTransform(p)
	if err != nil {
		t.Fatal(err)
	}
	// Same colorimetry as the synthetic matrix profile (gamma as para, not
	// curv u8Fixed8).
	r, g, b := transform.Convert(200.0/255, 100.0/255, 50.0/255)
	if got := []byte{byte(math.Round(r * 255)), byte(math.Round(g * 255)), byte(math.Round(b * 255))}; !nearBytes(got, []byte{227, 100, 42}, 1) {
		t.Fatalf("Convert = %v", got)
	}
}
