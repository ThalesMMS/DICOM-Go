package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	redactRows    = 24
	redactColumns = 32
	redactFrames  = 3
	redactText    = 4000 // bright "burned-in text" value in a 12-bit image
)

// redactionBanner is the synthetic burned-in header band: bright text-like
// blocks drawn on rows 1..4 of every frame.
var redactionBanner = pixeldata.RedactionRegion{Frame: pixeldata.RedactionAllFrames, X: 2, Y: 1, Width: 20, Height: 4}

// syntheticTextImage builds a non-PHI 12-bit MONOCHROME2 multi-frame image
// whose background is a frame-dependent gradient and whose header band holds
// bright, glyph-like blocks.
func syntheticTextImage(t *testing.T) (*object.Object, [][]uint16) {
	t.Helper()
	obj := object.FromElements(nil, nil)
	putUS := func(group, element, value uint16) {
		raw := make([]byte, 2)
		binary.LittleEndian.PutUint16(raw, value)
		obj.Put(core.NewRawElement(core.NewTag(group, element), core.VRUS, raw))
	}
	obj.Put(core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.7\x00")))
	obj.Put(core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.3.4.5.6.7.8.9\x00")))
	putUS(0x0028, 0x0002, 1)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{strconv.Itoa(redactFrames)}})
	putUS(0x0028, 0x0010, redactRows)
	putUS(0x0028, 0x0011, redactColumns)
	putUS(0x0028, 0x0100, 16)
	putUS(0x0028, 0x0101, 12)
	putUS(0x0028, 0x0102, 11)
	putUS(0x0028, 0x0103, 0)
	frames := make([][]uint16, redactFrames)
	raw := make([]byte, 0, redactFrames*redactRows*redactColumns*2)
	for f := range frames {
		frames[f] = make([]uint16, redactRows*redactColumns)
		for y := 0; y < redactRows; y++ {
			for x := 0; x < redactColumns; x++ {
				value := uint16(100 + 7*x + 11*y + 50*f)
				// Glyph-like blocks: 3-pixel-wide strokes with 1-pixel gaps.
				if y >= 1 && y < 5 && x >= 2 && x < 22 && (x-2)%4 != 3 {
					value = redactText
				}
				frames[f][y*redactColumns+x] = value
				raw = binary.LittleEndian.AppendUint16(raw, value)
			}
		}
	}
	obj.Put(core.NewRawElement(core.TagPixelData, core.VROW, raw))
	return obj, frames
}

// expectedRedaction returns frames with every pixel inside regions set to fill.
func expectedRedaction(frames [][]uint16, fill uint16, regions ...pixeldata.RedactionRegion) [][]uint16 {
	out := make([][]uint16, len(frames))
	for f := range frames {
		out[f] = append([]uint16(nil), frames[f]...)
		for _, region := range regions {
			if region.Frame != pixeldata.RedactionAllFrames && region.Frame != f {
				continue
			}
			for y := region.Y; y < region.Y+region.Height; y++ {
				for x := region.X; x < region.X+region.Width; x++ {
					out[f][y*redactColumns+x] = fill
				}
			}
		}
	}
	return out
}

func redactionCodecs(t *testing.T) (pixeldata.Registry, *pixeldata.MemoryEncoderRegistry) {
	t.Helper()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	for _, register := range []func(pixeldata.EncoderRegistry) error{rle.RegisterEncoder, jpegls.RegisterEncoder, jpeglossless.RegisterEncoder} {
		if err := register(encoders); err != nil {
			t.Fatal(err)
		}
	}
	return decoders, encoders
}

func decodedFrames(t *testing.T, obj *object.Object, syntax transfer.Syntax, decoders pixeldata.Registry) [][]uint16 {
	t.Helper()
	native := obj
	if syntax.Encapsulated {
		var err error
		native, _, err = pixeldata.DecompressDataSet(obj, syntax, pixeldata.DecompressOptions{Registry: decoders})
		if err != nil {
			t.Fatalf("decode redacted output: %v", err)
		}
	}
	frames, err := pixeldata.ExtractNativeFramesView(native)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]uint16, len(frames.Data))
	for f, frame := range frames.Data {
		out[f] = make([]uint16, len(frame)/2)
		for i := range out[f] {
			out[f][i] = binary.LittleEndian.Uint16(frame[i*2:])
		}
	}
	return out
}

func assertFramesEqual(t *testing.T, got, want [][]uint16) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("frames = %d, want %d", len(got), len(want))
	}
	for f := range want {
		for i := range want[f] {
			if got[f][i] != want[f][i] {
				t.Fatalf("frame %d pixel (%d,%d) = %d, want %d", f, i%redactColumns, i/redactColumns, got[f][i], want[f][i])
			}
		}
	}
}

func TestRedactDataSetZeroesRegionsAndKeepsOtherPixelsForLosslessSources(t *testing.T) {
	decoders, encoders := redactionCodecs(t)
	source, frames := syntheticTextImage(t)
	want := expectedRedaction(frames, 0, redactionBanner)
	for _, syntax := range []transfer.Syntax{
		transfer.ExplicitVRLittleEndian,
		transfer.ImplicitVRLittleEndian,
		transfer.RLELossless,
		transfer.JPEGLSLossless,
		transfer.JPEGLosslessSV1,
	} {
		t.Run(syntax.Name, func(t *testing.T) {
			encoded := source
			if syntax.Encapsulated {
				var err error
				encoded, _, err = pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, syntax, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotPixelData(t, encoded)
			output, outputSyntax, report, err := pixeldata.RedactDataSet(context.Background(), encoded, syntax, []pixeldata.RedactionRegion{redactionBanner}, pixeldata.RedactionOptions{
				DecoderRegistry: decoders, EncoderRegistry: encoders, Output: pixeldata.RedactionOutputLosslessOnly,
			})
			if err != nil {
				t.Fatal(err)
			}
			if outputSyntax.UID != syntax.UID || report.OutputTransferSyntaxUID != syntax.UID || report.DecodedToNative {
				t.Fatalf("output syntax = %s (report %+v), want lossless %s", outputSyntax.UID, report, syntax.UID)
			}
			if syntax.Encapsulated != report.Reencoded {
				t.Fatalf("Reencoded = %t, want %t", report.Reencoded, syntax.Encapsulated)
			}
			if report.Frames != redactFrames || report.FramesCovered != redactFrames || report.Regions != redactFrames || report.PixelsRedacted != int64(redactFrames*20*4) {
				t.Fatalf("coverage = %+v", report)
			}
			assertFramesEqual(t, decodedFrames(t, output, outputSyntax, decoders), want)
			if !bytes.Equal(before, snapshotPixelData(t, encoded)) {
				t.Fatal("RedactDataSet mutated its source")
			}
		})
	}
}

func snapshotPixelData(t *testing.T, obj *object.Object) []byte {
	t.Helper()
	element, ok := obj.Get(core.TagPixelData)
	if !ok {
		t.Fatal("missing Pixel Data")
	}
	if raw, ok := element.RawBytes(); ok {
		return append([]byte(nil), raw...)
	}
	pixel, err := pixeldata.ExtractView(obj)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, fragment := range pixel.Sequence.Fragments {
		out = append(out, fragment...)
	}
	return out
}

func TestRedactDataSetPerFrameRegionsAndConfiguredFill(t *testing.T) {
	decoders, encoders := redactionCodecs(t)
	source, frames := syntheticTextImage(t)
	regions := []pixeldata.RedactionRegion{
		{Frame: 0, X: 0, Y: 0, Width: 4, Height: 2},
		{Frame: 2, X: 28, Y: 20, Width: 4, Height: 4},
	}
	output, _, report, err := pixeldata.RedactDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, regions, pixeldata.RedactionOptions{
		DecoderRegistry: decoders, EncoderRegistry: encoders, Fill: 77,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.FramesCovered != 2 || report.AllFramesCovered() {
		t.Fatalf("coverage = %+v, want 2 of 3 frames", report.RedactionCoverage)
	}
	assertFramesEqual(t, decodedFrames(t, output, transfer.ExplicitVRLittleEndian, decoders), expectedRedaction(frames, 77, regions...))
}

func TestRedactDataSetStoresLossySourcesNativeAndNeverReencodesLossily(t *testing.T) {
	decoders, encoders := redactionCodecs(t)
	if err := jpegls.RegisterNearLosslessEncoder(encoders, jpegls.EncoderOptions{Near: 2, AllowLossy: true}); err != nil {
		t.Fatal(err)
	}
	source, _ := syntheticTextImage(t)
	lossy, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, pixeldata.TranscodeOptions{EncoderRegistry: encoders, AllowLossy: true})
	if err != nil {
		t.Fatal(err)
	}
	decodedSource := decodedFrames(t, lossy, transfer.JPEGLSNearLossless, decoders)
	regions := []pixeldata.RedactionRegion{redactionBanner}

	output, syntax, report, err := pixeldata.RedactDataSet(context.Background(), lossy, transfer.JPEGLSNearLossless, regions, pixeldata.RedactionOptions{DecoderRegistry: decoders, EncoderRegistry: encoders})
	if err != nil {
		t.Fatal(err)
	}
	if syntax.UID != transfer.ExplicitVRLittleEndian.UID || !report.DecodedToNative || report.Reencoded {
		t.Fatalf("lossy source output = %s %+v, want native", syntax.UID, report)
	}
	// Redaction itself adds no loss: the unredacted pixels equal the decoded
	// lossy source exactly.
	assertFramesEqual(t, decodedFrames(t, output, syntax, decoders), expectedRedaction(decodedSource, 0, regions...))
	if value, _ := output.GetString(core.NewTag(0x0028, 0x2110)); value != "01" {
		t.Fatalf("Lossy Image Compression = %q, want 01 kept on native output", value)
	}

	_, _, _, err = pixeldata.RedactDataSet(context.Background(), lossy, transfer.JPEGLSNearLossless, regions, pixeldata.RedactionOptions{
		DecoderRegistry: decoders, EncoderRegistry: encoders, Output: pixeldata.RedactionOutputLosslessOnly,
	})
	if !errors.Is(err, pixeldata.ErrRedactionNotLossless) {
		t.Fatalf("LosslessOnly lossy source error = %v, want ErrRedactionNotLossless", err)
	}
}

func TestRedactDataSetWithoutEncoderStoresNative(t *testing.T) {
	decoders, encoders := redactionCodecs(t)
	source, frames := syntheticTextImage(t)
	encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.RLELossless, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []pixeldata.RedactionOptions{
		{DecoderRegistry: decoders},
		{DecoderRegistry: decoders, EncoderRegistry: encoders, Output: pixeldata.RedactionOutputNative},
	} {
		output, syntax, report, err := pixeldata.RedactDataSet(context.Background(), encoded, transfer.RLELossless, []pixeldata.RedactionRegion{redactionBanner}, options)
		if err != nil {
			t.Fatal(err)
		}
		if syntax.UID != transfer.ExplicitVRLittleEndian.UID || !report.DecodedToNative {
			t.Fatalf("output = %s %+v, want native", syntax.UID, report)
		}
		assertFramesEqual(t, decodedFrames(t, output, syntax, decoders), expectedRedaction(frames, 0, redactionBanner))
	}
	_, _, _, err = pixeldata.RedactDataSet(context.Background(), encoded, transfer.RLELossless, []pixeldata.RedactionRegion{redactionBanner}, pixeldata.RedactionOptions{DecoderRegistry: decoders, Output: pixeldata.RedactionOutputLosslessOnly})
	if !errors.Is(err, pixeldata.ErrRedactionNotLossless) {
		t.Fatalf("LosslessOnly without encoder = %v, want ErrRedactionNotLossless", err)
	}
}

func TestRedactDataSetRejectsInvalidRequests(t *testing.T) {
	source, _ := syntheticTextImage(t)
	before := snapshotPixelData(t, source)
	cases := []struct {
		name    string
		regions []pixeldata.RedactionRegion
		fill    int64
	}{
		{name: "no regions"},
		{name: "outside columns", regions: []pixeldata.RedactionRegion{{X: 30, Y: 0, Width: 3, Height: 1}}},
		{name: "outside rows", regions: []pixeldata.RedactionRegion{{X: 0, Y: 23, Width: 1, Height: 2}}},
		{name: "negative origin", regions: []pixeldata.RedactionRegion{{X: -1, Y: 0, Width: 2, Height: 2}}},
		{name: "empty rectangle", regions: []pixeldata.RedactionRegion{{X: 1, Y: 1, Width: 0, Height: 2}}},
		{name: "frame outside", regions: []pixeldata.RedactionRegion{{Frame: redactFrames, Width: 1, Height: 1}}},
		{name: "fill above bits stored", regions: []pixeldata.RedactionRegion{redactionBanner}, fill: 4096},
		{name: "negative fill unsigned", regions: []pixeldata.RedactionRegion{redactionBanner}, fill: -1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := pixeldata.RedactDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, test.regions, pixeldata.RedactionOptions{Fill: test.fill})
			if !errors.Is(err, pixeldata.ErrInvalidRedaction) {
				t.Fatalf("error = %v, want ErrInvalidRedaction", err)
			}
		})
	}
	if !bytes.Equal(before, snapshotPixelData(t, source)) {
		t.Fatal("rejected redaction mutated the source")
	}
}

func TestRedactDataSetSignedFill(t *testing.T) {
	source, _ := syntheticTextImage(t)
	raw := make([]byte, 2)
	binary.LittleEndian.PutUint16(raw, 1)
	source.Put(core.NewRawElement(core.NewTag(0x0028, 0x0103), core.VRUS, raw))
	output, _, _, err := pixeldata.RedactDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, []pixeldata.RedactionRegion{{Frame: 1, X: 0, Y: 0, Width: 1, Height: 1}}, pixeldata.RedactionOptions{Fill: -2048})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := pixeldata.ExtractNativeFramesView(output)
	if err != nil {
		t.Fatal(err)
	}
	if got := int16(binary.LittleEndian.Uint16(frames.Data[1])); got != -2048 {
		t.Fatalf("signed fill = %d, want -2048", got)
	}
}

func TestValidateRedactionRegionsCoverage(t *testing.T) {
	metadata := pixeldata.Metadata{Rows: 10, Columns: 10, NumberOfFrames: 4}
	coverage, err := pixeldata.ValidateRedactionRegions(metadata, []pixeldata.RedactionRegion{{Frame: pixeldata.RedactionAllFrames, Width: 10, Height: 2}, {Frame: 1, Width: 1, Height: 1}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.AllFramesCovered() || coverage.Regions != 5 {
		t.Fatalf("coverage = %+v", coverage)
	}
	if _, err := pixeldata.ValidateRedactionRegions(metadata, make([]pixeldata.RedactionRegion, 3), 2); !errors.Is(err, pixeldata.ErrInvalidRedaction) {
		t.Fatalf("too many regions = %v", err)
	}
}
