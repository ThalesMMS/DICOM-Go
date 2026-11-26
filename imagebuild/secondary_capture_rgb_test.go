package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func rgbCaptureOptions() SecondaryCaptureRGBOptions {
	black := color.NRGBA{A: 255}
	return SecondaryCaptureRGBOptions{
		Common:         object.DerivedCommon{Source: commontest.Source()},
		SOPInstanceUID: "1.2.826.0.1.3680043.10.999.101", SeriesInstanceUID: "1.2.826.0.1.3680043.10.999.102",
		StudyInstanceUID: "1.2.826.0.1.3680043.10.999.103", SeriesNumber: 900, InstanceNumber: 1,
		SeriesDescription: "Synthetic RGB capture", Background: &black, BurnedInAnnotation: "YES",
		DeviceManufacturer: "Synthetic", DeviceModelName: "Test", DeviceSoftwareVersions: "1",
		ContentTime: time.Date(2026, 10, 8, 10, 12, 13, 123456000, time.UTC),
		Derivation: derivation.Derivation{Description: "Synthetic display capture", Sources: []derivation.Source{{
			SOPClassUID: ctImageStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.999.104", Frames: []int{2},
		}}},
	}
}

func TestSecondaryCaptureRGBPixelsAlphaStrideAndSubimage(t *testing.T) {
	parent := image.NewNRGBA(image.Rect(5, 7, 9, 10))
	parent.SetNRGBA(6, 8, color.NRGBA{R: 200, G: 100, B: 50, A: 128})
	parent.SetNRGBA(7, 8, color.NRGBA{R: 17, G: 33, B: 250, A: 255})
	coloredBackground := color.NRGBA{R: 10, G: 20, B: 30, A: 255}
	// Expected samples are literal independent integer-compositing results.
	// RGBA preserves its already-premultiplied samples without requantization.
	for _, tt := range []struct {
		name       string
		frame      image.Image
		background *color.NRGBA
		want       []byte
	}{
		{"odd opaque", &image.NRGBA{Rect: image.Rect(0, 0, 1, 1), Stride: 4, Pix: []byte{17, 33, 250, 255}}, nil, []byte{17, 33, 250, 0}},
		{"straight partial and zero alpha", &image.NRGBA{Rect: image.Rect(0, 0, 2, 1), Stride: 8, Pix: []byte{200, 100, 50, 128, 255, 7, 99, 0}}, nil, []byte{100, 50, 25, 0, 0, 0}},
		{"explicit colored background", &image.NRGBA{Rect: image.Rect(0, 0, 2, 1), Stride: 8, Pix: []byte{200, 100, 50, 128, 255, 7, 99, 0}}, &coloredBackground, []byte{105, 60, 40, 10, 20, 30}},
		{"premultiplied", &image.RGBA{Rect: image.Rect(0, 0, 2, 1), Stride: 8, Pix: []byte{64, 32, 16, 128, 0, 0, 0, 0}}, nil, []byte{64, 32, 16, 0, 0, 0}},
		{"premultiplied colored background", &image.RGBA{Rect: image.Rect(0, 0, 2, 1), Stride: 8, Pix: []byte{64, 32, 16, 128, 0, 0, 0, 0}}, &coloredBackground, []byte{68, 41, 30, 10, 20, 30}},
		{"premultiplied alpha2 black", &image.RGBA{Rect: image.Rect(0, 0, 1, 1), Stride: 4, Pix: []byte{1, 0, 0, 2}}, nil, []byte{1, 0, 0, 0}},
		{"premultiplied alpha2 colored", &image.RGBA{Rect: image.Rect(0, 0, 1, 1), Stride: 4, Pix: []byte{1, 0, 0, 2}}, &coloredBackground, []byte{10, 19, 29, 0}},
		{"padded stride", &image.NRGBA{Rect: image.Rect(9, 4, 11, 6), Stride: 12, Pix: []byte{1, 2, 3, 255, 4, 5, 6, 255, 99, 99, 99, 99, 7, 8, 9, 255, 10, 11, 12, 255}}, nil, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
		{"subimage offset", parent.SubImage(image.Rect(6, 8, 8, 9)), nil, []byte{100, 50, 25, 17, 33, 250}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := rgbCaptureOptions()
			if tt.background != nil {
				opts.Background = tt.background
			}
			file, err := BuildSecondaryCaptureRGBContext(context.Background(), tt.frame, opts)
			if err != nil {
				t.Fatal(err)
			}
			back := reopen(t, file)
			defer back.Close()
			raw, ok := back.Dataset.GetRaw(core.TagPixelData)
			if !ok || !bytes.Equal(raw, tt.want) {
				t.Fatalf("pixels %v, want %v", raw, tt.want)
			}
			element, _ := back.Dataset.Get(core.TagPixelData)
			if element.VR() != core.VROB {
				t.Fatalf("Pixel Data VR %v", element.VR())
			}
			frames, err := pixeldata.ExtractNativeFrames(back.Dataset)
			if err != nil || len(frames.Data) != 1 || frames.Metadata.SamplesPerPixel != 3 || frames.Metadata.BitsAllocated != 8 {
				t.Fatalf("native %+v err %v", frames, err)
			}
		})
	}
}

func TestSecondaryCaptureRGBNormativeTagsAndDetachedOwnership(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	frame.SetNRGBA(0, 0, color.NRGBA{R: 17, G: 33, B: 250, A: 255})
	opts := rgbCaptureOptions()
	file, err := BuildSecondaryCaptureRGBContext(context.Background(), frame, opts)
	if err != nil {
		t.Fatal(err)
	}
	frame.Pix[0] = 0
	opts.Derivation.Sources[0].Frames[0] = 99
	opts.Common.Source.Put(core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("CHANGED")))
	back := reopen(t, file)
	defer back.Close()
	if back.TransferSyntax != transfer.ExplicitVRLittleEndian {
		t.Fatal(back.TransferSyntax)
	}
	obj := back.Dataset
	if report, err := validation.ValidateFile(context.Background(), back.Meta.ToDataSet(), obj.ToDataSet(), back.TransferSyntax, validation.Options{Mode: validation.ModeStrict}); err != nil || report.HasErrors() {
		t.Fatalf("encoding validation %+v err %v", report, err)
	}
	if report, err := iod.Validate(obj, iod.Options{CheckValues: true}); err != nil || len(report.Errors()) != 0 {
		t.Fatalf("IOD validation %+v err %v", report.Errors(), err)
	}
	for tag, want := range map[core.Tag]string{
		core.NewTag(0x0008, 0x0016): "1.2.840.10008.5.1.4.1.1.7",
		core.NewTag(0x0008, 0x0018): opts.SOPInstanceUID, tagStudyInstanceUID: opts.StudyInstanceUID,
		core.NewTag(0x0020, 0x000e): opts.SeriesInstanceUID, tagModality: "OT", tagConversionType: "WSD",
		tagSeriesDescription: "Synthetic RGB capture", tagSeriesNumber: "900", tagInstanceNumber: "1",
		core.NewTag(0x0028, 0x0004): "RGB", core.NewTag(0x0028, 0x0301): "YES", core.NewTag(0x0028, 0x2110): "00",
		tagContentDate: "20261008", tagContentTime: "101213.123456", core.NewTag(0x0008, 0x2111): "Synthetic display capture",
		core.NewTag(0x0018, 0x1016): "Synthetic", core.NewTag(0x0018, 0x1018): "Test", core.NewTag(0x0018, 0x1019): "1",
		core.NewTag(0x0010, 0x0020): "SYN-1047",
	} {
		got, _ := obj.GetString(tag)
		if strings.TrimSpace(got) != want {
			t.Errorf("%s = %q, want %q", tag, got, want)
		}
	}
	if values, _ := obj.GetStrings(tagImageType); len(values) != 2 || values[0] != "DERIVED" || values[1] != "SECONDARY" {
		t.Fatalf("Image Type %v", values)
	}
	for tag, want := range map[core.Tag]int{
		core.NewTag(0x0028, 0x0010): 1, core.NewTag(0x0028, 0x0011): 1, core.NewTag(0x0028, 0x0002): 3,
		core.NewTag(0x0028, 0x0006): 0, core.NewTag(0x0028, 0x0100): 8, core.NewTag(0x0028, 0x0101): 8,
		core.NewTag(0x0028, 0x0102): 7, core.NewTag(0x0028, 0x0103): 0,
	} {
		got, err := obj.GetInt(tag)
		if err != nil || got != int64(want) {
			t.Errorf("%s = %d (%v), want %d", tag, got, err, want)
		}
	}
	for _, tag := range []core.Tag{tagFrameOfReferenceUID, tagImagePosition, tagImageOrientation, tagPixelSpacing, tagRescaleSlope, tagRescaleIntercept} {
		if obj.Has(tag) {
			t.Errorf("display capture unexpectedly carries %s", tag)
		}
	}
	if !obj.Has(tagPatientOrientation) {
		t.Fatal("Type 2 Patient Orientation absent")
	}
	refs, _ := obj.GetSequence(core.NewTag(0x0008, 0x2112))
	if len(refs) != 1 {
		t.Fatal("missing source reference")
	}
	if frames, _ := refs[0].GetInts(core.NewTag(0x0008, 0x1160)); len(frames) != 1 || frames[0] != 2 {
		t.Fatalf("detached frame reference %v", frames)
	}
	if raw, _ := obj.GetRaw(core.TagPixelData); !bytes.Equal(raw, []byte{17, 33, 250, 0}) {
		t.Fatal("input mutation affected output")
	}
}

func TestSecondaryCaptureRGBRejectsMalformedLayoutsAndOptions(t *testing.T) {
	var nilNRGBA *image.NRGBA
	var nilRGBA *image.RGBA
	valid := &image.NRGBA{Rect: image.Rect(0, 0, 1, 1), Stride: 4, Pix: []byte{1, 2, 3, 255}}
	for _, frame := range []image.Image{
		nil, nilNRGBA, nilRGBA, image.NewGray(image.Rect(0, 0, 1, 1)),
		&image.NRGBA{Rect: image.Rectangle{Min: image.Pt(math.MinInt, 0), Max: image.Pt(math.MaxInt, 1)}},
		&image.NRGBA{Rect: image.Rect(0, 0, 65535, 65535)},
		&image.NRGBA{Rect: image.Rect(0, 0, 2, 2), Stride: 7, Pix: make([]byte, 16)},
		&image.NRGBA{Rect: image.Rect(0, 0, 2, 2), Stride: 8, Pix: make([]byte, 15)},
		&image.NRGBA{Rect: image.Rect(0, 0, 1, 3), Stride: math.MaxInt, Pix: make([]byte, 4)},
		&image.RGBA{Rect: image.Rect(0, 0, 1, 1), Stride: 4, Pix: []byte{255, 0, 0, 128}},
	} {
		if file, err := BuildSecondaryCaptureRGBContext(context.Background(), frame, rgbCaptureOptions()); file != nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("malformed %T produced %v, %v", frame, file, err)
		}
	}
	for name, change := range map[string]func(*SecondaryCaptureRGBOptions){
		"missing background":     func(o *SecondaryCaptureRGBOptions) { o.Background = nil },
		"transparent background": func(o *SecondaryCaptureRGBOptions) { o.Background = &color.NRGBA{} },
		"annotation missing":     func(o *SecondaryCaptureRGBOptions) { o.BurnedInAnnotation = "" },
		"negative cap":           func(o *SecondaryCaptureRGBOptions) { o.MaxPixels = -1 },
		"bad UID":                func(o *SecondaryCaptureRGBOptions) { o.SOPInstanceUID = "not-a-uid" },
		"bad conversion":         func(o *SecondaryCaptureRGBOptions) { o.ConversionType = "BAD" },
		"bad reference":          func(o *SecondaryCaptureRGBOptions) { o.Derivation.Sources[0].SOPClassUID = "" },
		"oversized LO":           func(o *SecondaryCaptureRGBOptions) { o.SeriesDescription = strings.Repeat("x", 65) },
	} {
		t.Run(name, func(t *testing.T) {
			opts := rgbCaptureOptions()
			change(&opts)
			if file, err := BuildSecondaryCaptureRGBContext(context.Background(), valid, opts); file != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("file %v err %v", file, err)
			}
		})
	}
	opts := rgbCaptureOptions()
	opts.MaxPixels = 1
	if _, err := BuildSecondaryCaptureRGBContext(context.Background(), image.NewNRGBA(image.Rect(0, 0, 2, 1)), opts); !errors.Is(err, ErrInvalid) {
		t.Fatal("caller pixel cap not enforced")
	}
}

type rgbCancelContext struct {
	context.Context
	calls int
}

func (c *rgbCancelContext) Err() error {
	c.calls++
	if c.calls >= 3 {
		return context.Canceled
	}
	return nil
}

func TestSecondaryCaptureRGBCancellationWithinWideRow(t *testing.T) {
	ctx := &rgbCancelContext{Context: context.Background()}
	frame := image.NewNRGBA(image.Rect(0, 0, 65535, 1))
	if file, err := BuildSecondaryCaptureRGBContext(ctx, frame, rgbCaptureOptions()); file != nil || !errors.Is(err, context.Canceled) || ctx.calls != 3 {
		t.Fatalf("file %v err %v checks %d", file, err, ctx.calls)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildSecondaryCaptureRGBContext(canceled, nil, rgbCaptureOptions()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSecondaryCaptureRGBGeneratedUIDsAndStudyInheritance(t *testing.T) {
	opts := rgbCaptureOptions()
	opts.SOPInstanceUID, opts.SeriesInstanceUID, opts.StudyInstanceUID = "", "", ""
	file, err := BuildSecondaryCaptureRGBContext(nil, image.NewNRGBA(image.Rect(0, 0, 1, 1)), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []core.Tag{core.NewTag(0x0008, 0x0018), core.NewTag(0x0020, 0x000e), tagStudyInstanceUID} {
		uid, _ := file.Dataset.GetString(tag)
		if !core.IsValidUID(uid) {
			t.Fatalf("invalid generated/inherited UID %q", uid)
		}
	}
	study, _ := file.Dataset.GetString(tagStudyInstanceUID)
	sourceStudy, _ := opts.Common.Source.GetString(tagStudyInstanceUID)
	if study != sourceStudy {
		t.Fatal("source study not inherited")
	}
}
