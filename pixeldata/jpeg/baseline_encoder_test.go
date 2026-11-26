package jpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"math"
	"math/rand"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func baselineMetadata(rows, columns, samples uint16) pixeldata.Metadata {
	m := pixeldata.Metadata{Rows: rows, Columns: columns, SamplesPerPixel: samples, PhotometricInterpretation: "MONOCHROME2", NumberOfFrames: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7}
	if samples == 3 {
		m.PhotometricInterpretation, m.PlanarConfigurationPresent = "RGB", true
	}
	return m
}

func baselinePhantom(width, height, samples int, pattern string) []byte {
	frame := make([]byte, width*height*samples)
	rng := rand.New(rand.NewSource(1280))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			for c := 0; c < samples; c++ {
				value := 30 + (x*150/max(width-1, 1)+y*60/max(height-1, 1)+c*40)%210
				switch pattern {
				case "constant":
					value = 65 + c*60
				case "noise":
					value = rng.Intn(256)
				case "checker":
					value = ((x/8 + y/8 + c) % 2) * 255
				}
				frame[(y*width+x)*samples+c] = byte(value)
			}
		}
	}
	return frame
}

// Read the SOF independently of the encoder's block layout; never infer
// sampling from the Photometric Interpretation of encapsulated JPEG.
func baselineSOF(t testing.TB, stream []byte) []byte {
	t.Helper()
	if len(stream) < 4 || !bytes.Equal(stream[:2], []byte{0xff, 0xd8}) {
		t.Fatal("missing SOI")
	}
	for offset := 2; offset+4 <= len(stream); {
		if stream[offset] != 0xff {
			t.Fatal("invalid JPEG marker")
		}
		marker := stream[offset+1]
		length := int(stream[offset+2])<<8 | int(stream[offset+3])
		if length < 2 || offset+2+length > len(stream) {
			t.Fatal("invalid JPEG segment length")
		}
		if marker == 0xc0 {
			return stream[offset+4 : offset+2+length]
		}
		if marker == 0xda {
			break
		}
		offset += 2 + length
	}
	t.Fatal("missing Baseline SOF0")
	return nil
}

func baselineImageSamples(t testing.TB, stream []byte, samples int) []byte {
	t.Helper()
	img, err := stdjpeg.Decode(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	r := img.Bounds()
	out := make([]byte, r.Dx()*r.Dy()*samples)
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			if samples == 1 {
				out[y*r.Dx()+x] = color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y
			} else {
				c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				i := (y*r.Dx() + x) * 3
				out[i], out[i+1], out[i+2] = c.R, c.G, c.B
			}
		}
	}
	return out
}

func TestBaselineEncoderSamplingSOFAndDICOMMetadata(t *testing.T) {
	for _, sampling := range []Subsampling{Subsampling420, Subsampling422, Subsampling444} {
		for _, samples := range []uint16{1, 3} {
			for _, size := range [][2]uint16{{1, 1}, {19, 17}, {32, 32}} {
				for _, quality := range []int{1, 25, 90, 100} {
					t.Run(fmt.Sprintf("sampling%d/samples%d/%dx%d/q%d", sampling, samples, size[1], size[0], quality), func(t *testing.T) {
						encoder, err := NewEncoderWithOptions(EncoderOptions{Quality: quality, Subsampling: sampling, OptimizeHuffman: true})
						if err != nil {
							t.Fatal(err)
						}
						metadata := baselineMetadata(size[0], size[1], samples)
						frame := baselinePhantom(int(size[1]), int(size[0]), int(samples), "constant")
						encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
						if err != nil {
							t.Fatal(err)
						}
						sof := baselineSOF(t, encoded.Data)
						wantSampling := byte(0x11)
						if samples == 3 {
							wantSampling = []byte{0x22, 0x21, 0x11}[sampling]
							// PS3.5 Table 8.2.1-1 forbids YBR_FULL for lossy JPEG.
							if encoded.PhotometricInterpretation != "YBR_FULL_422" || encoded.PlanarConfiguration == nil || *encoded.PlanarConfiguration != 0 {
								t.Fatalf("invalid DICOM color attributes: %#v", encoded)
							}
						} else if encoded.PhotometricInterpretation != "" || encoded.PlanarConfiguration != nil {
							t.Fatal("monochrome attributes must remain unchanged")
						}
						if len(sof) != 6+3*int(samples) || sof[0] != 8 || sof[5] != byte(samples) || sof[7] != wantSampling || int(sof[1])<<8|int(sof[2]) != int(size[0]) || int(sof[3])<<8|int(sof[4]) != int(size[1]) {
							t.Fatalf("SOF0 = %x", sof)
						}
						if samples == 3 && (sof[10] != 0x11 || sof[13] != 0x11) {
							t.Fatalf("chroma sampling = %x", sof)
						}
						decoded := baselineImageSamples(t, encoded.Data, int(samples))
						// Constant colors detect DC/component-order errors even at q1.
						if worst := maxAbsoluteError(decoded, frame); worst > 20 {
							t.Fatalf("constant color error = %d", worst)
						}
						if quality == 100 && maxAbsoluteError(decoded, frame) > 2 {
							t.Fatal("q100 constant color mismatch")
						}
						img, err := stdjpeg.Decode(bytes.NewReader(encoded.Data))
						if err != nil {
							t.Fatal(err)
						}
						if samples == 3 && img.(*image.YCbCr).SubsampleRatio != []image.YCbCrSubsampleRatio{image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio422, image.YCbCrSubsampleRatio444}[sampling] {
							t.Fatal("decoder sampling does not match the requested mode")
						}
					})
				}
			}
		}
	}
}

func TestBaselineEncoderOptionsAndInputContract(t *testing.T) {
	for _, invalid := range []EncoderOptions{{Quality: 0}, {Quality: 101}, {Quality: 90, Subsampling: 3}, {Quality: 90, Subsampling: 255}} {
		if _, err := NewEncoderWithOptions(invalid); err == nil {
			t.Fatalf("accepted options %#v", invalid)
		}
	}
	if err := RegisterEncoderWithOptions(nil, EncoderOptions{Quality: 90}); !errors.Is(err, pixeldata.ErrEncoderRegistryNil) {
		t.Fatal(err)
	}
	legacy, _ := NewEncoder(90)
	if legacy.Subsampling() != Subsampling420 {
		t.Fatal("legacy constructor sampling changed")
	}
	if legacy.OptimizesHuffman() || legacy.Capabilities().Backend != "go-image-jpeg" {
		t.Fatal("legacy constructor selected a different backend")
	}
	for _, sampling := range []Subsampling{Subsampling420, Subsampling422, Subsampling444} {
		for _, optimize := range []bool{false, true} {
			encoder, err := NewEncoderWithOptions(EncoderOptions{Quality: 90, Subsampling: sampling, OptimizeHuffman: optimize})
			if err != nil {
				t.Fatal(err)
			}
			want := optimize || sampling != Subsampling420
			if encoder.OptimizesHuffman() != want || (encoder.Capabilities().Backend == "pure-go") != want {
				t.Fatal("options/backend contract mismatch")
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := legacy.EncodeFrame(ctx, []byte{1}, baselineMetadata(1, 1, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := legacy.EncodeFrame(context.Background(), []byte{1}, baselineMetadata(2, 2, 1)); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatal(err)
	}
}

func TestBaselineEncoderLegacyBytesUnchanged(t *testing.T) {
	for _, samples := range []uint16{1, 3} {
		metadata := baselineMetadata(17, 19, samples)
		frame := baselinePhantom(19, 17, int(samples), "noise")
		original := append([]byte(nil), frame...)
		for _, quality := range []int{1, 90, 100} {
			img, _, _ := jpegImage(frame, metadata)
			var legacy bytes.Buffer
			if err := stdjpeg.Encode(&legacy, img, &stdjpeg.Options{Quality: quality}); err != nil {
				t.Fatal(err)
			}
			for _, newEncoder := range []func() (*Encoder, error){
				func() (*Encoder, error) { return NewEncoder(quality) },
				func() (*Encoder, error) { return NewEncoderWithOptions(EncoderOptions{Quality: quality}) },
			} {
				encoder, err := newEncoder()
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
				if err != nil || !bytes.Equal(encoded.Data, legacy.Bytes()) {
					t.Fatalf("legacy bytes changed: %v", err)
				}
			}
		}
		if !bytes.Equal(frame, original) {
			t.Fatal("encoder changed source pixels")
		}
	}
}

type baselineCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *baselineCancelContext) Err() error {
	c.checks--
	if c.checks == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestBaselineEncoderCancellationDuringWideFrame(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The fourth check must occur inside this one MCU row, rather than after
	// converting all 65535 columns. A canceled frame never returns partial data.
	ctx := &baselineCancelContext{Context: parent, cancel: cancel, checks: 4}
	encoder, _ := NewEncoderWithOptions(EncoderOptions{Quality: 90, OptimizeHuffman: true})
	frame := baselinePhantom(65535, 1, 1, "smooth")
	encoded, err := encoder.EncodeFrame(ctx, frame, baselineMetadata(1, 65535, 1))
	if !errors.Is(err, context.Canceled) || len(encoded.Data) != 0 || ctx.checks != 0 {
		t.Fatalf("cancellation: err=%v data=%d checks=%d", err, len(encoded.Data), ctx.checks)
	}
}

func TestBaselineEncoderConcurrentOptimizedFrames(t *testing.T) {
	metadata := baselineMetadata(37, 53, 3)
	frame := baselinePhantom(53, 37, 3, "noise")
	original := append([]byte(nil), frame...)
	for _, sampling := range []Subsampling{Subsampling420, Subsampling422, Subsampling444} {
		encoder, _ := NewEncoderWithOptions(EncoderOptions{Quality: 95, Subsampling: sampling, OptimizeHuffman: true})
		want, err := encoder.EncodeFrame(context.Background(), frame, metadata)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := encoder.EncodeFrame(context.Background(), frame, metadata)
				if err != nil || !bytes.Equal(got.Data, want.Data) {
					t.Errorf("concurrent frame mismatch: %v", err)
				}
			}()
		}
		wg.Wait()
	}
	if !bytes.Equal(frame, original) {
		t.Fatal("optimized encoder changed the shared source")
	}
}

func TestBaselineEncoderOptimizedSizeAndFidelityAgainstLegacy(t *testing.T) {
	// Sampling and quantization quality are equal to the old Go encoder.
	// 422/444 preserve more chroma and are deliberately excluded from this
	// size assertion; quality alone does not specify equivalent fidelity.
	for _, samples := range []uint16{1, 3} {
		for _, pattern := range []string{"constant", "smooth", "checker", "noise"} {
			for _, quality := range []int{1, 25, 75, 95, 100} {
				t.Run(fmt.Sprintf("samples%d/%s/q%d", samples, pattern, quality), func(t *testing.T) {
					const width, height = 127, 113
					metadata := baselineMetadata(height, width, samples)
					frame := baselinePhantom(width, height, int(samples), pattern)
					encoder, _ := NewEncoderWithOptions(EncoderOptions{Quality: quality, OptimizeHuffman: true})
					encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
					if err != nil {
						t.Fatal(err)
					}
					img, _, _ := jpegImage(frame, metadata)
					var legacy bytes.Buffer
					if err := stdjpeg.Encode(&legacy, img, &stdjpeg.Options{Quality: quality}); err != nil {
						t.Fatal(err)
					}
					got, old := baselineImageSamples(t, encoded.Data, int(samples)), baselineImageSamples(t, legacy.Bytes(), int(samples))
					gotMAE, oldMAE := meanAbsoluteError(got, frame), meanAbsoluteError(old, frame)
					if len(encoded.Data) > legacy.Len() || gotMAE > oldMAE*1.03+0.1 {
						t.Fatalf("new/old bytes=%d/%d, MAE=%g/%g", len(encoded.Data), legacy.Len(), gotMAE, oldMAE)
					}
					t.Logf("new/old bytes=%d/%d, MAE=%.4f/%.4f", len(encoded.Data), legacy.Len(), gotMAE, oldMAE)
				})
			}
		}
	}
}

func TestBaselineForwardDCTAgainstDefinition(t *testing.T) {
	rng := rand.New(rand.NewSource(1280))
	for trial := 0; trial < 8; trial++ {
		var input [64]float64
		for i := range input {
			input[i] = float64(rng.Intn(256) - 128)
		}
		got := baselineForwardDCT(&input)
		for v := 0; v < 8; v++ {
			for u := 0; u < 8; u++ {
				sum := 0.0
				for y := 0; y < 8; y++ {
					for x := 0; x < 8; x++ {
						sum += input[y*8+x] * math.Cos(float64((2*x+1)*u)*math.Pi/16) * math.Cos(float64((2*y+1)*v)*math.Pi/16)
					}
				}
				sum *= 0.25
				if u == 0 {
					sum /= math.Sqrt2
				}
				if v == 0 {
					sum /= math.Sqrt2
				}
				if math.Abs(got[v*8+u]-sum) > 1e-10 {
					t.Fatalf("FDCT(%d,%d) = %.12f, definition %.12f", u, v, got[v*8+u], sum)
				}
			}
		}
	}
}

func BenchmarkBaselineEncoder(b *testing.B) {
	for _, samples := range []uint16{1, 3} {
		metadata := baselineMetadata(512, 512, samples)
		frame := baselinePhantom(512, 512, int(samples), "smooth")
		for _, sampling := range []Subsampling{Subsampling420, Subsampling422, Subsampling444} {
			if samples == 1 && sampling != Subsampling420 {
				continue
			}
			b.Run(fmt.Sprintf("samples%d/sampling%d", samples, sampling), func(b *testing.B) {
				encoder, _ := NewEncoderWithOptions(EncoderOptions{Quality: 90, Subsampling: sampling, OptimizeHuffman: true})
				b.SetBytes(int64(len(frame)))
				b.ReportAllocs()
				h, v := 1, 1
				if samples == 3 && sampling != Subsampling444 {
					h = 2
					if sampling == Subsampling420 {
						v = 2
					}
				}
				blocks := h * v
				if samples == 3 {
					blocks += 2
				}
				b.ReportMetric(float64(((512+8*h-1)/(8*h))*((512+8*v-1)/(8*v))*blocks*128), "coefficient-B/frame")
				encodedBytes := 0
				for i := 0; i < b.N; i++ {
					encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
					if err != nil {
						b.Fatal(err)
					}
					encodedBytes = len(encoded.Data)
				}
				b.ReportMetric(float64(encodedBytes), "JPEG-B/frame")
			})
		}
		b.Run(fmt.Sprintf("samples%d/default", samples), func(b *testing.B) {
			encoder, _ := NewEncoder(90)
			b.SetBytes(int64(len(frame)))
			b.ReportAllocs()
			encodedBytes := 0
			for i := 0; i < b.N; i++ {
				encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
				if err != nil {
					b.Fatal(err)
				}
				encodedBytes = len(encoded.Data)
			}
			b.ReportMetric(float64(encodedBytes), "JPEG-B/frame")
		})
		b.Run(fmt.Sprintf("samples%d/legacy", samples), func(b *testing.B) {
			b.SetBytes(int64(len(frame)))
			b.ReportAllocs()
			encodedBytes := 0
			for i := 0; i < b.N; i++ {
				img, _, _ := jpegImage(frame, metadata)
				var buf bytes.Buffer
				if err := stdjpeg.Encode(&buf, img, &stdjpeg.Options{Quality: 90}); err != nil {
					b.Fatal(err)
				}
				encodedBytes = buf.Len()
			}
			b.ReportMetric(float64(encodedBytes), "JPEG-B/frame")
		})
	}
}
