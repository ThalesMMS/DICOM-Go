package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Maxshift region-of-interest fixtures for the pure-Go JPEG 2000 core decoder
// (#1250), which lives in examples/codec-adapters/jpeg2000.
//
// OpenJPEG's opj_compress -ROI shifts a whole component and writes the same
// code-blocks it would without the shift, so its codestreams never mix
// region and background coefficients. These are a synthetic construction
// that does, written from ISO/IEC 15444-1 Annex H.1 with this package's own
// reversible tier-1 and tier-2: after the 5/3 transform, every coefficient
// whose position maps into a rectangle of the image is scaled up by 2^s, each
// band's Mb grows by s for its zero bit-planes, and an RGN segment (Srgn 0,
// SPrgn s) follows the QCD. Everything is coded to the last bit-plane, so the
// decode is lossless and the source frame is the reference.
//
// s is the bit length of the largest background coefficient of the component
// plus one, except for the minimal fixture, where it is that bit length
// alone: the smallest s Annex H allows, with background coefficients in
// [2^(s-1), 2^s). OpenJPEG 2.5.4 takes coefficients from 2^(s-1) up for
// region and shifts them, so it decodes the minimal fixture wrongly; the
// others are also OpenJPEG's own reconstruction, bit for bit.
//
// The test always rebuilds the codestreams and requires them to equal the
// committed ones and the committed .raw references to be the source frames;
// DICOMGO_REGEN_RGN_FIXTURES=1 rewrites both, requiring opj_decompress to
// reconstruct the source frame for every fixture but the minimal one.
var maxshiftFixtures = []struct {
	name                       string
	width, height, comps, bits int
	levels                     int
	seed                       int64
	roi                        [4]int // x0, y0, x1, y1 on the image
	shifted                    []int  // components that carry an RGN
	minimal                    bool   // the smallest s, which OpenJPEG misreads
}{
	{"maxshift-u8-gray-rect", 40, 36, 1, 8, 3, 1250, [4]int{9, 7, 27, 23}, []int{0}, false},
	{"maxshift-u8-rgb-rct-c0", 37, 29, 3, 8, 2, 1251, [4]int{0, 0, 19, 14}, []int{0}, false},
	{"maxshift-u10-gray-two-blocks", 150, 20, 1, 10, 2, 1252, [4]int{100, 3, 140, 17}, []int{0}, false},
	{"maxshift-u8-gray-minimal-shift", 40, 36, 1, 8, 3, 1253, [4]int{9, 7, 27, 23}, []int{0}, true},
}

const maxshiftFixtureDir = "../../examples/codec-adapters/jpeg2000/testdata/rgn"

func TestMaxshiftFixturesAreReproducible(t *testing.T) {
	regenerate := os.Getenv("DICOMGO_REGEN_RGN_FIXTURES") == "1"
	for _, fixture := range maxshiftFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			allocated := 8
			if fixture.bits > 8 {
				allocated = 16
			}
			tc := syntheticFrame(fixture.width, fixture.height, fixture.comps, fixture.bits, allocated, false, fixture.seed)
			img := image{width: fixture.width, height: fixture.height, depth: fixture.bits, planes: tc.samples}
			extra := 1
			if fixture.minimal {
				extra = 0
			}
			codestream, err := maxshiftCodestream(img, fixture.levels, fixture.roi, fixture.shifted, extra)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(maxshiftFixtureDir, fixture.name+".j2c")
			rawPath := filepath.Join(maxshiftFixtureDir, fixture.name+".raw")
			if regenerate {
				if err := os.WriteFile(path, codestream, 0o644); err != nil {
					t.Fatal(err)
				}
				reference, err := openJPEGReconstruction(t, path, fixture.width*fixture.height, tc.metadata.SamplesPerPixel, fixture.bits, allocated)
				if err != nil {
					t.Fatal(err)
				}
				if agrees := bytes.Equal(reference, tc.frame); agrees == fixture.minimal {
					t.Fatalf("opj_decompress reconstructs the source frame: %v, want %v", agrees, !fixture.minimal)
				}
				if err := os.WriteFile(rawPath, tc.frame, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			committed, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(committed, codestream) {
				t.Fatal("the committed codestream is not what the construction writes")
			}
			raw, err := os.ReadFile(rawPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, tc.frame) {
				t.Fatal("the committed reference is not the source frame")
			}
		})
	}
}

// maxshiftCodestream is encodeImage's reversible path with the Maxshift
// scaling of Annex H.1 applied to the coefficients inside roi for the
// components listed in shifted. s is the bit length of the component's largest
// background coefficient plus extra.
func maxshiftCodestream(img image, levels int, roi [4]int, shifted []int, extra int) ([]byte, error) {
	w, h := img.width, img.height
	components := len(img.planes)
	mct := components == 3
	resolutions := make([][][]subband, components)
	planes := make([][]int32, components)
	for i, p := range img.planes {
		planes[i] = append([]int32(nil), p...)
		for k := range planes[i] {
			planes[i][k] -= int32(1) << (img.depth - 1)
		}
	}
	if mct {
		forwardRCT(planes[0], planes[1], planes[2])
	}
	for i, plane := range planes {
		forwardDWT53(plane, w, h, levels)
		resolutions[i] = cutBands(plane, w, h, levels)
		for r := range resolutions[i] {
			for b := range resolutions[i][r] {
				band := &resolutions[i][r][b]
				band.exponent = img.depth + band.gain
				if mct {
					band.exponent++
				}
			}
		}
	}

	// Guard bits as the encoder chooses them, for the unshifted coefficients:
	// Mb itself does not change, the RGN adds s to it.
	guard := minGuardBits
	for _, comp := range resolutions {
		for _, res := range comp {
			for _, band := range res {
				guard = max(guard, bitLength(int(band.maxMag))-band.exponent+1)
			}
		}
	}

	inROI := func(band *subband, x, y int) bool {
		// A coefficient of a band at decomposition level n covers the
		// image around (x 2^n, y 2^n).
		ix, iy := x<<band.level, y<<band.level
		return ix >= roi[0] && ix < roi[2] && iy >= roi[1] && iy < roi[3]
	}
	shifts := make([]int, components)
	for _, comp := range shifted {
		background := int32(0)
		for r := range resolutions[comp] {
			for b := range resolutions[comp][r] {
				band := &resolutions[comp][r][b]
				for y := 0; y < band.h; y++ {
					for x := 0; x < band.w; x++ {
						if v := band.coeffs[y*band.w+x]; !inROI(band, x, y) {
							background = max(background, v, -v)
						}
					}
				}
			}
		}
		s := max(bitLength(int(background))+extra, 1)
		shifts[comp] = s
		for r := range resolutions[comp] {
			for b := range resolutions[comp][r] {
				band := &resolutions[comp][r][b]
				for y := 0; y < band.h; y++ {
					for x := 0; x < band.w; x++ {
						if inROI(band, x, y) {
							band.coeffs[y*band.w+x] <<= s
						}
					}
				}
			}
		}
	}

	packets := make([][][]packetBand, len(resolutions[0]))
	var jobs []blockJob
	for r := range packets {
		packets[r] = make([][]packetBand, components)
		for comp := 0; comp < components; comp++ {
			packets[r][comp] = make([]packetBand, len(resolutions[comp][r]))
			for b := range resolutions[comp][r] {
				band := &resolutions[comp][r][b]
				pb := &packets[r][comp][b]
				*pb = packetBand{mb: guard + band.exponent - 1 + shifts[comp]}
				if band.w == 0 || band.h == 0 {
					continue
				}
				pb.cols = (band.w + codeBlockSize - 1) / codeBlockSize
				pb.rows = (band.h + codeBlockSize - 1) / codeBlockSize
				pb.blocks = make([]codedBlock, pb.cols*pb.rows)
				for i := range pb.blocks {
					jobs = append(jobs, blockJob{band: band, out: &pb.blocks[i], x0: i % pb.cols * codeBlockSize, y0: i / pb.cols * codeBlockSize})
				}
			}
		}
	}
	if err := codeBlocks(context.Background(), jobs, false); err != nil {
		return nil, err
	}
	var body []byte
	for r := range packets {
		for comp := 0; comp < components; comp++ {
			body = writePacket(body, packets[r][comp], false)
		}
	}
	out, err := assemble(img, coding{reversible: true}, levels, mct, guard, 0, resolutions[0], body)
	if err != nil {
		return nil, err
	}

	// The RGN segments follow the QCD, in the main header.
	qcd := bytes.Index(out, []byte{0xFF, 0x5C})
	if qcd < 0 {
		return nil, fmt.Errorf("no QCD")
	}
	at := qcd + 2 + int(binary.BigEndian.Uint16(out[qcd+2:]))
	var rgn []byte
	for _, comp := range shifted {
		rgn = append(rgn, 0xFF, 0x5E, 0x00, 0x05, byte(comp), 0, byte(shifts[comp]))
	}
	return append(append(append([]byte(nil), out[:at]...), rgn...), out[at:]...), nil
}

// openJPEGReconstruction decodes a codestream with opj_decompress and returns
// the native little-endian frame.
func openJPEGReconstruction(t *testing.T, path string, pixels int, comps uint16, bits, allocated int) ([]byte, error) {
	t.Helper()
	decompress, err := exec.LookPath("opj_decompress")
	if err != nil {
		return nil, err
	}
	extension := ".pgm"
	if comps == 3 {
		extension = ".ppm"
	}
	output := filepath.Join(t.TempDir(), "out"+extension)
	if text, err := exec.Command(decompress, "-i", path, "-o", output).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("opj_decompress: %v\n%s", err, text)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return nil, err
	}
	width := 1
	if bits > 8 {
		width = 2
	}
	// The samples are the tail of the file, after the PNM header.
	samples := pixels * int(comps)
	if len(data) < samples*width {
		return nil, fmt.Errorf("opj_decompress wrote %d bytes for %d samples", len(data), samples)
	}
	raw := data[len(data)-samples*width:]
	out := make([]byte, 0, samples*allocated/8)
	for i := 0; i < samples; i++ {
		switch {
		case width == 2:
			out = append(out, raw[2*i+1], raw[2*i])
		case allocated == 16:
			out = append(out, raw[i], 0)
		default:
			out = append(out, raw[i])
		}
	}
	return out, nil
}
