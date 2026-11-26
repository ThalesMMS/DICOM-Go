package compare_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/compare"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type countingPresentedReader struct {
	*pixeldata.FrameReader
	calls  int
	failAt int
}

func (r *countingPresentedReader) Frame(ctx context.Context, i int) ([]byte, pixeldata.Metadata, error) {
	r.calls++
	if i == r.failAt {
		return nil, pixeldata.Metadata{}, errors.New("synthetic frame refusal")
	}
	return r.FrameReader.Frame(ctx, i)
}
func readerFixture(t testing.TB, frames, offset int) *countingPresentedReader {
	t.Helper()
	elements := dicomtest.DatasetWithPixelData()
	pixels := make([]byte, 64*frames)
	for i := range pixels {
		pixels[i] = byte(i%64 + offset)
	}
	for i, e := range elements {
		if e.Tag() == core.TagPixelData {
			elements[i] = dicomtest.NewOBElement(core.TagPixelData, pixels)
		}
	}
	elements = append(elements, dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, fmt.Sprint(frames)))
	encoded, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	reader, err := pixeldata.NewFrameReader(context.Background(), file, bytes.NewReader(encoded), pixeldata.FrameReaderOptions{MaxFrames: frames})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return &countingPresentedReader{FrameReader: reader, failAt: -1}
}
func readerOptions() compare.PresentedReaderOptions {
	o := localOptions()
	o.SSIM.Border = "reflect"
	return compare.PresentedReaderOptions{Comparison: o, Image: compare.PresentedImagePolicy{Depth: 16, Encoding: "gray-pvalue", Alpha: "opaque"}, Mode: "common", Reference: compare.PresentationProfile{VOI: display.VOILUT{Center: 32, Width: 64, Function: display.VOILinearExact}}, MaxFrames: 32}
}
func TestPresentedReadersCommonAndPerImageVOI(t *testing.T) {
	a, b := readerFixture(t, 3, 0), readerFixture(t, 3, 8)
	o := readerOptions()
	frames := 0
	n, err := compare.ComparePresentedReaders(context.Background(), a, b, o, func(_ context.Context, r compare.PresentedFrameRecord) error {
		frames++
		if r.Mode != "common" || r.ReferenceProfile.VOI.Center != r.CandidateProfile.VOI.Center || r.Result.Summary.Numerical.Identical() {
			t.Fatalf("common %+v", r)
		}
		return nil
	})
	if err != nil || n != 3 || frames != 3 {
		t.Fatalf("common completed%d frames%d err%v", n, frames, err)
	}
	o.Mode = "per-image"
	o.Candidate = compare.PresentationProfile{VOI: display.VOILUT{Center: 40, Width: 64, Function: display.VOILinearExact}}
	n, err = compare.ComparePresentedReaders(context.Background(), a, b, o, func(_ context.Context, r compare.PresentedFrameRecord) error {
		if !r.Result.Summary.Numerical.Identical() || r.Result.Summary.MeanSSIM != 1 || r.CandidateProfile.VOI.Center != 40 {
			t.Fatalf("explicit matching %+v", r)
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("per image %d %v", n, err)
	}
	// Existing stored/modality APIs remain independent of the new presentation.
	metrics, err := compare.Files(context.Background(), a.File(), b.File(), compare.Options{Domain: compare.DomainStored})
	if err != nil || metrics.MeanAbsoluteError != 8 || metrics.Frames != 3 {
		t.Fatalf("stored unchanged %+v %v", metrics, err)
	}
}
func TestPresentedReadersPresentationLUTAndDepth(t *testing.T) {
	a, b := readerFixture(t, 1, 0), readerFixture(t, 1, 0)
	o := readerOptions()
	o.Reference.Invert = true
	for _, depth := range []int{8, 16} {
		o.Image.Depth = depth
		n, err := compare.ComparePresentedReaders(context.Background(), a, b, o, func(_ context.Context, r compare.PresentedFrameRecord) error {
			if r.Result.Depth != depth || !r.Result.Summary.Numerical.Identical() || !r.ReferenceProfile.Invert || r.ReferenceModality.Slope != 0 && r.ReferenceModality.Slope != 1 {
				t.Fatalf("presentation %+v", r)
			}
			return nil
		})
		if err != nil || n != 1 {
			t.Fatalf("depth%d n%d %v", depth, n, err)
		}
	}
	o.Mode = "per-image"
	o.Candidate = o.Reference
	o.Candidate.Invert = false
	n, err := compare.ComparePresentedReaders(context.Background(), a, b, o, func(_ context.Context, r compare.PresentedFrameRecord) error {
		if r.Result.Summary.Numerical.Identical() {
			t.Fatal("inversion lost")
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("invert %d %v", n, err)
	}
	lut, err := display.NewLUT([]int{256, 0, 8}, make([]uint16, 256))
	if err != nil {
		t.Fatal(err)
	}
	for i := range lut.Entries {
		lut.Entries[i] = uint16(i)
	}
	o.Reference.LUT = lut
	o.Mode = "common"
	n, err = compare.ComparePresentedReaders(context.Background(), a, b, o, func(_ context.Context, r compare.PresentedFrameRecord) error {
		if r.ReferenceProfile.LUT == nil || len(r.ReferenceProfile.LUT.Entries) != 256 {
			t.Fatal("LUT unrecorded")
		}
		r.ReferenceProfile.LUT.Entries[0] = 200
		return nil
	})
	if err != nil || n != 1 || lut.Entries[0] != 0 {
		t.Fatalf("detached LUT %d %v", n, err)
	}
}
func TestPresentedReadersCancellationFailureAndBoundedPrefix(t *testing.T) {
	t.Run("detached-delivery-policy", func(t *testing.T) {
		a, b := readerFixture(t, 3, 0), readerFixture(t, 3, 0)
		o := readerOptions()
		var first, second *compare.PresentedImagePolicy
		n, err := compare.ComparePresentedReaders(context.Background(), a, b, o, func(_ context.Context, r compare.PresentedFrameRecord) error {
			policy := r.Result.ImagePolicy
			if policy == nil || *policy != o.Image || r.Result.Depth != 16 || r.Result.Encoding != "gray-pvalue" || r.Result.Summary.MeanSSIM != 1 {
				t.Fatalf("frame%d execution policy altered: %+v result%+v", r.Index, policy, r.Result)
			}
			if policy == first || policy == second {
				t.Fatal("delivered policies alias")
			}
			switch r.Index {
			case 0:
				first = policy
				policy.Depth = 8
				policy.Encoding = "linear-rgb"
				policy.Background = [3]float64{.25, .25, .25}
			case 1:
				second = policy
				policy.Depth = 8
				policy.Encoding = "srgb-code"
				policy.Background = [3]float64{.75, .75, .75}
			case 2:
				if first.Depth != 8 || first.Encoding != "linear-rgb" || first.Background[0] != .25 || second.Encoding != "srgb-code" || second.Background[0] != .75 {
					t.Fatal("later delivery changed earlier policy metadata")
				}
			}
			return nil
		})
		if err != nil || n != 3 || a.calls != 3 || b.calls != 3 {
			t.Fatalf("policy delivery completed%d err%v calls%d/%d", n, err, a.calls, b.calls)
		}
	})
	a, b := readerFixture(t, 16, 0), readerFixture(t, 16, 0)
	o := readerOptions()
	ctx, cancel := context.WithCancel(context.Background())
	var admitted int64
	n, err := compare.ComparePresentedReaders(ctx, a, b, o, func(_ context.Context, r compare.PresentedFrameRecord) error {
		admitted = r.Result.LogicalWorkingBytes
		cancel()
		return nil
	})
	if n != 1 || !errors.Is(err, context.Canceled) || a.calls != 1 || b.calls != 1 || admitted > o.Comparison.MaxWorkingBytes {
		t.Fatalf("cancel n%d err%v calls%d/%d budget%d", n, err, a.calls, b.calls, admitted)
	}
	a.calls = 0
	b.calls = 0
	sentinel := errors.New("consumer stopped")
	n, err = compare.ComparePresentedReaders(context.Background(), a, b, o, func(context.Context, compare.PresentedFrameRecord) error { return sentinel })
	if n != 0 || !errors.Is(err, sentinel) || a.calls != 1 || b.calls != 1 {
		t.Fatalf("consumer n%d %v", n, err)
	}
	a.calls = 0
	b.calls = 0
	b.failAt = 1
	seen := 0
	n, err = compare.ComparePresentedReaders(context.Background(), a, b, o, func(_ context.Context, r compare.PresentedFrameRecord) error {
		seen++
		if r.Result.LogicalWorkingBytes != admitted {
			t.Fatal("multiframe budget changed")
		}
		return nil
	})
	if n != 1 || err == nil || seen != 1 || a.calls != 2 || b.calls != 2 {
		t.Fatalf("frame failure n%d seen%d %v", n, seen, err)
	}
	// Readers remain borrowed and usable after failures.
	b.failAt = -1
	if _, _, err = b.Frame(context.Background(), 0); err != nil {
		t.Fatal("caller reader released", err)
	}
}
func TestPresentedReadersAdmissionBeforeDecodeAndExplicitWindow(t *testing.T) {
	a, b := readerFixture(t, 2, 0), readerFixture(t, 2, 0)
	base := readerOptions()
	for _, change := range []func(*compare.PresentedReaderOptions){func(o *compare.PresentedReaderOptions) { o.Comparison.MaxWorkingBytes = 1 }, func(o *compare.PresentedReaderOptions) { o.MaxFrames = 1 }, func(o *compare.PresentedReaderOptions) { o.Reference.VOI.Width = 0 }, func(o *compare.PresentedReaderOptions) { o.Mode = "implicit" }} {
		o := base
		change(&o)
		n, err := compare.ComparePresentedReaders(context.Background(), a, b, o, func(context.Context, compare.PresentedFrameRecord) error {
			t.Fatal("consumer before admission")
			return nil
		})
		if n != 0 || err == nil || a.calls != 0 || b.calls != 0 {
			t.Fatalf("admission n%d err%v calls%d/%d", n, err, a.calls, b.calls)
		}
	}
}

func BenchmarkPresentedMultiframe(b *testing.B) {
	for _, frames := range []int{1, 16} {
		b.Run(fmt.Sprint(frames), func(b *testing.B) {
			a, c := readerFixture(b, frames, 0), readerFixture(b, frames, 1)
			o := readerOptions()
			b.ReportAllocs()
			b.SetBytes(int64(2 * 64 * frames))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				n, err := compare.ComparePresentedReaders(context.Background(), a, c, o, func(context.Context, compare.PresentedFrameRecord) error { return nil })
				if err != nil || n != frames {
					b.Fatalf("frames%d err%v", n, err)
				}
			}
		})
	}
}

func TestPresentedReadersColorAndICCRefusal(t *testing.T) {
	makeRGB := func() *countingPresentedReader {
		r := readerFixture(t, 1, 0)
		r.File().Dataset.SetUS(core.NewTag(0x0028, 0x0002), 3)
		r.File().Dataset.SetUS(core.NewTag(0x0028, 0x0006), 0)
		if err := r.File().Dataset.SetStrings(core.NewTag(0x0028, 0x0004), core.VRCS, "RGB"); err != nil {
			t.Fatal(err)
		}
		pix := make([]byte, 64*3)
		for i := range pix {
			pix[i] = byte(i % 251)
		}
		r.File().Dataset.Put(dicomtest.NewOBElement(core.TagPixelData, pix))
		fr, err := pixeldata.NewFrameReader(context.Background(), r.File(), nil, pixeldata.FrameReaderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return &countingPresentedReader{FrameReader: fr, failAt: -1}
	}
	a, b := makeRGB(), makeRGB()
	o := readerOptions()
	o.Image.Encoding = "linear-rgb"
	o.Reference = compare.PresentationProfile{}
	for _, depth := range []int{8, 16} {
		o.Image.Depth = depth
		n, err := compare.ComparePresentedReaders(context.Background(), a, b, o, func(_ context.Context, r compare.PresentedFrameRecord) error {
			if r.Result.Components != 3 || r.Result.Encoding != "linear-rgb" || r.Result.Summary.MeanSSIM != 1 {
				t.Fatalf("RGB %+v", r)
			}
			return nil
		})
		if err != nil || n != 1 {
			t.Fatalf("color depth%d n%d %v", depth, n, err)
		}
	}
	a.calls = 0
	b.calls = 0
	a.File().Dataset.Put(dicomtest.NewOBElement(core.NewTag(0x0028, 0x2000), []byte{0, 0}))
	n, err := compare.ComparePresentedReaders(context.Background(), a, b, o, func(context.Context, compare.PresentedFrameRecord) error {
		t.Fatal("unmanaged ICC reached consumer")
		return nil
	})
	if n != 0 || !errors.Is(err, compare.ErrMismatch) || a.calls != 0 || b.calls != 0 {
		t.Fatalf("ICC n%d err%v calls%d/%d", n, err, a.calls, b.calls)
	}
}
