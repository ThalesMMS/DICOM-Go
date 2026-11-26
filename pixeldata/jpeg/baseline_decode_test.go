package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func baselineDecodeFixture(t testing.TB, width, height int, colorFrame bool) []byte {
	t.Helper()
	var img image.Image
	if colorFrame {
		frame := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				frame.SetRGBA(x, y, color.RGBA{R: byte(x*9 + y*3), G: byte(x*5 + y*11), B: byte(x*x + y*7), A: 255})
			}
		}
		img = frame
	} else {
		frame := image.NewGray(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				frame.Pix[y*frame.Stride+x] = byte((x*13 + y*7 + (x*y)%37) % 256)
			}
		}
		img = frame
	}
	var out bytes.Buffer
	if err := stdjpeg.Encode(&out, img, &stdjpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Frozen pre-change standard-library gate and decode for error/pixel parity.
// The generic At/color-conversion helper remains unchanged in production.
func legacyBaselineDecodeImage(stream []byte, metadata pixeldata.Metadata, frameIndex int) (image.Image, error) {
	config, err := stdjpeg.DecodeConfig(bytes.NewReader(stream))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidFragment, err)
	}
	if config.Width != int(metadata.Columns) || config.Height != int(metadata.Rows) {
		return nil, ErrImageSizeMismatch
	}
	img, err := stdjpeg.Decode(bytes.NewReader(stream))
	if err != nil {
		return nil, fmt.Errorf("%w: frame %d: %w", ErrInvalidFragment, frameIndex, err)
	}
	return img, nil
}

func TestBaselineGrayRowsMatchLegacyAndDetach(t *testing.T) {
	parent := image.NewGray(image.Rect(-7, 3, 31, 24))
	for i := range parent.Pix {
		parent.Pix[i] = byte(i*17 + 5)
	}
	for _, gray := range []*image.Gray{image.NewGray(image.Rect(0, 0, 17, 13)), parent.SubImage(image.Rect(-3, 6, 14, 19)).(*image.Gray)} {
		metadata := baselineMetadata(13, 17, 1)
		before := bytes.Clone(gray.Pix)
		want, err := imageToFrameBytes(gray, metadata)
		if err != nil {
			t.Fatal(err)
		}
		for _, representation := range []uint16{0, 1} {
			metadata.BitsStored, metadata.HighBit, metadata.PixelRepresentation = 5, 4, representation
			got, err := baselineImageToFrameBytes(context.Background(), gray, metadata, pixeldata.JPEGColorGuess)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("copy changed visible samples: %v", err)
			}
			clampedWant := bytes.Clone(want)
			limitStoredBits8(got, metadata)
			limitStoredBits8(clampedWant, metadata)
			if !bytes.Equal(got, clampedWant) || !bytes.Equal(gray.Pix, before) {
				t.Fatal("stored-bit limiting changed source or samples")
			}
		}
	}
	if _, err := baselineImageToFrameBytes(context.Background(), &image.Gray{Pix: []byte{7}, Stride: 1, Rect: image.Rect(0, 0, 2, 1)}, baselineMetadata(1, 2, 1), pixeldata.JPEGColorGuess); !errors.Is(err, ErrInvalidFragment) {
		t.Fatalf("invalid Gray layout = %v", err)
	}
}

func TestBaselineDecodeGrayAndColorPoliciesByteExact(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {19, 17}, {129, 127}} {
		for _, colorFrame := range []bool{false, true} {
			stream := baselineDecodeFixture(t, size[0], size[1], colorFrame)
			for _, samples := range []uint16{1, 3} { // includes color→mono and gray→RGB fallback
				metadata := baselineMetadata(uint16(size[1]), uint16(size[0]), samples)
				for _, policy := range []pixeldata.JPEGColorConversion{pixeldata.JPEGColorGuess, pixeldata.JPEGColorNever, pixeldata.JPEGColorAlways, pixeldata.JPEGColorPhotometric} {
					metadata.PhotometricInterpretation = "YBR_FULL_422"
					header, err := probeBaselineHeader(context.Background(), stream)
					if err != nil || !header.admitted {
						t.Fatalf("SOF admission: %+v, %v", header, err)
					}
					legacy, err := legacyBaselineDecodeImage(stream, metadata, 0)
					if err != nil {
						t.Fatal(err)
					}
					want, err := imageToFrameBytesPolicy(legacy, metadata, policy)
					if err != nil {
						t.Fatal(err)
					}
					img, err := decodeBaselineImage(context.Background(), stream, metadata, header, 0)
					if err != nil {
						t.Fatal(err)
					}
					got, err := baselineImageToFrameBytes(context.Background(), img, metadata, policy)
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("%dx%d color%v samples%d policy%d differs: %v", size[0], size[1], colorFrame, samples, policy, err)
					}
				}
			}
		}
	}
}

func TestBaselineDecodeColorSubsamplingUnchanged(t *testing.T) {
	metadata := baselineMetadata(17, 19, 3)
	for _, sampling := range []Subsampling{Subsampling444, Subsampling422, Subsampling420} {
		metadata.PhotometricInterpretation = "RGB"
		encoder, _ := NewEncoderWithOptions(EncoderOptions{Quality: 90, Subsampling: sampling})
		encoded, err := encoder.EncodeFrame(context.Background(), baselinePhantom(19, 17, 3, "checker"), metadata)
		if err != nil {
			t.Fatal(err)
		}
		img, err := stdjpeg.Decode(bytes.NewReader(encoded.Data))
		if err != nil {
			t.Fatal(err)
		}
		for _, policy := range []pixeldata.JPEGColorConversion{pixeldata.JPEGColorGuess, pixeldata.JPEGColorNever, pixeldata.JPEGColorAlways, pixeldata.JPEGColorPhotometric} {
			for _, photometric := range []string{"RGB", "YBR_FULL_422"} {
				metadata.PhotometricInterpretation = photometric
				want, _ := imageToFrameBytesPolicy(img, metadata, policy)
				obj := object.FromElements(append(jpegMetadataElements(17, 19, 3, photometric, 1), dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, encoded.Data)), nil)
				pixel, _ := pixeldata.Extract(obj)
				got, err := newBaselineCodec().DecodeContext(pixeldata.WithJPEGColorConversion(context.Background(), policy), pixel, obj)
				if err != nil || len(got.Data) != 1 || !bytes.Equal(got.Data[0], want) {
					t.Fatalf("sampling%d policy%d photo%s: %v", sampling, policy, photometric, err)
				}
				_, ycc := img.(*image.YCbCr)
				if got.PhotometricInterpretation != outputPhotometric(policy, metadata, convertsYCbCr(policy, metadata, ycc)) {
					t.Fatal("photometric output changed")
				}
			}
		}
	}
}

func TestBaselineHeaderProbeParityAndSkipsAPPContents(t *testing.T) {
	stream := baselineDecodeFixture(t, 19, 17, false)
	for n := 0; n <= len(stream); n++ {
		marker, legacyErr := jpegFrameSOFMarker(context.Background(), stream[:n])
		header, err := probeBaselineHeader(context.Background(), stream[:n])
		if marker != header.sof || (err == nil) != (legacyErr == nil) || err != nil && err.Error() != legacyErr.Error() {
			t.Fatalf("prefix%d marker/error changed: %02x %v / %02x %v", n, marker, legacyErr, header.sof, err)
		}
	}
	// A fake SOF in an APP payload must never authorize its dimensions.
	fake := []byte{0xff, 0xe2, 0, 15, 0xff, 0xc0, 0, 11, 8, 0xff, 0xff, 0xff, 0xff, 1, 1, 0x11, 0}
	withAPP := append(append([]byte{0xff, 0xd8}, fake...), stream[2:]...)
	header, err := probeBaselineHeader(context.Background(), withAPP)
	if err != nil || header.width != 19 || header.height != 17 || !header.admitted {
		t.Fatalf("APP content became SOF: %+v %v", header, err)
	}
	filled := append([]byte{0xff, 0xd8, 0xff, 0xff}, stream[3:]...)
	if header, err := probeBaselineHeader(context.Background(), filled); err != nil || !header.admitted {
		t.Fatalf("marker fill = %+v %v", header, err)
	}
	for _, malformed := range [][]byte{{0xff, 0xd8, 0xff, 0}, {0xff, 0xd8, 0xff, 0xda}, {0xff, 0xd8, 0xff, 0xd0}, {0xff, 0xd8, 0xff, 0xe0, 0, 1}, {0xff, 0xd8, 0xff, 0xe0, 0, 30}} {
		if _, err := probeBaselineHeader(context.Background(), malformed); err == nil {
			t.Fatalf("accepted malformed prefix %x", malformed)
		}
	}
}

func TestBaselineDecodePreservesMalformedErrorsAndDimensionAdmission(t *testing.T) {
	base := baselineDecodeFixture(t, 19, 17, false)
	sof := baselineSOF(t, base)
	sofOffset := bytes.Index(base, sof)
	var variants [][]byte
	for _, change := range [][2]int{{0, 12}, {1, 0}, {3, 0}, {5, 2}, {7, 0}, {8, 9}} {
		modified := bytes.Clone(base)
		modified[sofOffset+change[0]] = byte(change[1])
		if change[0] == 1 || change[0] == 3 {
			modified[sofOffset+change[0]+1] = 0
		}
		variants = append(variants, modified)
	}
	// Truncated header is handled by the old config refusal path. A header
	// followed by malformed entropy still yields mismatch first when the
	// declared dimensions differ, including maximum 16-bit SOF dimensions.
	variants = append(variants, base[:sofOffset+4], base[:len(base)-3])
	for _, marker := range []byte{0xdb, 0xc4} {
		modified := bytes.Clone(base)
		offset := bytes.Index(modified, []byte{0xff, marker})
		if offset < 0 {
			t.Fatal("fixture table is absent")
		}
		modified[offset+4] = 0xff // invalid table selector, preserving segment length
		variants = append(variants, modified)
	}
	huge := bytes.Clone(base)
	binary.BigEndian.PutUint16(huge[sofOffset+1:], 65535)
	binary.BigEndian.PutUint16(huge[sofOffset+3:], 65535)
	variants = append(variants, huge)
	metadata := baselineMetadata(17, 19, 1)
	for index, stream := range variants {
		header, err := probeBaselineHeader(context.Background(), stream)
		if err != nil {
			continue // scanner rejection parity is covered separately
		}
		_, wantErr := legacyBaselineDecodeImage(stream, metadata, 3)
		img, gotErr := decodeBaselineImage(context.Background(), stream, metadata, header, 3)
		if wantErr == nil || gotErr == nil || img != nil || gotErr.Error() != wantErr.Error() {
			t.Fatalf("variant%d error changed: %v / %v", index, wantErr, gotErr)
		}
	}
	header, _ := probeBaselineHeader(context.Background(), huge)
	if img, err := decodeBaselineImage(context.Background(), huge, metadata, header, 0); img != nil || !errors.Is(err, ErrImageSizeMismatch) {
		t.Fatalf("huge SOF was not refused before image allocation: %v", err)
	}
}

func TestBaselineDecodeCancellationReturnsNoPartialFrame(t *testing.T) {
	stream := baselineDecodeFixture(t, 513, 257, false)
	metadata := baselineMetadata(257, 513, 1)
	header, _ := probeBaselineHeader(context.Background(), stream)
	for _, checks := range []int{1, 3, 5} {
		parent, cancel := context.WithCancel(context.Background())
		ctx := &baselineCancelContext{Context: parent, cancel: cancel, checks: checks}
		img, err := decodeBaselineImage(ctx, stream, metadata, header, 0)
		cancel()
		if img != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("decode cancellation %d: %v", checks, err)
		}
	}
	parent, cancel := context.WithCancel(context.Background())
	ctx := &baselineCancelContext{Context: parent, cancel: cancel, checks: 5}
	if out, err := baselineImageToFrameBytes(ctx, image.NewGray(image.Rect(0, 0, 513, 257)), metadata, pixeldata.JPEGColorGuess); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("row cancellation: %v", err)
	}
	cancel()
	parent, cancel = context.WithCancel(context.Background())
	ctx = &baselineCancelContext{Context: parent, cancel: cancel, checks: 40}
	obj := object.FromElements(append(jpegMetadataElements(257, 513, 1, "MONOCHROME2", 2), dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, stream, stream)), nil)
	pixel, _ := pixeldata.Extract(obj)
	result, err := newBaselineCodec().DecodeContext(ctx, pixel, obj)
	cancel()
	if len(result.Data) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("batch cancellation published partial frames: %v", err)
	}
}
