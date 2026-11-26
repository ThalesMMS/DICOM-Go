package frame_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/pixeldata/frame"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The issue's signed 13-bit profile (BitsAllocated 16, HighBit 12): raw 0x1830
// is -2000. Rescale slope 2 and intercept -1024 make a single rescale
// distinguishable from none and from two.
const (
	pathRows, pathColumns = 2, 4
	pathSlope             = 2.0
	pathIntercept         = -1024.0
	pathCenter, pathWidth = 0.0, 12000.0
)

var pathExpected = []int64{-4096, -2000, -2000, 4095, 4095, 0, -1, 2000}

// pathRaw holds bits above HighBit in two samples (0xF830 and 0x2FFF), which
// storage interpretation must ignore.
var pathRaw = []uint16{0x1000, 0x1830, 0xF830, 0x0FFF, 0x2FFF, 0x0000, 0x1FFF, 0x07D0}

// pathClean is pathRaw without those bits, for encoders that take only
// BitsStored-bit samples.
var pathClean = []uint16{0x1000, 0x1830, 0x1830, 0x0FFF, 0x0FFF, 0x0000, 0x1FFF, 0x07D0}

func pathElements(pixels []uint16) []core.Element {
	us := func(group, element uint16, v uint16) core.Element {
		return dicomtest.Uint16Element(core.NewTag(group, element), core.VRUS, binary.LittleEndian, v)
	}
	raw := make([]byte, 0, 2*len(pixels))
	for _, v := range pixels {
		raw = binary.LittleEndian.AppendUint16(raw, v)
	}
	return []core.Element{
		dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.935"),
		us(0x0028, 0x0002, 1),
		dicomtest.StringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		us(0x0028, 0x0010, pathRows),
		us(0x0028, 0x0011, pathColumns),
		us(0x0028, 0x0100, 16),
		us(0x0028, 0x0101, 13),
		us(0x0028, 0x0102, 12),
		us(0x0028, 0x0103, 1),
		dicomtest.BytesElement(core.NewTag(0x0028, 0x0120), core.VRSS, []byte{0x00, 0xF0}), // SS -4096
		dicomtest.StringElement(core.NewTag(0x0028, 0x1052), core.VRDS, "-1024"),
		dicomtest.StringElement(core.NewTag(0x0028, 0x1053), core.VRDS, "2"),
		dicomtest.BytesElement(core.TagPixelData, core.VROW, raw),
	}
}

func pathFile(t *testing.T, syntax transfer.Syntax, pixels []uint16) *object.File {
	t.Helper()
	data, err := dicomtest.Part10File(syntax, pathElements(pixels)...)
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func transcoded(t *testing.T, file *object.File, target transfer.Syntax, encoders pixeldata.EncoderRegistry) *object.File {
	t.Helper()
	dataset, _, err := pixeldata.TranscodeDataSet(context.Background(), file.Dataset, file.TransferSyntax, target, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
	if err != nil {
		t.Fatal(err)
	}
	return &object.File{Dataset: dataset, TransferSyntax: target}
}

// decodedFrames decodes an encapsulated file through a codec, not through
// transcoding, so the frames are the codec's own output.
func decodedFrames(t *testing.T, file *object.File) []*frame.Frame {
	t.Helper()
	registry, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	pixels, err := pixeldata.ExtractView(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := registry.DecodeFrames(file.TransferSyntax.UID, pixels, file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := pixeldata.ExtractMetadata(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	return frame.FromDecodedFrames(metadata, decoded.Data, true, frame.WithByteOrder(binary.LittleEndian), frame.WithRescale(pathSlope, pathIntercept), frame.WithWindow(pathCenter, pathWidth))
}

func TestStoredSamplesAgreeAcrossNativeCodecStatisticsAndRendering(t *testing.T) {
	le := pathFile(t, transfer.ExplicitVRLittleEndian, pathRaw)
	rleEncoders := pixeldata.NewMemoryEncoderRegistry()
	if err := rle.RegisterEncoder(rleEncoders); err != nil {
		t.Fatal(err)
	}
	lsEncoders := pixeldata.NewMemoryEncoderRegistry()
	if err := jpegls.RegisterEncoder(lsEncoders); err != nil {
		t.Fatal(err)
	}
	extract := func(file *object.File) []*frame.Frame {
		frames, err := frame.ExtractFrames(file, frame.WithWindow(pathCenter, pathWidth))
		if err != nil {
			t.Fatal(err)
		}
		return frames
	}
	paths := map[string]struct {
		file   *object.File
		frames []*frame.Frame
	}{
		"native explicit little-endian": {le, extract(le)},
		"native implicit little-endian": func() (p struct {
			file   *object.File
			frames []*frame.Frame
		}) {
			p.file = pathFile(t, transfer.ImplicitVRLittleEndian, pathRaw)
			p.frames = extract(p.file)
			return p
		}(),
		"native explicit big-endian": func() (p struct {
			file   *object.File
			frames []*frame.Frame
		}) {
			p.file = transcoded(t, le, transfer.ExplicitVRBigEndian, nil)
			p.frames = extract(p.file)
			return p
		}(),
		"RLE Lossless decoded": func() (p struct {
			file   *object.File
			frames []*frame.Frame
		}) {
			p.file = transcoded(t, le, transfer.RLELossless, rleEncoders)
			p.frames = decodedFrames(t, p.file)
			return p
		}(),
		"JPEG-LS Lossless decoded": func() (p struct {
			file   *object.File
			frames []*frame.Frame
		}) {
			p.file = transcoded(t, pathFile(t, transfer.ExplicitVRLittleEndian, pathClean), transfer.JPEGLSLossless, lsEncoders)
			p.frames = decodedFrames(t, p.file)
			return p
		}(),
	}
	ctx := context.Background()
	reference := renderInterpreted(t, pathExpected)
	for name, path := range paths {
		if len(path.frames) != 1 {
			t.Fatalf("%s: %d frames", name, len(path.frames))
		}
		data, format, err := path.frames[0].StoredSampleData()
		if err != nil {
			t.Fatal(err)
		}
		// Access: every sample, once, without rescale.
		stored, err := pixeldata.StoredSamples[int16](ctx, data, format, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, want := range pathExpected {
			if int64(stored[i]) != want {
				t.Fatalf("%s: stored sample %d = %d, want %d", name, i, stored[i], want)
			}
		}
		// Statistics: Pixel Padding read from the data set in the format's
		// signedness (SS; resolved from the implicit VR data set too).
		padding, err := pixeldata.PixelPaddingFromObject(path.file.Dataset, format)
		if err != nil || padding == nil || padding.Value != -4096 {
			t.Fatalf("%s: padding %+v %v", name, padding, err)
		}
		stats, err := pixeldata.ComputeStoredSampleStatistics(ctx, data, format, padding)
		if err != nil || stats.Samples != 7 || stats.Padding != 1 || stats.Minimum != -2000 || stats.Maximum != 4095 || stats.Mean != float64(-2000-2000+4095+4095+0-1+2000)/7 {
			t.Fatalf("%s: statistics %+v %v", name, stats, err)
		}
		// Modality: exactly one rescale, after exactly one sign extension.
		modality, err := display.DecodeModality(display.Frame{
			Rows: pathRows, Columns: pathColumns, Pixels: data,
			Format:   display.PixelFormat{BitsAllocated: format.BitsAllocated, BitsStored: format.BitsStored, HighBit: format.HighBit, Signed: format.Signed, ByteOrder: format.ByteOrder},
			Modality: display.ModalityLUT{Slope: pathSlope, Intercept: pathIntercept},
		})
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range pathExpected {
			single := float64(want)*pathSlope + pathIntercept
			if modality[i] != single {
				double := single*pathSlope + pathIntercept
				t.Fatalf("%s: modality %d = %v, want %v (double rescale would be %v)", name, i, modality[i], single, double)
			}
		}
		// Render sampling reports the same stored and rescaled values.
		metadata := pixeldata.Metadata{Rows: pathRows, Columns: pathColumns, SamplesPerPixel: 1, BitsAllocated: 16, BitsStored: 13, HighBit: 12, PixelRepresentation: 1}
		for i, want := range pathExpected {
			value, ok := render.SamplePixelValue(&metadata, format.ByteOrder, data, render.Rescale{Slope: pathSlope, Intercept: pathIntercept}, i%pathColumns, i/pathColumns)
			if !ok || int64(value.Stored) != want || value.Rescaled != float64(want)*pathSlope+pathIntercept {
				t.Fatalf("%s: render sample %d = %+v", name, i, value)
			}
		}
		// Presentation: the stored bytes render exactly like the interpreted
		// samples re-encoded as full 16-bit signed values.
		img, err := path.frames[0].GetImage()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(grayPix(t, img), reference) {
			t.Fatalf("%s: rendering differs from interpreted samples", name)
		}
	}
}

// renderInterpreted renders already interpreted samples stored as full 16-bit
// signed values, with the same modality and window as the paths.
func renderInterpreted(t *testing.T, samples []int64) []byte {
	t.Helper()
	pixels := make([]byte, 0, 2*len(samples))
	for _, v := range samples {
		pixels = binary.LittleEndian.AppendUint16(pixels, uint16(int16(v)))
	}
	img, err := display.RenderGray(display.Frame{
		Rows: pathRows, Columns: pathColumns, Pixels: pixels,
		Format:   display.PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, Signed: true},
		Modality: display.ModalityLUT{Slope: pathSlope, Intercept: pathIntercept},
		VOI:      display.VOILUT{Center: pathCenter, Width: pathWidth},
	})
	if err != nil {
		t.Fatal(err)
	}
	return img.Pix
}

func grayPix(t *testing.T, img image.Image) []byte {
	t.Helper()
	gray, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("image is %T", img)
	}
	return gray.Pix
}
