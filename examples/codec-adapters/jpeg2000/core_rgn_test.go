package jpeg2000

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
)

// Region-of-interest fixtures (#1250): codestreams written by OpenJPEG's
// opj_compress with -ROI (an RGN marker segment, Maxshift, Annex H) from the
// synthetic codecfixture encoder cases, and the frames opj_decompress
// reconstructs from them, stored beside each as .raw (and, for a layered
// fixture, the first layers alone as .lN.raw). Regenerate with
// DICOMGO_REGEN_RGN_FIXTURES=1 (needs opj_compress and opj_decompress); the
// provenance is in testdata/rgn/README.md.
//
// opj_compress -ROI scales a whole component, and writes the same code-blocks
// it would without the scaling, so these exercise the RGN syntax and the
// region path but never background coefficients; the maxshift-* fixtures,
// built by pixeldata/jpeg2000's TestMaxshiftFixturesAreReproducible, mix the
// two.
var rgnFixtures = []struct {
	name   string
	caseID int
	args   []string
	// layers lists quality-layer counts with their own reference decode.
	layers []int
}{
	{"u8-gray-reversible-c0-u5", 0, []string{"-n", "3", "-ROI", "c=0,U=5"}, nil},
	{"u8-rgb-reversible-c1-u4", 1, []string{"-n", "3", "-ROI", "c=1,U=4"}, nil},
	{"u12-gray-irreversible-c0-u7", 8, []string{"-n", "3", "-I", "-r", "6", "-ROI", "c=0,U=7"}, nil},
	{"u8-rgb-irreversible-layers-c2-u6", 1, []string{"-n", "3", "-I", "-r", "16,6,2", "-ROI", "c=2,U=6"}, []int{1, 2}},
	{"u16-gray-reversible-tiled-c0-u9", 16, []string{"-n", "2", "-t", "16,16", "-ROI", "c=0,U=9"}, nil},
}

const rgnFixtureDir = "testdata/rgn"

func TestCoreDecodesOpenJPEGRegionOfInterestFixtures(t *testing.T) {
	if os.Getenv("DICOMGO_REGEN_RGN_FIXTURES") == "1" {
		regenerateRGNFixtures(t)
	}
	cases := codecfixture.JPEG2000LosslessEncoderCases()
	decoder := NewCoreDecoder(CoreOptions{}).(ContextDecoder)
	codec := NewCore(CoreOptions{})
	for _, fixture := range rgnFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			tc := cases[fixture.caseID]
			codestream := readRGNFixture(t, fixture.name+".j2c")
			if !bytes.Contains(codestream, []byte{0xFF, 0x5E}) {
				t.Fatal("the fixture carries no RGN marker segment")
			}
			want := readRGNFixture(t, fixture.name+".raw")
			got, err := decoder.DecodeFrameContext(context.Background(), codestream, tc.Metadata)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			lossy := contains(fixture.args, "-I")
			compareRGNFrame(t, "all layers", got, want, tc.Metadata, lossy)
			for _, layers := range fixture.layers {
				want := readRGNFixture(t, fmt.Sprintf("%s.l%d.raw", fixture.name, layers))
				got := partial(t, codec, codestream, tc.Metadata, pixeldata.PartialDecodeRequest{QualityLayers: layers, NoFallback: true})
				compareRGNFrame(t, fmt.Sprintf("%d layers", layers), got, want, tc.Metadata, lossy)
			}
			// The code-blocks are those of the unscaled component, so the
			// decode without the RGN is the same frame, exactly, also where
			// 9/7 synthesis rounds differently from OpenJPEG.
			at := bytes.Index(codestream, []byte{0xFF, 0x5E})
			without := append(append([]byte(nil), codestream[:at]...), codestream[at+7:]...)
			plain, err := decoder.DecodeFrameContext(context.Background(), without, tc.Metadata)
			if err != nil {
				t.Fatalf("decode without RGN: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatal("the RGN changes the decode of a whole-component shift")
			}
		})
	}
}

// The maxshift-* fixtures mix region and background coefficients within
// code-blocks (see pixeldata/jpeg2000/maxshift_fixture_test.go); each is
// reversible and decodes to its source frame, which OpenJPEG also
// reconstructs except from the minimal-shift one.
func TestCoreDecodesMaxshiftRegionOfInterest(t *testing.T) {
	decoder := NewCoreDecoder(CoreOptions{}).(ContextDecoder)
	for _, name := range []string{
		"maxshift-u8-gray-rect",
		"maxshift-u8-rgb-rct-c0",
		"maxshift-u10-gray-two-blocks",
		"maxshift-u8-gray-minimal-shift",
	} {
		t.Run(name, func(t *testing.T) {
			codestream := readRGNFixture(t, name+".j2c")
			metadata := coreMetadata(t, codestream)
			got, err := decoder.DecodeFrameContext(context.Background(), codestream, metadata)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			compareRGNFrame(t, "decode", got, readRGNFixture(t, name+".raw"), metadata, false)
		})
	}
}

// The reversible fixtures are lossless: OpenJPEG's reference is the source
// frame itself, which pins the reference as well as the decoder.
func TestOpenJPEGRegionOfInterestReversibleFixturesAreLossless(t *testing.T) {
	cases := codecfixture.JPEG2000LosslessEncoderCases()
	for _, fixture := range rgnFixtures {
		if contains(fixture.args, "-I") {
			continue
		}
		if want := cases[fixture.caseID].ExpectedFrames[0]; !bytes.Equal(readRGNFixture(t, fixture.name+".raw"), want) {
			t.Fatalf("%s: the reference is not the source frame", fixture.name)
		}
	}
}

// An RGN in a tile-part header applies to that tile and replaces the main
// header's: moving the segment there decodes to the same frame, and an RGN
// that names no component, or a style other than Maxshift, is refused.
func TestCoreRegionOfInterestInTilePartHeader(t *testing.T) {
	const name = "maxshift-u8-gray-rect"
	codestream := readRGNFixture(t, name+".j2c")
	want := readRGNFixture(t, name+".raw")
	metadata := coreMetadata(t, codestream)
	decoder := NewCoreDecoder(CoreOptions{}).(ContextDecoder)

	rgnAt := bytes.Index(codestream, []byte{0xFF, 0x5E})
	segment := append([]byte(nil), codestream[rgnAt:rgnAt+7]...) // FF5E, Lrgn 5, Crgn, Srgn, SPrgn
	if segment[3] != 5 {
		t.Fatalf("RGN length %d, want 5 (one-byte component index)", segment[3])
	}
	main := append(append([]byte(nil), codestream[:rgnAt]...), codestream[rgnAt+7:]...)
	moved := insertInTilePartHeader(t, main, segment)
	got, err := decoder.DecodeFrameContext(context.Background(), moved, metadata)
	if err != nil {
		t.Fatalf("RGN in the tile-part header: %v", err)
	}
	compareRGNFrame(t, "tile-part RGN", got, want, metadata, false)

	// Without the RGN the region's coefficients are read at their scaled
	// size: the decode differs, which shows the segment is what restores it.
	if got, err := decoder.DecodeFrameContext(context.Background(), main, metadata); err == nil && bytes.Equal(got, want) {
		t.Fatal("the codestream decodes the same without its RGN")
	}

	// A tile-part RGN of shift 0 overrides the main header's: no ROI there.
	zero := append([]byte(nil), segment...)
	zero[6] = 0
	if got, err := decoder.DecodeFrameContext(context.Background(), insertInTilePartHeader(t, codestream, zero), metadata); err == nil && bytes.Equal(got, want) {
		t.Fatal("a tile-part RGN of shift 0 did not replace the main header's")
	}

	style := append([]byte(nil), segment...)
	style[5] = 1
	if _, err := decoder.DecodeFrameContext(context.Background(), insertInTilePartHeader(t, main, style), metadata); !errors.Is(err, ErrUnsupportedCodestreamFeature) {
		t.Fatalf("RGN style 1: %v, want ErrUnsupportedCodestreamFeature", err)
	}
	component := append([]byte(nil), segment...)
	component[4] = 3
	if _, err := decoder.DecodeFrameContext(context.Background(), insertInTilePartHeader(t, main, component), metadata); !errors.Is(err, ErrMalformedCodestream) {
		t.Fatalf("RGN of component 3: %v, want ErrMalformedCodestream", err)
	}
}

// insertInTilePartHeader places a marker segment right after the first SOT
// segment and grows that tile-part's Psot to match.
func insertInTilePartHeader(t *testing.T, codestream, segment []byte) []byte {
	t.Helper()
	sot := bytes.Index(codestream, []byte{0xFF, 0x90, 0x00, 0x0A})
	if sot < 0 {
		t.Fatal("no SOT segment")
	}
	out := append([]byte(nil), codestream[:sot+12]...)
	out = append(out, segment...)
	out = append(out, codestream[sot+12:]...)
	if psot := beUint32Of(out[sot+6:]); psot != 0 {
		copy(out[sot+6:], beUint32(psot+uint32(len(segment))))
	}
	return out
}

func beUint32Of(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func readRGNFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rgnFixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// compareRGNFrame requires got to equal want, or for an irreversible
// codestream to be within one step of it in every sample, the core's policy
// against OpenJPEG (9/7 synthesis in float32 against OpenJPEG's own path
// rounds a few samples differently; PRODUCTION_PROFILE.md).
func compareRGNFrame(t *testing.T, what string, got, want []byte, m pixeldata.Metadata, lossy bool) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	if len(got) != len(want) {
		t.Fatalf("%s: decoded %d bytes, want %d", what, len(got), len(want))
	}
	width := int(m.BitsAllocated / 8)
	worst, differ := 0, 0
	for i := 0; i < len(got); i += width {
		a, b := int(got[i]), int(want[i])
		if width == 2 {
			a, b = a|int(got[i+1])<<8, b|int(want[i+1])<<8
		}
		if d := max(a-b, b-a); d > 0 {
			differ++
			worst = max(worst, d)
		}
	}
	if !lossy || worst > 1 {
		t.Fatalf("%s: %d samples differ, by up to %d", what, differ, worst)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func regenerateRGNFixtures(t *testing.T) {
	t.Helper()
	compress, err := exec.LookPath("opj_compress")
	if err != nil {
		t.Fatalf("opj_compress: %v", err)
	}
	decompress, err := exec.LookPath("opj_decompress")
	if err != nil {
		t.Fatalf("opj_decompress: %v", err)
	}
	if err := os.MkdirAll(rgnFixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := codecfixture.JPEG2000LosslessEncoderCases()
	for _, fixture := range rgnFixtures {
		tc := cases[fixture.caseID]
		dir := t.TempDir()
		extension := ".pgm"
		if tc.Metadata.SamplesPerPixel == 3 {
			extension = ".ppm"
		}
		input := filepath.Join(dir, "in"+extension)
		if err := os.WriteFile(input, pnmOf(tc.ExpectedFrames[0], tc.Metadata), 0o600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(rgnFixtureDir, fixture.name+".j2c")
		if text, err := exec.Command(compress, append([]string{"-i", input, "-o", output}, fixture.args...)...).CombinedOutput(); err != nil {
			t.Fatalf("opj_compress %s: %v\n%s", fixture.name, err, text)
		}
		references := map[string][]string{fixture.name + ".raw": nil}
		for _, layers := range fixture.layers {
			references[fmt.Sprintf("%s.l%d.raw", fixture.name, layers)] = []string{"-l", strconv.Itoa(layers)}
		}
		for reference, args := range references {
			expanded := filepath.Join(dir, "out"+extension)
			if text, err := exec.Command(decompress, append([]string{"-i", output, "-o", expanded}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("opj_decompress %s: %v\n%s", fixture.name, err, text)
			}
			frame, err := frameOfWidePNM(expanded, tc.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rgnFixtureDir, reference), frame, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// frameOfWidePNM reads a binary PGM or PPM written by opj_decompress, of one
// or two bytes per sample (big-endian), as a native little-endian frame.
func frameOfWidePNM(path string, m pixeldata.Metadata) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	samples := int(m.Rows) * int(m.Columns) * int(m.SamplesPerPixel)
	width := 1
	if m.BitsStored > 8 {
		width = 2
	}
	if len(data) < samples*width {
		return nil, fmt.Errorf("%s: %d bytes for %d samples", path, len(data), samples)
	}
	raw := data[len(data)-samples*width:]
	out := make([]byte, 0, samples*int(m.BitsAllocated/8))
	for i := 0; i < samples; i++ {
		switch {
		case width == 2:
			out = append(out, raw[2*i+1], raw[2*i])
		case m.BitsAllocated == 16:
			out = append(out, raw[i], 0)
		default:
			out = append(out, raw[i])
		}
	}
	return out, nil
}
