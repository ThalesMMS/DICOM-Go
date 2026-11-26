package pixeldata_test

import (
	"context"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Hide only the new capability to measure the actual existing RLE allocating
// frame/regional codec paths against the same encoded bytes.
type callerBufferLegacyRLE struct{ codec *rle.Codec }

func (c callerBufferLegacyRLE) Decode(p pixeldata.PixelData, o *object.Object) (pixeldata.Frames, error) {
	return c.codec.Decode(p, o)
}
func (c callerBufferLegacyRLE) DecodeContext(ctx context.Context, p pixeldata.PixelData, o *object.Object) (pixeldata.Frames, error) {
	return c.codec.DecodeContext(ctx, p, o)
}
func (c callerBufferLegacyRLE) DecodeFrameRegion(ctx context.Context, b []byte, m pixeldata.Metadata, rect image.Rectangle) (pixeldata.Frames, error) {
	return c.codec.DecodeFrameRegion(ctx, b, m, rect)
}

// Embed only the allocating interfaces so FrameInto uses the existing copy path.
type callerBufferLegacyJPEGLS struct {
	pixeldata.Codec
	pixeldata.ContextCodec
}

// BenchmarkFrameReaderCallerBuffers excludes constructor, encoding and the
// caller's reusable destination allocation. Frame count is 32; operations rotate
// frames. Codec codestream/scratch and allocated fallback output stay timed.
func BenchmarkFrameReaderCallerBuffers(b *testing.B) {
	for _, target := range []struct {
		name   string
		syntax transfer.Syntax
	}{{"native-le", transfer.ExplicitVRLittleEndian}, {"native-be", transfer.ExplicitVRBigEndian}, {"rle-direct", transfer.RLELossless}, {"rle-copy", transfer.RLELossless}, {"jls-direct", transfer.JPEGLSLossless}, {"jls-copy", transfer.JPEGLSLossless}} {
		r := intoFixture(b, 16, 1, 0, target.syntax, 256, 256, 32)
		var legacyCodec pixeldata.Codec
		switch target.name {
		case "rle-copy":
			legacyCodec = callerBufferLegacyRLE{codec: rle.New()}
		case "jls-copy":
			codec := jpegls.New()
			legacyCodec = callerBufferLegacyJPEGLS{Codec: codec, ContextCodec: codec}
		}
		if legacyCodec != nil {
			registry := pixeldata.NewMemoryRegistry()
			if err := registry.RegisterCodec(target.syntax.UID, legacyCodec); err != nil {
				b.Fatal(err)
			}
			var err error
			r, err = pixeldata.NewFrameReader(nil, r.File(), nil, pixeldata.FrameReaderOptions{Registry: registry})
			if err != nil {
				b.Fatal(err)
			}
		}
		ctx := context.Background()
		rect := image.Rect(43, 61, 107, 125)
		// Warm codec metadata initialization outside every timed pair.
		if _, _, err := r.Frame(ctx, 0); err != nil {
			b.Fatal(err)
		}
		for _, regional := range []bool{false, true} {
			kind, size := "frame", 256*256*2
			if regional {
				kind, size = "region", 64*64*2
			}
			for _, into := range []bool{false, true} {
				api := "allocated"
				if into {
					api = "into"
				}
				b.Run(target.name+"/"+kind+"/"+api, func(b *testing.B) {
					dst := make([]byte, size)
					if into {
						var result pixeldata.BufferResult
						var err error
						if regional {
							result, err = r.RegionInto(ctx, 0, rect, dst)
						} else {
							result, err = r.FrameInto(ctx, 0, dst)
						}
						expected := pixeldata.BufferNativeDirect
						switch target.name {
						case "rle-direct":
							expected = pixeldata.BufferCodecDirect
						case "rle-copy":
							expected = pixeldata.BufferFrameCopy
							if regional {
								expected = pixeldata.BufferRegionCopy
							}
						case "jls-direct":
							expected = pixeldata.BufferCodecDirect
						case "jls-copy":
							expected = pixeldata.BufferFrameCopy
							if regional {
								expected = pixeldata.BufferFrameRegionCopy
							}
						}
						if err != nil || result.Layout.Mode != expected || result.Written != size {
							b.Fatalf("actual path %+v, %v", result, err)
						}
					}

					b.ReportAllocs()
					b.SetBytes(int64(size))
					b.ResetTimer()
					for turn := 0; turn < b.N; turn++ {
						i := turn % 32
						if into {
							if regional {
								if _, err := r.RegionInto(ctx, i, rect, dst); err != nil {
									b.Fatal(err)
								}
							} else {
								if _, err := r.FrameInto(ctx, i, dst); err != nil {
									b.Fatal(err)
								}
							}
						} else {
							if regional {
								if _, _, err := r.Region(ctx, i, rect); err != nil {
									b.Fatal(err)
								}
							} else {
								if _, _, err := r.Frame(ctx, i); err != nil {
									b.Fatal(err)
								}
							}
						}
					}
				})
			}
		}
	}
}
