package jpeg2000

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	j2kenc "github.com/ThalesMMS/dicom-go/pixeldata/jpeg2000"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Part 1 streams from OpenJPEG and from the base module's pure-Go encoder
// must decode bit for bit through Register (#1276): go-jpeg2000 returned a
// flat image for them. OpenJPEG fixtures are written by opj_compress from
// the synthetic codecfixture encoder cases; the irreversible one decodes to
// the frame opj_decompress reconstructs, stored beside it as .raw.
// Regenerate with DICOMGO_REGEN_OPENJPEG_FIXTURES=1 (needs opj_compress and
// opj_decompress).
var openJPEGFixtures = []struct {
	name   string
	caseID int
	syntax transfer.Syntax
	args   []string
}{
	{"u8-gray-reversible", 0, transfer.JPEG2000LosslessOnly, []string{"-n", "3"}},
	{"u8-rgb-reversible", 1, transfer.JPEG2000LosslessOnly, []string{"-n", "3"}},
	{"u12-gray-rpcl", 8, transfer.JPEG2000LosslessOnly, []string{"-n", "3", "-p", "RPCL"}},
	{"u16-rgb-tiled", 20, transfer.JPEG2000LosslessOnly, []string{"-n", "3", "-t", "64,64"}},
	{"u8-gray-irreversible", 0, transfer.JPEG2000, []string{"-n", "3", "-I", "-r", "4"}},
}

const openJPEGFixtureDir = "testdata/openjpeg"

func TestRegisterDecodesOpenJPEGPart1Fixtures(t *testing.T) {
	if os.Getenv("DICOMGO_REGEN_OPENJPEG_FIXTURES") == "1" {
		regenerateOpenJPEGFixtures(t)
	}
	cases := codecfixture.JPEG2000LosslessEncoderCases()
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range openJPEGFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			tc := cases[fixture.caseID]
			codestream, err := os.ReadFile(filepath.Join(openJPEGFixtureDir, fixture.name+".j2k"))
			if err != nil {
				t.Fatal(err)
			}
			want := tc.ExpectedFrames[0]
			if fixture.syntax.UID == transfer.JPEG2000.UID {
				if want, err = os.ReadFile(filepath.Join(openJPEGFixtureDir, fixture.name+".raw")); err != nil {
					t.Fatal(err)
				}
			}
			assertRegisterDecodes(t, registry, fixture.syntax, tc.Metadata, codestream, want)
		})
	}
}

func TestRegisterDecodesPureGoPart1Encoder(t *testing.T) {
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, tc := range codecfixture.JPEG2000LosslessEncoderCases() {
		t.Run(tc.Name, func(t *testing.T) {
			encoder, err := j2kenc.NewEncoder(j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000LosslessOnly.UID})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := encoder.EncodeFrame(context.Background(), tc.ExpectedFrames[0], tc.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			metadata := tc.Metadata
			if encoded.PhotometricInterpretation != "" {
				metadata.PhotometricInterpretation = encoded.PhotometricInterpretation
			}
			assertRegisterDecodes(t, registry, transfer.JPEG2000LosslessOnly, metadata, encoded.Data, tc.ExpectedFrames[0])
		})
	}
}

func assertRegisterDecodes(t *testing.T, registry pixeldata.Registry, syntax transfer.Syntax, m pixeldata.Metadata, codestream, want []byte) {
	t.Helper()
	photometric := m.PhotometricInterpretation
	if cod := bytes.Index(codestream, []byte{0xFF, 0x52}); m.SamplesPerPixel == 3 && cod >= 0 && codestream[cod+8] == 1 {
		photometric = "YBR_RCT"
		if syntax.UID == transfer.JPEG2000.UID {
			photometric = "YBR_ICT"
		}
	}
	obj, pixel := jpeg2000Object(t, jpeg2000MetadataOptions{
		rows: m.Rows, columns: m.Columns, samplesPerPixel: m.SamplesPerPixel,
		bitsAllocated: m.BitsAllocated, bitsStored: m.BitsStored, photometric: photometric,
	}, codestream)
	frames, err := registry.DecodeFrames(syntax.UID, pixel, obj)
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
}

func regenerateOpenJPEGFixtures(t *testing.T) {
	t.Helper()
	compress, err := exec.LookPath("opj_compress")
	if err != nil {
		t.Fatalf("opj_compress: %v", err)
	}
	decompress, err := exec.LookPath("opj_decompress")
	if err != nil {
		t.Fatalf("opj_decompress: %v", err)
	}
	if err := os.MkdirAll(openJPEGFixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := codecfixture.JPEG2000LosslessEncoderCases()
	for _, fixture := range openJPEGFixtures {
		tc := cases[fixture.caseID]
		dir := t.TempDir()
		input := filepath.Join(dir, "in.pgm")
		if tc.Metadata.SamplesPerPixel == 3 {
			input = filepath.Join(dir, "in.ppm")
		}
		if err := os.WriteFile(input, pnmOf(tc.ExpectedFrames[0], tc.Metadata), 0o600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(openJPEGFixtureDir, fixture.name+".j2k")
		if text, err := exec.Command(compress, append([]string{"-i", input, "-o", output}, fixture.args...)...).CombinedOutput(); err != nil {
			t.Fatalf("opj_compress %s: %v\n%s", fixture.name, err, text)
		}
		if fixture.syntax.UID != transfer.JPEG2000.UID {
			continue
		}
		expanded := filepath.Join(dir, "out.pgm")
		if text, err := exec.Command(decompress, "-i", output, "-o", expanded).CombinedOutput(); err != nil {
			t.Fatalf("opj_decompress %s: %v\n%s", fixture.name, err, text)
		}
		reference, err := frameOfPNM(expanded, tc.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(openJPEGFixtureDir, fixture.name+".raw"), reference, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
