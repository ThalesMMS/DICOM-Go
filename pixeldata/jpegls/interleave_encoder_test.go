package jpegls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// interleaveEncoderFixture is one CharLS 2.4.2 RGB stream of the codecfull
// corpus with the samples it was encoded from.
type interleaveEncoderFixture struct {
	id     string
	stream []byte
	source []byte
	meta   pixeldata.Metadata
	near   int
	ilv    InterleaveMode
}

// charLSInterleaveEncoderFixtures returns the ILV=1 and ILV=2 RGB streams of
// the lossless (jpegls) and near-lossless (jpegls-near) CharLS corpora. The
// lossless source is the full reconstruction; the near-lossless one is the
// recorded source the stream was encoded from.
func charLSInterleaveEncoderFixtures(t *testing.T) []interleaveEncoderFixture {
	t.Helper()
	root := filepath.Join("..", "codecfixture", "testdata", "codecfull")
	read := func(path, hash string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if hash != "" && fmt.Sprintf("%x", sha256.Sum256(data)) != hash {
			t.Fatalf("%s: fixture hash mismatch", path)
		}
		return data
	}
	var fixtures []interleaveEncoderFixture
	for _, corpus := range []string{"jpegls", "jpegls-near"} {
		var record struct {
			Fixtures []struct {
				ID, Path, SHA256, ReferencePath, ReferenceSHA256 string
				Input                                            struct {
					Rows, Columns, Components, BitsAllocated, BitsStored uint16
				}
				SourceSamples struct {
					Path, SHA256 string
					Near         int
				}
			}
		}
		if err := json.Unmarshal(read(filepath.Join(corpus, "generation.json"), ""), &record); err != nil {
			t.Fatal(err)
		}
		for _, f := range record.Fixtures {
			if f.Input.Components != 3 {
				continue
			}
			var ilv InterleaveMode
			switch {
			case strings.HasSuffix(f.ID, "-ilv1"):
				ilv = InterleaveLine
			case strings.HasSuffix(f.ID, "-ilv2"):
				ilv = InterleaveSample
			default:
				continue
			}
			meta := encoderMetadata(f.Input.Rows, f.Input.Columns, 3, f.Input.BitsAllocated, "RGB")
			meta.BitsStored, meta.HighBit = f.Input.BitsStored, f.Input.BitsStored-1
			fixture := interleaveEncoderFixture{id: f.ID, stream: read(f.Path, f.SHA256), meta: meta, ilv: ilv}
			if f.SourceSamples.Path != "" {
				fixture.source, fixture.near = read(f.SourceSamples.Path, f.SourceSamples.SHA256), f.SourceSamples.Near
			} else {
				fixture.source = read(f.ReferencePath, f.ReferenceSHA256)
			}
			fixtures = append(fixtures, fixture)
		}
	}
	return fixtures
}

// TestInterleavedEncoderReproducesCharLSEntropy encodes the samples of every
// ILV=1 and ILV=2 RGB stream CharLS 2.4.2 wrote for the codecfull corpora
// (2-16 stored bits, NEAR 0 to 255, 1x9, 9x1 and 129x33 frames, and preset
// coding parameters) and requires the same scan header and entropy-coded
// bytes, then decodes the result with the pure-Go decoder: lossless frames
// round-trip bit-exactly and near-lossless ones equal CharLS's
// reconstruction.
func TestInterleavedEncoderReproducesCharLSEntropy(t *testing.T) {
	fixtures := charLSInterleaveEncoderFixtures(t)
	var lines, samples, presets int
	for _, f := range fixtures {
		t.Run(f.id, func(t *testing.T) {
			oracleHeader, oracleScans, err := splitScans(f.stream)
			if err != nil {
				t.Fatal(err)
			}
			if len(oracleScans) != 1 || oracleHeader.ilv != int(f.ilv) || oracleHeader.near != f.near {
				t.Fatalf("oracle scans=%d ILV=%d NEAR=%d", len(oracleScans), oracleHeader.ilv, oracleHeader.near)
			}
			params, err := validateEncoderInput(f.meta)
			if err != nil {
				t.Fatal(err)
			}
			codes, err := nativeToSamples(f.source, f.meta, params)
			if err != nil {
				t.Fatal(err)
			}
			var encoded []byte
			if strings.Contains(f.id, "preset") {
				// Reproduce the LSE coding parameters CharLS signalled.
				presets++
				encoded, err = encodeFrameWithPresetInterleave(context.Background(), codes, f.meta, params, oracleHeader.preset, f.ilv)
			} else {
				var encoder *Encoder
				encoder, err = NewEncoderWithOptions(EncoderOptions{Near: f.near, AllowLossy: f.near > 0, Interleave: f.ilv})
				if err != nil {
					t.Fatal(err)
				}
				var frame pixeldata.EncodedFrame
				frame, err = encoder.EncodeFrame(context.Background(), f.source, f.meta)
				encoded = frame.Data
			}
			if err != nil {
				t.Fatal(err)
			}
			header, scans, err := splitScans(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if len(scans) != 1 || header.ilv != int(f.ilv) || header.near != f.near || !bytes.Equal(scans[0].componentIDs, oracleScans[0].componentIDs) {
				t.Fatalf("scan header ILV=%d NEAR=%d components=%v", header.ilv, header.near, scans[0].componentIDs)
			}
			if !bytes.Equal(scans[0].entropy, oracleScans[0].entropy) {
				t.Fatalf("entropy differs from CharLS at byte %d of %d", firstDifferentByte(scans[0].entropy, oracleScans[0].entropy), len(oracleScans[0].entropy))
			}
			if strings.Contains(f.id, "preset") {
				return // the stream carries no LSE; its decoder would use defaults
			}
			got, err := decodeFrameModeContext(context.Background(), encoded, f.meta, f.near > 0)
			if err != nil {
				t.Fatal(err)
			}
			want, err := decodeFrameModeContext(context.Background(), f.stream, f.meta, f.near > 0)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) || (f.near == 0 && !bytes.Equal(got, f.source)) {
				t.Fatalf("decoded frame differs at byte %d", firstDifferentByte(got, want))
			}
		})
		if f.ilv == InterleaveLine {
			lines++
		} else {
			samples++
		}
	}
	if lines != 48 || samples != 48 || presets != 8 {
		t.Fatalf("qualified %d line and %d sample interleaved streams (%d preset), want 48, 48 and 8", lines, samples, presets)
	}
}

// interleaveSource is a deterministic RGB frame with flat runs (whole and
// part lines, equal and unequal components), edges and noise.
func interleaveSource(rows, columns, bits, allocated int, seed uint32) []byte {
	top := uint32(1)<<bits - 1
	bytesPerSample := allocated / 8
	out := make([]byte, rows*columns*3*bytesPerSample)
	state := seed
	i := 0
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			for c := 0; c < 3; c++ {
				state = state*1664525 + 1013904223
				var v uint32
				switch {
				case y%5 == 1:
					v = top / 2 // whole flat line, equal components
				case x > columns/2 && y%3 == 0:
					v = top / uint32(c+2) // flat tail, unequal components
				case y%7 == 3:
					v = (state >> 8) & top // noise
				default:
					v = (uint32(x*13+y*7+c*40) + (state>>28)&3) & top
				}
				if bytesPerSample == 2 {
					binary.LittleEndian.PutUint16(out[i:], uint16(v))
				} else {
					out[i] = byte(v)
				}
				i += bytesPerSample
			}
		}
	}
	return out
}

// TestInterleavedEncoderRoundTrips encodes synthetic RGB frames with ILV=1
// and ILV=2 over stored bits, frame shapes and NEAR, and decodes them with
// the pure-Go decoder: lossless frames are bit-exact, near-lossless frames
// are within NEAR of the source, and every stream is deterministic.
func TestInterleavedEncoderRoundTrips(t *testing.T) {
	shapes := [][2]int{{1, 1}, {1, 17}, {17, 1}, {9, 40}, {33, 129}}
	for _, ilv := range []InterleaveMode{InterleaveLine, InterleaveSample} {
		for _, bits := range []int{2, 5, 8, 10, 12, 16} {
			allocated := 8
			if bits > 8 {
				allocated = 16
			}
			for _, near := range []int{0, 1, 3} {
				if near > ((1<<bits)-1)/2 {
					continue
				}
				for _, shape := range shapes {
					name := fmt.Sprintf("ilv%d/bits%d/near%d/%dx%d", ilv, bits, near, shape[1], shape[0])
					t.Run(name, func(t *testing.T) {
						meta := encoderMetadata(uint16(shape[0]), uint16(shape[1]), 3, uint16(allocated), "RGB")
						meta.BitsStored, meta.HighBit = uint16(bits), uint16(bits-1)
						source := interleaveSource(shape[0], shape[1], bits, allocated, uint32(bits*31+shape[1]))
						encoder, err := NewEncoderWithOptions(EncoderOptions{Near: near, AllowLossy: near > 0, Interleave: ilv})
						if err != nil {
							t.Fatal(err)
						}
						first, err := encoder.EncodeFrame(context.Background(), source, meta)
						if err != nil {
							t.Fatal(err)
						}
						again, err := encoder.EncodeFrame(context.Background(), source, meta)
						if err != nil || !bytes.Equal(first.Data, again.Data) {
							t.Fatalf("non-deterministic stream: %v", err)
						}
						got, err := decodeFrameModeContext(context.Background(), first.Data, meta, near > 0)
						if err != nil {
							t.Fatal(err)
						}
						if near == 0 {
							if !bytes.Equal(got, source) {
								t.Fatalf("lossless round trip differs at byte %d", firstDifferentByte(got, source))
							}
							return
						}
						step := allocated / 8
						for i := 0; i < len(source); i += step {
							a, b := int(source[i]), int(got[i])
							if step == 2 {
								a, b = int(binary.LittleEndian.Uint16(source[i:])), int(binary.LittleEndian.Uint16(got[i:]))
							}
							if absInt(a-b) > near {
								t.Fatalf("sample %d is %d, beyond NEAR=%d of %d", i/step, b, near, a)
							}
						}
					})
				}
			}
		}
	}
}

// TestInterleavedEncoderOptions checks the scan headers, that monochrome
// frames keep one ILV=0 scan, and that an unknown mode is refused.
func TestInterleavedEncoderOptions(t *testing.T) {
	if _, err := NewEncoderWithOptions(EncoderOptions{Interleave: 3}); !errors.As(err, new(*pixeldata.UnsupportedEncoderMetadataError)) {
		t.Fatalf("ILV=3 error = %v", err)
	}
	if _, err := NewEncoderWithOptions(EncoderOptions{Interleave: -1}); err == nil {
		t.Fatal("ILV=-1 accepted")
	}
	rgb := encoderMetadata(4, 6, 3, 8, "RGB")
	rgbFrame := interleaveSource(4, 6, 8, 8, 7)
	mono := encoderMetadata(4, 6, 1, 8, "MONOCHROME2")
	monoFrame := rgbFrame[:24]
	for _, tc := range []struct {
		ilv       InterleaveMode
		near      int
		meta      pixeldata.Metadata
		frame     []byte
		wantScans int
		wantILV   int
	}{
		{InterleaveNone, 0, rgb, rgbFrame, 3, 0},
		{InterleaveLine, 0, rgb, rgbFrame, 1, 1},
		{InterleaveSample, 0, rgb, rgbFrame, 1, 2},
		{InterleaveSample, 2, rgb, rgbFrame, 1, 2},
		{InterleaveLine, 0, mono, monoFrame, 1, 0},
		{InterleaveSample, 1, mono, monoFrame, 1, 0},
	} {
		encoder, err := NewEncoderWithOptions(EncoderOptions{Near: tc.near, AllowLossy: tc.near > 0, Interleave: tc.ilv})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := encoder.EncodeFrame(context.Background(), tc.frame, tc.meta)
		if err != nil {
			t.Fatal(err)
		}
		header, scans, err := splitScans(encoded.Data)
		if err != nil {
			t.Fatal(err)
		}
		if len(scans) != tc.wantScans || header.ilv != tc.wantILV || header.near != tc.near {
			t.Fatalf("ILV option %d on %d samples: scans=%d ILV=%d NEAR=%d", tc.ilv, tc.meta.SamplesPerPixel, len(scans), header.ilv, header.near)
		}
		if tc.wantScans == 1 && tc.meta.SamplesPerPixel == 3 && !bytes.Equal(scans[0].componentIDs, []byte{1, 2, 3}) {
			t.Fatalf("interleaved scan components = %v", scans[0].componentIDs)
		}
	}
}

func BenchmarkEncoderRGB8Interleave(b *testing.B) {
	meta := encoderMetadata(512, 512, 3, 8, "RGB")
	frame := interleaveSource(512, 512, 8, 8, 1)
	for _, ilv := range []InterleaveMode{InterleaveNone, InterleaveLine, InterleaveSample} {
		b.Run(fmt.Sprintf("ilv%d", ilv), func(b *testing.B) {
			encoder, err := NewEncoderWithOptions(EncoderOptions{Interleave: ilv})
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(frame)))
			b.ReportAllocs()
			var size int
			for i := 0; i < b.N; i++ {
				encoded, err := encoder.EncodeFrame(context.Background(), frame, meta)
				if err != nil {
					b.Fatal(err)
				}
				size = len(encoded.Data)
			}
			b.ReportMetric(float64(size), "bytes/frame")
		})
	}
}
