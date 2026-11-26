package jpeg2000

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// OpenJPH-encoded HTJ2K fixtures (#1270): codestreams written by
// ojph_compress from the synthetic codecfixture encoder cases. Lossless
// fixtures must decode to the source frame; the irreversible one to the
// frame ojph_expand reconstructs, stored beside it as .raw. Regenerate with
// DICOMGO_REGEN_OPENJPH_FIXTURES=1 (needs ojph_compress and ojph_expand).
var openJPHFixtures = []struct {
	name   string
	caseID int
	syntax transfer.Syntax
	args   []string
}{
	{"u8-gray-reversible", 0, transfer.HTJ2KLossless, []string{"-reversible", "true"}},
	{"u8-rgb-reversible", 1, transfer.HTJ2KLossless, []string{"-reversible", "true"}},
	{"u12-gray-reversible", 8, transfer.HTJ2KLossless, []string{"-reversible", "true"}},
	{"u12-gray-rpcl", 8, transfer.HTJ2KLosslessRPCL, []string{"-reversible", "true", "-prog_order", "RPCL", "-num_decomps", "3"}},
	{"u16-rgb-reversible", 17, transfer.HTJ2KLossless, []string{"-reversible", "true"}},
	{"u16-rgb-large", 20, transfer.HTJ2KLossless, []string{"-reversible", "true", "-block_size", "{32,32}"}},
	{"u8-gray-irreversible", 0, transfer.HTJ2K, []string{"-reversible", "false", "-qstep", "0.02"}},
}

const openJPHFixtureDir = "testdata/openjph"

func TestRegisterDecodesOpenJPHHTJ2KFixtures(t *testing.T) {
	if os.Getenv("DICOMGO_REGEN_OPENJPH_FIXTURES") == "1" {
		regenerateOpenJPHFixtures(t)
	}
	cases := codecfixture.JPEG2000LosslessEncoderCases()
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range openJPHFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			tc := cases[fixture.caseID]
			codestream, err := os.ReadFile(filepath.Join(openJPHFixtureDir, fixture.name+".j2c"))
			if err != nil {
				t.Fatal(err)
			}
			want := tc.ExpectedFrames[0]
			if fixture.syntax.UID == transfer.HTJ2K.UID {
				if want, err = os.ReadFile(filepath.Join(openJPHFixtureDir, fixture.name+".raw")); err != nil {
					t.Fatal(err)
				}
			}
			m := tc.Metadata
			photometric := m.PhotometricInterpretation
			if cod := bytes.Index(codestream, []byte{0xFF, 0x52}); m.SamplesPerPixel == 3 && codestream[cod+8] == 1 {
				photometric = "YBR_RCT"
			}
			obj, pixel := jpeg2000Object(t, jpeg2000MetadataOptions{
				rows: m.Rows, columns: m.Columns, samplesPerPixel: m.SamplesPerPixel,
				bitsAllocated: m.BitsAllocated, bitsStored: m.BitsStored, photometric: photometric,
			}, codestream)
			frames, err := registry.DecodeFrames(fixture.syntax.UID, pixel, obj)
			if err != nil {
				t.Fatalf("DecodeFrames: %v", err)
			}
			if got := frames.Data[0]; !bytes.Equal(got, want) {
				differ := 0
				for i := range min(len(got), len(want)) {
					if got[i] != want[i] {
						differ++
					}
				}
				t.Fatalf("decoded %d bytes, want %d; %d bytes differ", len(got), len(want), differ)
			}
		})
	}
}

func regenerateOpenJPHFixtures(t *testing.T) {
	t.Helper()
	compress, err := exec.LookPath("ojph_compress")
	if err != nil {
		t.Fatalf("ojph_compress: %v", err)
	}
	expand, err := exec.LookPath("ojph_expand")
	if err != nil {
		t.Fatalf("ojph_expand: %v", err)
	}
	if err := os.MkdirAll(openJPHFixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := codecfixture.JPEG2000LosslessEncoderCases()
	for _, fixture := range openJPHFixtures {
		tc := cases[fixture.caseID]
		dir := t.TempDir()
		input := filepath.Join(dir, "in.pgm")
		if tc.Metadata.SamplesPerPixel == 3 {
			input = filepath.Join(dir, "in.ppm")
		}
		if err := os.WriteFile(input, pnmOf(tc.ExpectedFrames[0], tc.Metadata), 0o600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(openJPHFixtureDir, fixture.name+".j2c")
		if text, err := exec.Command(compress, append([]string{"-i", input, "-o", output}, fixture.args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ojph_compress %s: %v\n%s", fixture.name, err, text)
		}
		if fixture.syntax.UID != transfer.HTJ2K.UID {
			continue
		}
		expanded := filepath.Join(dir, "out.pgm")
		if text, err := exec.Command(expand, "-i", output, "-o", expanded).CombinedOutput(); err != nil {
			t.Fatalf("ojph_expand %s: %v\n%s", fixture.name, err, text)
		}
		reference, err := frameOfPNM(expanded, tc.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(openJPHFixtureDir, fixture.name+".raw"), reference, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// frameOfPNM reads an 8-bit binary PGM written by ojph_expand as a native
// frame.
func frameOfPNM(path string, m pixeldata.Metadata) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	samples := int(m.Rows) * int(m.Columns) * int(m.SamplesPerPixel)
	if m.BitsAllocated != 8 || len(data) < samples {
		return nil, os.ErrInvalid
	}
	return append([]byte(nil), data[len(data)-samples:]...), nil
}
