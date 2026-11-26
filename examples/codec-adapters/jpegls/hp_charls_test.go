//go:build (jpegls_charls || codecfull) && (darwin || linux || windows)

package jpegls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestCharLSHPColorTransforms(t *testing.T) {
	if err := ValidateQualifiedRuntime(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..", "..", "pixeldata", "codecfixture", "testdata", "codecs", "jpegls-hp")
	decoder := NewCharLSDecoder()
	zero := uint16(0)
	for _, bits := range []int{8, 16} {
		for _, hp := range []int{1, 2, 3} {
			for _, ilv := range []int{1, 2} {
				t.Run(fmt.Sprintf("rgb%d/hp%d/ilv%d", bits, hp, ilv), func(t *testing.T) {
					prefix := fmt.Sprintf("rgb%d-33x17", bits)
					stream, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("%s-hp%d-ilv%d.jls", prefix, hp, ilv)))
					if err != nil {
						t.Fatal(err)
					}
					want, err := os.ReadFile(filepath.Join(root, prefix+".raw"))
					if err != nil {
						t.Fatal(err)
					}
					obj, pixel := jpeglsObject(t, jpeglsMetadataOptions{rows: 17, columns: 33, samplesPerPixel: 3, bitsAllocated: uint16(bits), photometric: "RGB", planarConfiguration: &zero}, stream)
					defer obj.Close()
					codec := NewLossless(decoder)
					if got, err := codec.Decode(pixel, obj); !errors.Is(err, ErrUnsupportedMetadata) || len(got.Data) != 0 {
						t.Fatalf("strict frames=%d error=%v", len(got.Data), err)
					}
					ctx := pixeldata.DecodeContextWithRecovery(context.Background(), pixeldata.DecodeRecovery{JPEGLSHPColorTransforms: true})
					got, err := codec.DecodeContext(ctx, pixel, obj)
					if err != nil {
						t.Fatal(err)
					}
					diagnostic := []string{"", pixeldata.DiagnosticJPEGLSHP1, pixeldata.DiagnosticJPEGLSHP2, pixeldata.DiagnosticJPEGLSHP3}[hp]
					if len(got.Data) != 1 || !bytes.Equal(got.Data[0], want) || !slices.Equal(got.Diagnostics, []string{diagnostic, pixeldata.DiagnosticJPEGLSHPBackendCharLS}) {
						t.Fatalf("independent CharLS2.4.2 RGB/diagnostic mismatch: %v", got.Diagnostics)
					}
					if got, err := NewNearLossless(decoder).DecodeContext(ctx, pixel, obj); !errors.Is(err, ErrUnsupportedMetadata) || len(got.Data) != 0 {
						t.Fatal("HP accepted under .81")
					}
					metadata, err := pixeldata.ExtractMetadata(obj)
					if err != nil {
						t.Fatal(err)
					}
					badHighBit := metadata
					badHighBit.HighBit--
					if got, err := decoder.DecodeJPEGLS(stream, DecoderInput{Metadata: badHighBit, AllowHPColorTransforms: true}); !errors.Is(err, ErrUnsupportedMetadata) || got != nil {
						t.Fatalf("direct mismatched HighBit error=%v", err)
					}
					metadata.BitsStored--
					metadata.HighBit--
					if got, err := decoder.DecodeJPEGLS(stream, DecoderInput{Metadata: metadata, AllowHPColorTransforms: true}); !errors.Is(err, ErrUnsupportedMetadata) || got != nil {
						t.Fatalf("partial precision error=%v", err)
					}
				})
			}
		}
	}
}
