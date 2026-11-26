package jpeg2000

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The independent-decoder gate is opt-in: DICOMGO_J2K_ORACLE=1 runs every
// encoded codestream through OpenJPEG's opj_decompress and, for HTJ2K,
// OpenJPH's ojph_expand. DICOM_GO_OPENJPEG_DECOMPRESS and
// DICOM_GO_OPENJPH_EXPAND name the executables (default: PATH). When the gate
// is enabled a missing executable fails the test rather than skipping it.

type oracle struct {
	name, path string
	ht         bool // decodes only HTJ2K
}

func oracles(t *testing.T) []oracle {
	t.Helper()
	if os.Getenv("DICOMGO_J2K_ORACLE") != "1" {
		t.Skip("set DICOMGO_J2K_ORACLE=1 to decode with OpenJPEG and OpenJPH")
	}
	var out []oracle
	for _, o := range []struct{ name, env, fallback string }{
		{"OpenJPEG", "DICOM_GO_OPENJPEG_DECOMPRESS", "opj_decompress"},
		{"OpenJPH", "DICOM_GO_OPENJPH_EXPAND", "ojph_expand"},
	} {
		name := os.Getenv(o.env)
		if name == "" {
			name = o.fallback
		}
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("%s decoder %q unavailable: %v", o.name, name, err)
		}
		out = append(out, oracle{name: o.name, path: path, ht: o.name == "OpenJPH"})
	}
	return out
}

// decode runs one external decoder and returns the decoded planes.
func (o oracle) decode(t *testing.T, codestream []byte, m pixeldata.Metadata, signed bool) [][]int32 {
	t.Helper()
	dir := t.TempDir()
	input := filepath.Join(dir, "frame.j2c")
	if err := os.WriteFile(input, codestream, 0o600); err != nil {
		t.Fatal(err)
	}
	comps := int(m.SamplesPerPixel)
	var output string
	switch {
	case o.ht && comps == 3:
		output = filepath.Join(dir, "frame.ppm")
	case o.ht:
		output = filepath.Join(dir, "frame.raw")
	default:
		// PGX keeps the signedness of every precision; OpenJPEG's raw
		// writer clips signed samples narrower than their container.
		output = filepath.Join(dir, "frame.pgx")
	}
	cmd := exec.Command(o.path, "-i", input, "-o", output)
	if text, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", o.name, err, text)
	}
	var data []byte
	if filepath.Ext(output) == ".pgx" {
		for c := 0; c < comps; c++ {
			part, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("frame_%d.pgx", c)))
			if err != nil {
				t.Fatal(err)
			}
			end := bytes.IndexByte(part, '\n')
			if end < 0 {
				t.Fatal("malformed PGX header")
			}
			data = append(data, part[end+1:]...)
		}
	} else {
		var err error
		if data, err = os.ReadFile(output); err != nil {
			t.Fatal(err)
		}
	}
	pixels := int(m.Rows) * int(m.Columns)
	depth := uint(m.BitsStored)
	bytesPer := 1
	if depth > 8 {
		bytesPer = 2
	}
	// Raw output is planar and little-endian, PGX planar and big-endian,
	// PPM interleaved and big-endian after its header.
	interleaved, bigEndian := false, filepath.Ext(output) == ".pgx"
	if filepath.Ext(output) == ".ppm" {
		data = pnmRaster(t, data)
		interleaved, bigEndian = true, true
	}
	if len(data) != comps*pixels*bytesPer {
		t.Fatalf("%s wrote %d bytes, want %d", o.name, len(data), comps*pixels*bytesPer)
	}
	low, high := int32(0), int32(1)<<depth-1
	if signed {
		low, high = -(int32(1) << (depth - 1)), int32(1)<<(depth-1)-1
	}
	planes := make([][]int32, comps)
	for c := range planes {
		planes[c] = make([]int32, pixels)
		for i := range planes[c] {
			o := (c*pixels + i) * bytesPer
			if interleaved {
				o = (i*comps + c) * bytesPer
			}
			var v int32
			switch {
			case bytesPer == 1 && signed:
				v = int32(int8(data[o]))
			case bytesPer == 1:
				v = int32(data[o])
			case bigEndian && signed:
				v = int32(int16(uint16(data[o])<<8 | uint16(data[o+1])))
			case bigEndian:
				v = int32(uint16(data[o])<<8 | uint16(data[o+1]))
			case signed:
				v = int32(int16(uint16(data[o]) | uint16(data[o+1])<<8))
			default:
				v = int32(uint16(data[o]) | uint16(data[o+1])<<8)
			}
			// Decoders clip a lossy reconstruction differently; a viewer
			// clamps to the stored range.
			planes[c][i] = min(max(v, low), high)
		}
	}
	return planes
}

func TestIndependentDecodersReproduceLosslessCodestreams(t *testing.T) {
	decoders := oracles(t)
	uids := []string{transfer.JPEG2000LosslessOnly.UID, transfer.HTJ2KLossless.UID, transfer.HTJ2KLosslessRPCL.UID}
	samples := 0
	for _, uid := range uids {
		for _, transform := range []bool{false, true} {
			encoder, err := NewEncoder(Options{TransferSyntaxUID: uid, ColorTransform: transform})
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range losslessCases() {
				if transform && tc.metadata.SamplesPerPixel != 3 {
					continue
				}
				encoded, err := encoder.EncodeFrame(context.Background(), tc.frame, tc.metadata)
				if err != nil {
					t.Fatalf("%s %s: %v", uid, tc.name, err)
				}
				for _, o := range decoders {
					if o.ht && !encoder.coding.ht {
						continue
					}
					got := o.decode(t, encoded.Data, tc.metadata, tc.metadata.PixelRepresentation == 1)
					if c, i, ok := firstMismatch(tc.samples, got); !ok {
						t.Errorf("%s %s RCT=%t %s: component %d sample %d = %d, want %d", o.name, uid, transform, tc.name, c, i, got[c][i], tc.samples[c][i])
						continue
					}
					for c := range tc.samples {
						samples += len(tc.samples[c])
					}
				}
			}
		}
	}
	t.Logf("%d samples decoded bit-exactly", samples)
}

func TestIndependentDecodersAgreeOnLossyCodestreams(t *testing.T) {
	decoders := oracles(t)
	cases := []frameCase{
		smoothFrame(256, 192, 1, 12, false, 31),
		smoothFrame(160, 120, 3, 8, false, 32),
		smoothFrame(97, 65, 1, 16, true, 33),
		syntheticFrame(64, 48, 1, 10, 16, false, 34),
	}
	for _, uid := range []string{transfer.JPEG2000.UID, transfer.HTJ2K.UID} {
		for _, tc := range cases {
			for _, step := range []float64{0.5, 2, 8, 32} {
				for _, transform := range []bool{false, true} {
					if transform && tc.metadata.SamplesPerPixel != 3 {
						continue
					}
					encoder, err := NewEncoder(Options{TransferSyntaxUID: uid, StepSize: step, ColorTransform: transform, AllowLossy: true})
					if err != nil {
						t.Fatal(err)
					}
					encoded, err := encoder.EncodeFrame(context.Background(), tc.frame, tc.metadata)
					if err != nil {
						t.Fatal(err)
					}
					model := reconstructLossy(tc, encoder.coding)
					psnr := peakSNR(tc.samples, model, int(tc.metadata.BitsStored))
					ratio := float64(len(tc.frame)) / float64(len(encoded.Data))
					line := fmt.Sprintf("%s %s step %v ICT %t: %.1f:1, PSNR %.1f dB", uid, tc.name, step, transform, ratio, psnr)
					if psnr < minimumPSNR(step, int(tc.metadata.BitsStored)) {
						t.Errorf("%s: PSNR below %.1f dB", line, minimumPSNR(step, int(tc.metadata.BitsStored)))
					}
					for _, o := range decoders {
						if o.ht && !encoder.coding.ht {
							continue
						}
						got := o.decode(t, encoded.Data, tc.metadata, tc.metadata.PixelRepresentation == 1)
						d := maxDifference(got, model)
						line += fmt.Sprintf(", %s within %d", o.name, d)
						if d > lossyTolerance(int(tc.metadata.BitsStored)) {
							t.Errorf("%s: %s differs from the reconstruction model by %d", line, o.name, d)
						}
					}
					t.Log(line)
				}
			}
		}
	}
}

// lossyTolerance bounds how far an independent decoder may be from the
// float64 model: decoders compute the 9/7 synthesis in float32 and round
// their own way, which costs OpenJPEG up to two steps at 16 bits.
func lossyTolerance(bits int) int32 {
	if bits > 12 {
		return 2
	}
	return 1
}

// firstMismatch finds the first sample where got differs from want.
func firstMismatch(want, got [][]int32) (int, int, bool) {
	for c := range want {
		for i := range want[c] {
			if got[c][i] != want[c][i] {
				return c, i, false
			}
		}
	}
	return 0, 0, true
}

// pnmRaster returns the raster after a binary PNM header.
func pnmRaster(t *testing.T, data []byte) []byte {
	t.Helper()
	fields, i := 0, 0
	for fields < 4 && i < len(data) {
		for i < len(data) && (data[i] == ' ' || data[i] == '\n' || data[i] == '\r' || data[i] == '\t') {
			i++
		}
		for i < len(data) && !(data[i] == ' ' || data[i] == '\n' || data[i] == '\r' || data[i] == '\t') {
			i++
		}
		fields++
	}
	if fields != 4 || i >= len(data) {
		t.Fatal("malformed PNM header")
	}
	return data[i+1:]
}

// peakSNR is the PSNR of decoded against source over all components.
func peakSNR(source, decoded [][]int32, bits int) float64 {
	var sum float64
	n := 0
	for c := range source {
		for i := range source[c] {
			d := float64(source[c][i] - decoded[c][i])
			sum += d * d
			n++
		}
	}
	if sum == 0 {
		return math.Inf(1)
	}
	peak := float64(int(1)<<bits - 1)
	return 10 * math.Log10(peak*peak/(sum/float64(n)))
}

// minimumPSNR is a loose floor: an error of about 2*step RMS. The
// synthetic frames are noise-heavy, so this checks that quantization is
// bounded by the step rather than measuring quality.
func minimumPSNR(step float64, bits int) float64 {
	peak := float64(int(1)<<bits - 1)
	return 20 * math.Log10(peak/(2*step+1))
}

func ExampleRegisterEncoders() {
	registry := pixeldata.NewMemoryEncoderRegistry()
	if err := RegisterEncoders(registry); err != nil {
		panic(err)
	}
	fmt.Println(registry.RegisteredEncoderUIDs())
	// Output: [1.2.840.10008.1.2.4.201 1.2.840.10008.1.2.4.202 1.2.840.10008.1.2.4.90]
}
