package jpeg2000

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	j2kenc "github.com/ThalesMMS/dicom-go/pixeldata/jpeg2000"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The pure-Go encoder of the base module (pixeldata/jpeg2000) checked against
// this module's core decoder: Part 1 through RegisterCore, HTJ2K through
// RegisterCoreHTJ2K. OpenJPEG and OpenJPH check the same encoder in the base
// module's opt-in oracle tests.

func coreDecoders(t testing.TB) *pixeldata.MemoryRegistry {
	t.Helper()
	registry := pixeldata.NewMemoryRegistry()
	if err := RegisterCore(registry, CoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterCoreHTJ2K(registry, CoreOptions{}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func pureGoEncoders(t testing.TB, colorTransform bool) *pixeldata.MemoryEncoderRegistry {
	t.Helper()
	registry := pixeldata.NewMemoryEncoderRegistry()
	for _, uid := range j2kenc.LosslessTransferSyntaxUIDs() {
		encoder, err := j2kenc.NewEncoder(j2kenc.Options{TransferSyntaxUID: uid, ColorTransform: colorTransform})
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.RegisterEncoder(uid, encoder); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

var pureGoLosslessTargets = []transfer.Syntax{transfer.JPEG2000LosslessOnly, transfer.HTJ2KLossless, transfer.HTJ2KLosslessRPCL}

// decodeNative decompresses dataset with the core decoders and returns its
// native frames and metadata.
func decodeNative(t *testing.T, dataset *object.Object, syntax transfer.Syntax) ([][]byte, pixeldata.Metadata) {
	t.Helper()
	native, _, err := pixeldata.DecompressDataSetContext(context.Background(), dataset, syntax, pixeldata.DecompressOptions{Registry: coreDecoders(t)})
	if err != nil {
		t.Fatalf("decode %s: %v", syntax.Name, err)
	}
	frames, err := pixeldata.ExtractNativeFrames(native)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := pixeldata.ExtractMetadata(native)
	if err != nil {
		t.Fatal(err)
	}
	return frames.Data, metadata
}

func TestCoreDecodesPureGoEncoderCorpusExactly(t *testing.T) {
	frames, samples := 0, 0
	for _, colorTransform := range []bool{false, true} {
		encoders := pureGoEncoders(t, colorTransform)
		for _, target := range pureGoLosslessTargets {
			for _, tc := range codecfixture.JPEG2000LosslessEncoderCases() {
				if colorTransform && tc.Metadata.SamplesPerPixel != 3 {
					continue
				}
				source := tc.Object()
				converted, report, err := pixeldata.TranscodeDataSet(context.Background(), source, tc.Syntax, target, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
				if err != nil || report.Lossy || report.Frames != 2 {
					t.Fatalf("%s %s: %+v %v", target.Name, tc.Name, report, err)
				}
				metadata, err := pixeldata.ExtractMetadata(converted)
				if err != nil {
					t.Fatal(err)
				}
				wantPhotometric := tc.Metadata.PhotometricInterpretation
				if colorTransform {
					wantPhotometric = "YBR_RCT"
				}
				if metadata.PhotometricInterpretation != wantPhotometric || metadata.BitsStored != tc.Metadata.BitsStored {
					t.Fatalf("%s %s metadata = %+v", target.Name, tc.Name, metadata)
				}
				decoded, _ := decodeNative(t, converted, target)
				for i, want := range tc.ExpectedFrames {
					if !bytes.Equal(decoded[i], want) {
						t.Fatalf("%s %s RCT=%t frame %d differs", target.Name, tc.Name, colorTransform, i)
					}
					frames++
					samples += int(tc.Metadata.Rows) * int(tc.Metadata.Columns) * int(tc.Metadata.SamplesPerPixel)
				}
			}
		}
	}
	// 21 cases x 2 frames x 3 syntaxes, plus the 11 colour cases again with
	// the component transform.
	if frames != 21*2*3+11*2*3 {
		t.Fatalf("checked %d frames", frames)
	}
	t.Logf("%d frames, %d samples bit-exact", frames, samples)
}

// TestPureGoEncoderRoundTripsExistingFixtures re-encodes the repository's
// independently encoded JPEG 2000 and HTJ2K fixtures, and the native twins
// of the JPEG 2000 ones, and requires the core decoder to return exactly the
// samples it decoded before.
func TestPureGoEncoderRoundTripsExistingFixtures(t *testing.T) {
	for _, name := range []string{
		"693_UNCR.dcm", "emri_small.dcm", "US1_UNCI.dcm", "MR_small.dcm",
		"693_J2KR.dcm", "emri_small_jpeg_2k_lossless.dcm",
		"HTJ2KLossless_08_RGB.dcm", "HTJ2K_08_RGB.dcm",
	} {
		file := coreFixture(t, name)
		want, wantMetadata := decodeNative(t, file.Dataset, file.TransferSyntax)
		for _, colorTransform := range []bool{false, true} {
			if colorTransform && wantMetadata.SamplesPerPixel != 3 {
				continue
			}
			encoders := pureGoEncoders(t, colorTransform)
			for _, target := range pureGoLosslessTargets {
				converted, report, err := pixeldata.TranscodeDataSet(context.Background(), file.Dataset, file.TransferSyntax, target, pixeldata.TranscodeOptions{
					EncoderRegistry: encoders, DecoderRegistry: coreDecoders(t), ForceReencode: true,
				})
				if err != nil || report.Frames != wantMetadata.NumberOfFrames {
					t.Fatalf("%s to %s: %+v %v", name, target.Name, report, err)
				}
				got, metadata := decodeNative(t, converted, target)
				if metadata.Rows != wantMetadata.Rows || metadata.SamplesPerPixel != wantMetadata.SamplesPerPixel || len(got) != len(want) {
					t.Fatalf("%s to %s metadata = %+v", name, target.Name, metadata)
				}
				for i := range want {
					if !bytes.Equal(got[i], want[i]) {
						t.Fatalf("%s to %s RCT=%t frame %d differs", name, target.Name, colorTransform, i)
					}
				}
			}
		}
	}
}

// The pydicom HTJ2K fixtures were encoded by OpenJPH; these are the
// reference points codecfull checks with OpenJPH itself.
func TestCoreDecodesIndependentHTJ2KFixtures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tolerance int
		points    map[[2]int][]int // (row, column) -> interleaved samples from there
	}{
		{"HTJ2KLossless_08_RGB.dcm", 0, map[[2]int][]int{{160, 295}: {90, 38, 1, 94, 40, 1, 97, 42, 5}, {275, 635}: {208, 193, 172}}},
		{"HTJ2K_08_RGB.dcm", 1, map[[2]int][]int{{160, 295}: {91, 37, 2, 94, 40, 1, 97, 42, 5}, {275, 635}: {207, 193, 171}}},
	} {
		file := coreFixture(t, tc.name)
		frames, metadata := decodeNative(t, file.Dataset, file.TransferSyntax)
		for at, want := range tc.points {
			start := (at[0]*int(metadata.Columns) + at[1]) * int(metadata.SamplesPerPixel)
			for i, w := range want {
				if d := int(frames[0][start+i]) - w; d > tc.tolerance || d < -tc.tolerance {
					t.Fatalf("%s at %v+%d = %d, want %d", tc.name, at, i, frames[0][start+i], w)
				}
			}
		}
	}
}

func TestCoreDecodesPureGoLossyEncoder(t *testing.T) {
	tc := codecfixture.JPEG2000LosslessEncoderCases()[8] // 12 bits, one component
	for _, target := range []transfer.Syntax{transfer.JPEG2000, transfer.HTJ2K} {
		encoders := pixeldata.NewMemoryEncoderRegistry()
		if err := j2kenc.RegisterLossyEncoders(encoders, j2kenc.LossyOptions{StepSize: 2, AllowLossy: true}); err != nil {
			t.Fatal(err)
		}
		source := tc.Object()
		if _, _, err := pixeldata.TranscodeDataSet(context.Background(), source, tc.Syntax, target, pixeldata.TranscodeOptions{EncoderRegistry: encoders}); !errors.Is(err, pixeldata.ErrTranscodeLossyDisallowed) {
			t.Fatalf("%s without AllowLossy: %v", target.Name, err)
		}
		converted, report, err := pixeldata.TranscodeDataSet(context.Background(), source, tc.Syntax, target, pixeldata.TranscodeOptions{EncoderRegistry: encoders, AllowLossy: true})
		if err != nil || !report.Lossy {
			t.Fatalf("%s: %+v %v", target.Name, report, err)
		}
		method := j2kenc.MethodPart1
		if target.UID == transfer.HTJ2K.UID {
			method = j2kenc.MethodHTJ2K
		}
		lossy, _ := converted.GetString(core.NewTag(0x0028, 0x2110))
		methods, _ := converted.GetString(core.NewTag(0x0028, 0x2114))
		if strings.TrimSpace(lossy) != "01" || !strings.Contains(methods, method) {
			t.Fatalf("%s lossy attributes %q %q", target.Name, lossy, methods)
		}
		decoded, _ := decodeNative(t, converted, target)
		for i, want := range tc.ExpectedFrames {
			var worst int
			var sum float64
			for k := 0; k+1 < len(want); k += 2 {
				d := int(uint16(decoded[i][k])|uint16(decoded[i][k+1])<<8) - int(uint16(want[k])|uint16(want[k+1])<<8)
				sum += float64(d * d)
				worst = max(worst, d, -d)
			}
			psnr := 10 * math.Log10(4095*4095/(sum/float64(len(want)/2)))
			// The fixture alternates extremes, the hardest content there is; a
			// step of 2 still keeps every frame near-lossless.
			if psnr < 60 || worst > 16 {
				t.Fatalf("%s frame %d: PSNR %.1f dB, worst %d", target.Name, i, psnr, worst)
			}
		}
	}
}

func TestCoreRefusesUnsupportedHTCodeBlockStyles(t *testing.T) {
	tc := codecfixture.JPEG2000LosslessEncoderCases()[0]
	encoder, err := j2kenc.NewEncoder(j2kenc.Options{TransferSyntaxUID: transfer.HTJ2KLossless.UID})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encoder.EncodeFrame(context.Background(), tc.ExpectedFrames[0], tc.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	cod := bytes.Index(encoded.Data, []byte{0xFF, 0x52})
	decoder := NewCoreDecoder(CoreOptions{}).(ContextDecoder)
	for _, style := range []byte{0xC0, 0x41} {
		mutated := append([]byte(nil), encoded.Data...)
		mutated[cod+12] = style // SPcod code-block style
		if _, err := decoder.DecodeFrameContext(context.Background(), mutated, tc.Metadata); !errors.Is(err, ErrUnsupportedCodestreamFeature) {
			t.Fatalf("style %#x error = %v", style, err)
		}
	}
	if got, err := decoder.DecodeFrameContext(context.Background(), encoded.Data, tc.Metadata); err != nil || !bytes.Equal(got, tc.ExpectedFrames[0]) {
		t.Fatalf("unmodified stream: %v", err)
	}
}

// TestCoreDecodesOpenJPHEncodedHTJ2K is opt-in (DICOMGO_J2K_ORACLE=1): it
// encodes synthetic images with OpenJPH's ojph_compress and requires the core
// decoder to return them exactly.
func TestCoreDecodesOpenJPHEncodedHTJ2K(t *testing.T) {
	if os.Getenv("DICOMGO_J2K_ORACLE") != "1" {
		t.Skip("set DICOMGO_J2K_ORACLE=1 to encode with OpenJPH")
	}
	compress := os.Getenv("DICOM_GO_OPENJPH_COMPRESS")
	if compress == "" {
		compress = "ojph_compress"
	}
	path, err := exec.LookPath(compress)
	if err != nil {
		t.Fatalf("OpenJPH encoder unavailable: %v", err)
	}
	for _, tc := range codecfixture.JPEG2000LosslessEncoderCases() {
		if tc.Metadata.Rows < 2 || tc.Metadata.Columns < 2 {
			continue
		}
		for _, args := range [][]string{{"-reversible", "true"}, {"-reversible", "true", "-prog_order", "RPCL", "-num_decomps", "3"}} {
			dir := t.TempDir()
			input := filepath.Join(dir, "in.pgm")
			if tc.Metadata.SamplesPerPixel == 3 {
				input = filepath.Join(dir, "in.ppm")
			}
			if err := os.WriteFile(input, pnmOf(tc.ExpectedFrames[0], tc.Metadata), 0o600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "out.j2c")
			cmd := exec.Command(path, append([]string{"-i", input, "-o", output}, args...)...)
			if text, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("ojph_compress: %v\n%s", err, text)
			}
			codestream, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			metadata := tc.Metadata
			if metadata.SamplesPerPixel == 3 && !bytes.Contains(codestream[:64], []byte{0xFF, 0x52}) {
				t.Fatal("no COD")
			}
			if cod := bytes.Index(codestream, []byte{0xFF, 0x52}); metadata.SamplesPerPixel == 3 && codestream[cod+8] == 1 {
				metadata.PhotometricInterpretation = "YBR_RCT"
			}
			got, err := NewCoreDecoder(CoreOptions{}).(ContextDecoder).DecodeFrameContext(context.Background(), codestream, metadata)
			if err != nil {
				t.Fatalf("%s %v: %v", tc.Name, args, err)
			}
			if !bytes.Equal(got, tc.ExpectedFrames[0]) {
				t.Fatalf("%s %v: core decode differs from the source", tc.Name, args)
			}
		}
	}
}

// pnmOf writes a native frame as binary PGM/PPM (16-bit samples big-endian).
func pnmOf(frame []byte, m pixeldata.Metadata) []byte {
	magic := "P5"
	if m.SamplesPerPixel == 3 {
		magic = "P6"
	}
	maxValue := 1<<m.BitsStored - 1
	header := []byte(magic + "\n" + itoa(int(m.Columns)) + " " + itoa(int(m.Rows)) + "\n" + itoa(maxValue) + "\n")
	samples := int(m.Rows) * int(m.Columns) * int(m.SamplesPerPixel)
	out := append([]byte(nil), header...)
	for i := 0; i < samples; i++ {
		switch {
		case m.BitsAllocated == 8:
			out = append(out, frame[i])
		case m.BitsStored <= 8:
			out = append(out, frame[2*i])
		default:
			out = append(out, frame[2*i+1], frame[2*i])
		}
	}
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for ; v > 0; v /= 10 {
		b = append([]byte{byte('0' + v%10)}, b...)
	}
	return string(b)
}
