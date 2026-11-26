package autothreshold

import (
	"image"
	"image/color"
	_ "image/png"
	"os"
	"testing"
)

// TestITKExpectedThresholdsOnCTHead1 is opt-in: set DICOMGO_CTHEAD1_PNG to a
// local copy of ITK's Testing/Data/Input/cthead1.png (fetched by ITK's
// ExternalData; it is not part of this repository). It compares the
// thresholds that ITK 35b196e1's Modules/Filtering/Thresholding/test/
// CMakeLists.txt expects with 256 bins and automatic minimum/maximum.
//
// The histogram is rebuilt the way ITK's ImageToHistogramFilter builds it
// for a floating-point measurement type: from the image minimum to the
// maximum plus 1/100 of a bin. ITK reports the centre of the threshold bin
// cast to the pixel type (short), except Otsu, which reports the bin's upper
// edge. Triangle and Kittler–Illingworth are logged only: ITK's Triangle
// draws its line to the 1st/99th percentile with vertical distances and
// returns the next bin, and its Kittler–Illingworth is the iterative
// variant started from the mean, while this package follows Zack et al. and
// the global minimum of the criterion. The other methods must agree to
// within one gray level.
func TestITKExpectedThresholdsOnCTHead1(t *testing.T) {
	path := os.Getenv("DICOMGO_CTHEAD1_PNG")
	if path == "" {
		t.Skip("set DICOMGO_CTHEAD1_PNG to ITK's cthead1.png to compare the ITK expected thresholds")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	img, _, err := image.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	bounds := img.Bounds()
	values := make([]float64, 0, bounds.Dx()*bounds.Dy())
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			var v float64
			switch m := img.(type) {
			case *image.Gray:
				v = float64(m.GrayAt(x, y).Y)
			case *image.Gray16:
				v = float64(int16(m.Gray16At(x, y).Y))
			default:
				v = float64(color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
			}
			values = append(values, v)
		}
	}
	lower, upper := observedRange(SliceValues(values))
	upper += (upper - lower) / (256 * 100)
	h, err := NewHistogram(SliceValues(values), HistogramOptions{Bins: 256, Lower: lower, Upper: upper})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method  Method
		itk     int
		logOnly bool
	}{
		{Otsu, 84, false}, {Huang, 19, false}, {IsoData, 85, false},
		{KittlerIllingworth, 32, true}, {Li, 54, false}, {MaxEntropy, 140, false},
		{Moments, 133, false}, {RenyiEntropy, 51, false}, {Shanbhag, 133, false},
		{Triangle, 2, true}, {Yen, 26, false}, {Intermodes, 76, false},
	} {
		got, err := h.Threshold(c.method)
		if err != nil {
			t.Errorf("%s: %v", c.method, err)
			continue
		}
		reported := int(got.Center)
		if c.method == Otsu {
			reported = int(got.Value)
		}
		t.Logf("%-20s dicom-go %4d  ITK %4d", c.method, reported, c.itk)
		if d := reported - c.itk; !c.logOnly && (d < -1 || d > 1) {
			t.Errorf("%s: %d, ITK expects %d", c.method, reported, c.itk)
		}
	}
}
