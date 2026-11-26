package jpeg

import (
	"bytes"
	"crypto/sha256"
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

// This harness uses only APIs/helpers present before #1309, so its exact bytes
// also compile against the pinned pre-change archive. Fixture encoding and
// one output hash check occur outside the timed decode loop.
func baselineDecodeBenchStream(b *testing.B, size int, samples uint16) []byte {
	b.Helper()
	var source image.Image
	if samples == 1 {
		gray := image.NewGray(image.Rect(0, 0, size, size))
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				value := 20 + (x*130+y*65)/size
				if (x-size/2)*(x-size/2)+(y-size/2)*(y-size/2) < size*size/36 {
					value += 35
				}
				gray.Pix[y*gray.Stride+x] = byte(value)
			}
		}
		source = gray
	} else {
		rgb := image.NewRGBA(image.Rect(0, 0, size, size))
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				rgb.SetRGBA(x, y, color.RGBA{R: byte(20 + x*190/size), G: byte(30 + y*170/size), B: byte(50 + (x+y)*85/size), A: 255})
			}
		}
		source = rgb
	}
	var stream bytes.Buffer
	if err := stdjpeg.Encode(&stream, source, &stdjpeg.Options{Quality: 90}); err != nil {
		b.Fatal(err)
	}
	return stream.Bytes()
}

var baselineDecodeBenchSink pixeldata.Frames

func BenchmarkBaselineDecode1309(b *testing.B) {
	for _, cell := range []struct {
		size    int
		samples uint16
	}{{64, 1}, {512, 1}, {1024, 1}, {512, 3}} {
		b.Run(fmt.Sprintf("%dx%d/samples%d", cell.size, cell.size, cell.samples), func(b *testing.B) {
			stream := baselineDecodeBenchStream(b, cell.size, cell.samples)
			photometric := "MONOCHROME2"
			if cell.samples == 3 {
				photometric = "YBR_FULL_422"
			}
			obj := object.FromElements(append(jpegMetadataElements(uint16(cell.size), uint16(cell.size), cell.samples, photometric, 1), dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, stream)), nil)
			pixel, err := pixeldata.Extract(obj)
			if err != nil {
				b.Fatal(err)
			}
			codec := newBaselineCodec()
			frames, err := codec.Decode(pixel, obj)
			if err != nil || len(frames.Data) != 1 {
				b.Fatalf("fixture decode: %v", err)
			}
			b.Logf("encoded_sha256=%x output_sha256=%x encoded_bytes=%d", sha256.Sum256(stream), sha256.Sum256(frames.Data[0]), len(stream))
			b.SetBytes(int64(len(frames.Data[0])))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := codec.Decode(pixel, obj)
				if err != nil {
					b.Fatal(err)
				}
				baselineDecodeBenchSink = result
			}
		})
	}
}
