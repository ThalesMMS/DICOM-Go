package frame

import (
	"bytes"
	"context"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type jpegPolicyCodec struct{ policy pixeldata.JPEGColorConversion }

func (c jpegPolicyCodec) Decode(pixels pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	return jpeg.New().DecodeContext(pixeldata.WithJPEGColorConversion(context.Background(), c.policy), pixels, obj)
}

func TestExtractFramesUsesJPEGDecodedPhotometric(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	for x := 0; x < 2; x++ {
		img.Set(x, 0, color.RGBA{R: 240, G: 40, B: 10, A: 255})
	}
	var encoded bytes.Buffer
	if err := stdjpeg.Encode(&encoded, img, &stdjpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	previous := pixeldata.DefaultRegistry
	t.Cleanup(func() { pixeldata.DefaultRegistry = previous })
	for _, syntax := range []transfer.Syntax{transfer.JPEGBaseline, transfer.JPEGExtended} {
		for _, photometric := range []string{"YBR_FULL", "YBR_FULL_422"} {
			for _, tc := range []struct {
				name   string
				policy pixeldata.JPEGColorConversion
				want   string
			}{
				{"default", 0, "RGB"},
				{"never convert", pixeldata.JPEGColorNever, "YBR_FULL"},
			} {
				t.Run(syntax.Name+"/"+photometric+"/"+tc.name, func(t *testing.T) {
					registry := pixeldata.NewMemoryRegistry()
					if err := registry.RegisterCodec(syntax.UID, jpegPolicyCodec{tc.policy}); err != nil {
						t.Fatal(err)
					}
					pixeldata.DefaultRegistry = registry
					stream := encoded.Bytes()
					if syntax.UID == transfer.JPEGExtended.UID {
						stream = bytes.Replace(stream, []byte{0xff, 0xc0}, []byte{0xff, 0xc1}, 1)
					}
					elements := frameMetadataElements(1, 2, 8, 8, 7, 0, photometric)
					for i := range elements {
						if elements[i].Tag() == core.NewTag(0x0028, 0x0002) {
							elements[i] = dicomtest.Uint16Element(elements[i].Tag(), core.VRUS, nil, 3)
						}
					}
					elements = append(elements, dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, stream))
					file := &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: syntax}
					frames, err := ExtractFrames(file)
					if err != nil || len(frames) != 1 {
						t.Fatalf("ExtractFrames: %v, %d frames", err, len(frames))
					}
					metadata := frames[0].EncapsulatedData.Metadata
					if metadata.PhotometricInterpretation != tc.want || metadata.PlanarConfiguration != 0 || !metadata.PlanarConfigurationPresent {
						t.Fatalf("decoded metadata = %+v, want interleaved %s", metadata, tc.want)
					}
					if _, err := frames[0].GetImage(); err != nil {
						t.Fatalf("render decoded frame: %v", err)
					}
				})
			}
		}
	}
}

func TestDecodedFrameMetadataNormalizesJPEGPartialYBR(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.JPEGBaseline, transfer.JPEGExtended} {
		got := decodedFrameMetadata(pixeldata.Metadata{SamplesPerPixel: 3, PhotometricInterpretation: "YBR_PARTIAL_422"}, syntax.UID)
		if got.PhotometricInterpretation != "RGB" || got.PlanarConfiguration != 0 || !got.PlanarConfigurationPresent {
			t.Fatalf("%s fallback metadata: %+v", syntax.Name, got)
		}
	}
}
