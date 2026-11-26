package compare_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata/compare"
	"github.com/ThalesMMS/dicom-go/render/qualification"
)

func plane(width, height int, values []float64) compare.PresentedFrame {
	return compare.PresentedFrame{Width: width, Height: height, Components: 1, Depth: 16, Encoding: "gray-pvalue", Values: values}
}
func localOptions() compare.PresentedOptions {
	o := compare.DefaultPresentedOptions()
	o.SSIM = compare.SSIMOptions{Window: 3, Kernel: "uniform", K1: .01, K2: .03, Range: 1, Border: "valid"}
	o.Difference = "signed"
	return o
}
func presented(t *testing.T, a, b compare.PresentedFrame, o compare.PresentedOptions) *compare.PresentedResult {
	t.Helper()
	r, err := compare.ComparePresented(context.Background(), a, b, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestPresentedAnalyticConstantsAndOffset(t *testing.T) {
	o := localOptions()
	o.SSIM.Window = 1
	for _, pair := range [][2]float64{{0, 0}, {.25, .25}, {.25, .5}, {0, 1}} {
		a, b := make([]float64, 9), make([]float64, 9)
		for i := range a {
			a[i] = pair[0]
			b[i] = pair[1]
		}
		r := presented(t, plane(3, 3, a), plane(3, 3, b), o)
		// Independent closed form: variances and covariance vanish exactly.
		want := (2*pair[0]*pair[1] + .0001) / (pair[0]*pair[0] + pair[1]*pair[1] + .0001)
		if !near(r.Summary.MeanSSIM, want) || !near(r.Summary.Numerical.MeanAbsoluteError, math.Abs(pair[1]-pair[0])) || r.Summary.SSIMSamples != 9 {
			t.Fatalf("%v: %+v wantSSIM%.17g", pair, r.Summary, want)
		}
		for _, d := range r.Difference {
			if d != pair[1]-pair[0] {
				t.Fatalf("signed %.17g", d)
			}
		}
	}
}
func TestPresentedIndependentPopulationOracle(t *testing.T) {
	// Fractional fixture: x=[0..8]/16, y=x+1/8. Mean x=1/4,
	// mean y=3/8, population variance/covariance=5/192. The second
	// SSIM factor is exactly one. Contrast y=2x has covariance=5/96.
	x := []float64{0, 1.0 / 16, 2.0 / 16, 3.0 / 16, 4.0 / 16, 5.0 / 16, 6.0 / 16, 7.0 / 16, 8.0 / 16}
	for _, test := range []struct {
		name string
		y    []float64
		want float64
		mae  float64
	}{{"offset", []float64{.125, .1875, .25, .3125, .375, .4375, .5, .5625, .625}, (.1875 + .0001) / (.203125 + .0001), .125}, {"contrast", []float64{0, .125, .25, .375, .5, .625, .75, .875, 1}, (.25 + .0001) / (.3125 + .0001) * ((5.0/48 + .0009) / (25.0/192 + .0009)), .25}} {
		t.Run(test.name, func(t *testing.T) {
			r := presented(t, plane(3, 3, x), plane(3, 3, test.y), localOptions())
			if r.Summary.SSIMSamples != 1 || !near(r.SSIM[4], test.want) || !near(r.Summary.Numerical.MeanAbsoluteError, test.mae) {
				t.Fatalf("%+v center%.17g want%.17g", r.Summary, r.SSIM[4], test.want)
			}
		})
	}
}
func TestPresentedBlurAndLocalizedRegions(t *testing.T) {
	a := make([]float64, 81)
	b := make([]float64, 81)
	a[40] = 1
	b[40] = 1.0 / 9
	for _, i := range []int{30, 31, 32, 39, 41, 48, 49, 50} {
		b[i] = 1.0 / 9
	}
	o := localOptions()
	o.SSIM.Border = "crop"
	o.Regions = []compare.ComparisonRegion{{Name: "changed", Bounds: image.Rect(3, 3, 6, 6)}, {Name: "outside", Bounds: image.Rect(0, 0, 2, 2)}}
	r := presented(t, plane(9, 9, a), plane(9, 9, b), o)
	if r.Summary.Numerical.MaxAbsoluteError != 8.0/9 || r.Summary.MinimumSSIM >= r.Summary.MeanSSIM || r.Regions[0].MeanSSIM >= r.Regions[1].MeanSSIM || r.Regions[1].MeanSSIM != 1 {
		t.Fatalf("localized %+v regions%+v", r.Summary, r.Regions)
	}
}
func TestPresentedBordersWeightsMaskAndColorComponents(t *testing.T) {
	a := []float64{0, .5, 1, .25, .75, 0, 1, .5, 0}
	b := append([]float64(nil), a...)
	b[0] = 1
	for _, border := range []string{"crop", "reflect", "valid"} {
		for _, kernel := range []string{"uniform", "gaussian"} {
			o := localOptions()
			o.SSIM.Border = border
			o.SSIM.Kernel = kernel
			o.SSIM.Sigma = 1
			o.Mask = []bool{false, true, true, true, true, true, true, true, true}
			r := presented(t, plane(3, 3, a), plane(3, 3, b), o)
			if r.Valid[0] || r.Summary.MeanSSIM != 1 || !r.Summary.Numerical.Identical() {
				t.Fatalf("%s/%s %+v", border, kernel, r.Summary)
			}
		}
	}
	o := localOptions()
	o.SSIM.Window = 1
	rgb := compare.PresentedFrame{Width: 1, Height: 1, Components: 3, Depth: 8, Encoding: "srgb-code", Values: []float64{0, .5, 1}}
	cand := rgb
	cand.Values = []float64{1, .5, 0}
	r := presented(t, rgb, cand, o)
	if r.Summary.SSIMSamples != 3 || r.Summary.Numerical.Samples != 3 || r.Difference[0] != 1 || r.Difference[2] != -1 {
		t.Fatalf("rgb %+v", r)
	}
}
func TestPresentedInvalidConfigurationAndBudget(t *testing.T) {
	a := plane(1, 1, []float64{.5})
	base := localOptions()
	base.SSIM.Window = 1
	for _, change := range []func(*compare.PresentedOptions){func(o *compare.PresentedOptions) { o.SSIM.Range = 0 }, func(o *compare.PresentedOptions) { o.SSIM.K1 = 1e-200 }, func(o *compare.PresentedOptions) { o.SSIM.Sigma = math.NaN() }, func(o *compare.PresentedOptions) { o.SSIM.Window = 2 }, func(o *compare.PresentedOptions) { o.Scale = 0 }, func(o *compare.PresentedOptions) { o.Mask = []bool{false} }, func(o *compare.PresentedOptions) {
		o.Regions = []compare.ComparisonRegion{{Name: "outside", Bounds: image.Rect(0, 0, 2, 2)}}
	}} {
		o := base
		change(&o)
		r, err := compare.ComparePresented(context.Background(), a, a, o)
		if err == nil || r != nil {
			t.Fatalf("invalid %+v r%+v err%v", o, r, err)
		}
	}
	o := base
	o.MaxWorkingBytes = 1
	if _, err := compare.ComparePresented(context.Background(), a, a, o); !errors.Is(err, compare.ErrPresentedLimit) {
		t.Fatal(err)
	}
	for _, v := range []float64{-1, 2, math.NaN(), math.Inf(1)} {
		bad := a
		bad.Values = []float64{v}
		if _, err := compare.ComparePresented(context.Background(), a, bad, base); !errors.Is(err, compare.ErrMismatch) {
			t.Fatal(err)
		}
	}
	mismatch := a
	mismatch.Depth = 8
	if _, err := compare.ComparePresented(context.Background(), a, mismatch, base); !errors.Is(err, compare.ErrMismatch) {
		t.Fatal(err)
	}
}
func TestPresentedImageAlphaLinearAndDepth(t *testing.T) {
	a, b := image.NewNRGBA(image.Rect(2, 3, 3, 4)), image.NewNRGBA(image.Rect(0, 0, 1, 1))
	a.SetNRGBA(2, 3, color.NRGBA{R: 255, A: 128})
	b.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	o := localOptions()
	o.SSIM.Window = 1
	p := compare.PresentedImagePolicy{Depth: 8, Encoding: "linear-rgb", Alpha: "composite", Background: [3]float64{0, 0, 0}}
	r, err := compare.ComparePresentedImages(context.Background(), a, b, p, o)
	if err != nil {
		t.Fatal(err)
	}
	if !near(r.Difference[0], 127.0/255) || r.ImagePolicy == nil {
		t.Fatalf("alpha %+v", r)
	}
	p.Alpha = "opaque"
	if _, err = compare.ComparePresentedImages(context.Background(), a, b, p, o); !errors.Is(err, compare.ErrMismatch) {
		t.Fatal(err)
	}
	p.Alpha = "ignore"
	r, err = compare.ComparePresentedImages(context.Background(), a, b, p, o)
	if err != nil || !r.Summary.Numerical.Identical() {
		t.Fatalf("ignore %v %+v", err, r)
	}
	gray := image.NewGray16(image.Rect(0, 0, 1, 1))
	gray.SetGray16(0, 0, color.Gray16{Y: 32768})
	p = compare.PresentedImagePolicy{Depth: 16, Encoding: "gray-pvalue", Alpha: "opaque"}
	if _, err = compare.ComparePresentedImages(context.Background(), gray, gray, p, o); err != nil {
		t.Fatal(err)
	}
	p.Depth = 8
	if _, err = compare.ComparePresentedImages(context.Background(), gray, gray, p, o); !errors.Is(err, compare.ErrMismatch) {
		t.Fatal(err)
	}
}
func TestPresentedRGBAReusesRawSilhouetteMetrics(t *testing.T) {
	o := localOptions()
	o.SSIM.Window = 1
	p := compare.PresentedImagePolicy{Depth: 8, Encoding: "srgb-code", Alpha: "composite", Background: [3]float64{1, 1, 1}}
	a := []byte{0, 0, 0, 0}
	b := []byte{255, 255, 255, 255}
	r, err := compare.ComparePresentedRGBA8(context.Background(), a, b, 1, 1, p, o, qualification.RGBAFrameThresholdsV1{AlphaSilhouetteCutoff: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.RawRGBA == nil || !r.Summary.Numerical.Identical() {
		t.Fatalf("separated pre/post %+v", r)
	}
}
func TestPresentedExportsExactMapsScaleAndMetadata(t *testing.T) {
	o := localOptions()
	o.SSIM.Window = 1
	o.Scale = .25
	r := presented(t, plane(2, 1, []float64{.25, .5}), plane(2, 1, []float64{.5, .25}), o)
	var streams [6]bytes.Buffer
	w := compare.PresentedWriters{Difference: &streams[0], SSIM: &streams[1], Validity: &streams[2], DifferencePNG: &streams[3], SSIMPNG: &streams[4], Metadata: &streams[5]}
	m, err := compare.ExportPresented(context.Background(), r, nil, w, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(streams[0].Bytes()) != 16 || math.Float64frombits(binary.LittleEndian.Uint64(streams[0].Bytes())) != .25 || math.Float64frombits(binary.LittleEndian.Uint64(streams[0].Bytes()[8:])) != -.25 || !bytes.Equal(streams[2].Bytes(), []byte{3, 3}) {
		t.Fatal("exact maps/mask")
	}
	im, err := png.Decode(bytes.NewReader(streams[3].Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	red, _, _, alpha := im.At(0, 0).RGBA()
	black, _, _, _ := im.At(1, 0).RGBA()
	if im.Bounds() != image.Rect(0, 0, 2, 1) || red != 65535 || black != 0 || alpha != 65535 {
		t.Fatal("fixed signed scale")
	}
	hash := sha256.Sum256(streams[0].Bytes())
	if m.Artifacts["difference.f64le"].SHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("hash")
	}
	var decoded compare.PresentedExportMetadata
	if err = json.Unmarshal(streams[5].Bytes(), &decoded); err != nil || decoded.Options.Scale != .25 || decoded.Summary.Numerical.Samples != 2 {
		t.Fatalf("metadata %v %+v", err, decoded)
	}
	// Identical planes produce +Inf dB; export must encode it explicitly, not fail JSON.
	same := presented(t, plane(2, 1, []float64{0, 0}), plane(2, 1, []float64{0, 0}), o)
	for i := range streams {
		streams[i].Reset()
	}
	m, err = compare.ExportPresented(context.Background(), same, nil, w, 1<<20)
	if err != nil || m.Summary.Numerical.PSNR != "+Inf" {
		t.Fatalf("identical export %v %+v", err, m)
	}
}
func TestPresentedCancellationAndExportFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := plane(1, 1, []float64{0})
	o := localOptions()
	o.SSIM.Window = 1
	if r, err := compare.ComparePresented(ctx, a, a, o); r != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v %+v", err, r)
	}
	r := presented(t, a, a, o)
	var dst bytes.Buffer
	w := compare.PresentedWriters{Difference: &dst, SSIM: &dst, Validity: &dst, DifferencePNG: &dst, SSIMPNG: &dst, Metadata: &dst}
	if _, err := compare.ExportPresented(context.Background(), r, nil, w, 1); !errors.Is(err, compare.ErrPresentedLimit) || dst.Len() != 0 {
		t.Fatalf("bounded export %v bytes%d", err, dst.Len())
	}
}
func BenchmarkPresentedComparison(b *testing.B) {
	a, c := make([]float64, 64*64), make([]float64, 64*64)
	for i := range a {
		a[i] = float64(i%251) / 250
		c[i] = a[i]
	}
	o := localOptions()
	o.SSIM.Border = "reflect"
	b.Run("numerical-only", func(b *testing.B) {
		b.ReportAllocs()
		for k := 0; k < b.N; k++ {
			var acc compare.Accumulator
			for i := range a {
				acc.Add(a[i], c[i])
			}
			_ = acc.Metrics()
		}
	})
	b.Run("difference-local-ssim", func(b *testing.B) {
		b.ReportAllocs()
		for k := 0; k < b.N; k++ {
			if _, err := compare.ComparePresented(context.Background(), plane(64, 64, a), plane(64, 64, c), o); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestPresentedExplicitSRGBTransferFunction(t *testing.T) {
	a, b := image.NewNRGBA(image.Rect(0, 0, 1, 1)), image.NewNRGBA(image.Rect(0, 0, 1, 1))
	a.SetNRGBA(0, 0, color.NRGBA{R: 128, G: 128, B: 128, A: 255})
	b.SetNRGBA(0, 0, color.NRGBA{A: 255})
	o := localOptions()
	o.SSIM.Window = 1
	p := compare.PresentedImagePolicy{Depth: 8, Encoding: "linear-rgb", Alpha: "opaque"}
	r, err := compare.ComparePresentedImages(context.Background(), a, b, p, o)
	if err != nil {
		t.Fatal(err)
	}
	if !near(r.Difference[0], -.21586050011389926) {
		t.Fatalf("IEC sRGB8 midpoint%.17g", r.Difference[0])
	}
	p.Encoding = "srgb-code"
	r, err = compare.ComparePresentedImages(context.Background(), a, b, p, o)
	if err != nil || !near(r.Difference[0], -128.0/255) {
		t.Fatalf("code domain %v %+v", err, r)
	}
}

func TestPresentedIndependentWeightedEdgeOracle(t *testing.T) {
	encoded, err := os.ReadFile("presented_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		Schema               string
		Width, Height        int
		Reference, Candidate []float64
		Tolerance            float64
		Rows                 []struct {
			Border, Kernel string
			Sigma          float64
			SSIM           []float64
		}
	}
	if err = json.Unmarshal(encoded, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.Schema != "synthetic-local-ssim-oracle-v1" || oracle.Tolerance != 1e-12 || len(oracle.Rows) != 4 {
		t.Fatal("oracle contract")
	}
	for _, row := range oracle.Rows {
		o := localOptions()
		o.SSIM.Border = row.Border
		o.SSIM.Kernel = row.Kernel
		o.SSIM.Sigma = row.Sigma
		r := presented(t, plane(oracle.Width, oracle.Height, oracle.Reference), plane(oracle.Width, oracle.Height, oracle.Candidate), o)
		if len(r.SSIM) != len(row.SSIM) {
			t.Fatal("map extent")
		}
		for i, want := range row.SSIM {
			if math.Abs(r.SSIM[i]-want) > oracle.Tolerance {
				t.Fatalf("independent %s/%s pixel%d got%.17g want%.17g", row.Border, row.Kernel, i, r.SSIM[i], want)
			}
		}
	}
}
