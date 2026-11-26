package jpeg2000

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
)

// openJPEGCase is one codestream feature exercised against OpenJPEG.
type openJPEGCase struct {
	name                string
	columns, rows       int
	components, bits    int
	signed              bool
	args                []string
	reduce              int
	lossy               bool
	maxError, meanError float64
}

// openJPEGFeatureCases cover what the core backend decodes beyond the
// qualification corpus: every progression order with precincts, tiles,
// offsets and layers, each code-block style option alone and together, POC,
// subsampling and reduced resolution.
var openJPEGFeatureCases = []openJPEGCase{
	{name: "rpcl-precincts-tiles", columns: 97, rows: 83, components: 3, bits: 8, args: []string{"-p", "RPCL", "-c", "[32,32],[16,16]", "-n", "4", "-r", "30,10,1", "-t", "64,48"}},
	{name: "pcrl-precincts-offsets", columns: 97, rows: 83, components: 3, bits: 8, args: []string{"-p", "PCRL", "-c", "[16,32],[16,16],[8,8]", "-n", "3", "-d", "5,3", "-T", "2,1", "-t", "40,40"}},
	{name: "cprl-precincts", columns: 61, rows: 70, components: 3, bits: 8, args: []string{"-p", "CPRL", "-c", "[32,32],[16,16]", "-n", "4", "-r", "20,5,1"}},
	{name: "lrcp-precincts-12bit", columns: 64, rows: 50, components: 1, bits: 12, args: []string{"-c", "[64,64],[32,32],[16,16],[8,8]", "-n", "5", "-b", "16,16"}},
	{name: "rlcp-precincts-sop-eph", columns: 50, rows: 64, components: 1, bits: 16, args: []string{"-p", "RLCP", "-c", "[16,16],[16,16],[16,16],[16,16],[16,16],[16,16]", "-SOP", "-EPH", "-r", "10,1"}},
	{name: "bypass", columns: 64, rows: 64, components: 1, bits: 16, args: []string{"-M", "1"}},
	{name: "reset", columns: 64, rows: 64, components: 1, bits: 8, args: []string{"-M", "2"}},
	{name: "termall", columns: 64, rows: 64, components: 1, bits: 12, args: []string{"-M", "4", "-r", "8,1"}},
	{name: "vsc", columns: 61, rows: 45, components: 1, bits: 12, args: []string{"-M", "8"}},
	{name: "pterm", columns: 64, rows: 64, components: 1, bits: 8, args: []string{"-M", "16"}},
	{name: "segsym", columns: 64, rows: 64, components: 1, bits: 8, args: []string{"-M", "32"}},
	{name: "bypass-termall-layers", columns: 70, rows: 66, components: 3, bits: 8, args: []string{"-M", "5", "-r", "30,10,1", "-n", "4"}},
	{name: "all-styles-signed", columns: 57, rows: 63, components: 1, bits: 16, signed: true, args: []string{"-M", "63", "-r", "20,4,1", "-c", "[32,32],[16,16]"}},
	{name: "all-styles-lossy", columns: 64, rows: 64, components: 3, bits: 8, args: []string{"-M", "63", "-I", "-r", "10"}, lossy: true, maxError: 1, meanError: 0.05},
	{name: "subsampled", columns: 37, rows: 23, components: 1, bits: 8, args: []string{"-s", "2,2"}},
	{name: "subsampled-color-offsets", columns: 41, rows: 27, components: 3, bits: 8, args: []string{"-s", "2,3", "-d", "3,1", "-mct", "0"}},
	{name: "reduced-1", columns: 97, rows: 83, components: 1, bits: 12, args: []string{"-n", "4", "-t", "40,40"}, reduce: 1},
	{name: "reduced-3-precincts", columns: 97, rows: 83, components: 3, bits: 8, args: []string{"-n", "5", "-c", "[32,32],[16,16]", "-p", "RPCL"}, reduce: 3},
	{name: "reduced-2-lossy", columns: 64, rows: 64, components: 1, bits: 16, args: []string{"-I", "-n", "4", "-r", "8"}, reduce: 2, lossy: true, maxError: 1, meanError: 0.05},
}

// TestCoreFeaturesAgainstOpenJPEG encodes synthetic images with opj_compress
// and requires the core backend to reproduce opj_decompress: exactly for
// reversible codestreams (and then also the source samples, at full
// resolution), within a stated error for irreversible ones. Enable with
// DICOM_GO_OPENJPEG_COMPRESS and DICOM_GO_OPENJPEG_DECOMPRESS naming the
// OpenJPEG 2.5 tools.
func TestCoreFeaturesAgainstOpenJPEG(t *testing.T) {
	compress, decompress := os.Getenv("DICOM_GO_OPENJPEG_COMPRESS"), os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if compress == "" || decompress == "" {
		t.Skip("opt-in: set DICOM_GO_OPENJPEG_COMPRESS and DICOM_GO_OPENJPEG_DECOMPRESS")
	}
	dir := t.TempDir()
	for _, tc := range openJPEGFeatureCases {
		t.Run(tc.name, func(t *testing.T) {
			source := openJPEGSource(tc)
			input := filepath.Join(dir, tc.name+".rawl")
			if err := os.WriteFile(input, planarBytes(source, tc.bits), 0o600); err != nil {
				t.Fatal(err)
			}
			codestream := filepath.Join(dir, tc.name+".j2k")
			sign := "u"
			if tc.signed {
				sign = "s"
			}
			args := []string{"-i", input, "-o", codestream, "-F", fmt.Sprintf("%d,%d,%d,%d,%s", tc.columns, tc.rows, tc.components, tc.bits, sign)}
			runOpenJPEG(t, compress, append(args, tc.args...)...)
			decoded := filepath.Join(dir, tc.name+".decoded.rawl")
			args = []string{"-i", codestream, "-o", decoded}
			if tc.reduce > 0 {
				args = append(args, "-r", fmt.Sprint(tc.reduce))
			}
			runOpenJPEG(t, decompress, args...)

			data, err := os.ReadFile(codestream)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := j2kcore.ParseCodestream(data)
			if err != nil {
				t.Fatal(err)
			}
			image, err := j2kcore.DecodeContextOptions(context.Background(), stream, j2kcore.DefaultLimits(), j2kcore.DecodeOptions{Reduce: tc.reduce})
			if err != nil {
				t.Fatal(err)
			}
			// OpenJPEG writes each component at its own size, one after the
			// other; the core repeats a subsampled component's samples over
			// the output grid, so it is read back at each sample's origin.
			raw, err := os.ReadFile(decoded)
			if err != nil {
				t.Fatal(err)
			}
			want := make([][]int32, len(stream.Components))
			got := make([][]int32, len(stream.Components))
			sx, sy := 1<<tc.reduce, 1<<tc.reduce
			for c, comp := range stream.Components {
				scale := func(v, d int) int { return ceilDiv(v, d) }
				w := scale(int(stream.Width), comp.DX*sx) - scale(int(stream.XOffset), comp.DX*sx)
				h := scale(int(stream.Height), comp.DY*sy) - scale(int(stream.YOffset), comp.DY*sy)
				want[c], raw, err = readPlane(raw, w*h, comp.Depth, comp.Signed)
				if err != nil {
					t.Fatal(err)
				}
				got[c] = subsample(image, c, stream, w, h, tc.reduce)
			}
			if len(raw) != 0 {
				t.Fatalf("%d bytes of OpenJPEG output left over", len(raw))
			}
			worst, sum, count := 0.0, 0.0, 0
			for c := range want {
				for i := range want[c] {
					diff := float64(got[c][i] - want[c][i])
					if diff < 0 {
						diff = -diff
					}
					worst = max(worst, diff)
					sum += diff
					count++
				}
			}
			mean := sum / float64(count)
			t.Logf("%d samples, max |diff| %g, mean %.4f", count, worst, mean)
			if !tc.lossy && worst != 0 {
				t.Fatalf("reversible codestream differs from OpenJPEG by up to %g", worst)
			}
			if tc.lossy && (worst > tc.maxError || mean > tc.meanError) {
				t.Fatalf("irreversible codestream differs from OpenJPEG by up to %g (mean %.4f), allowed %g (%.4f)", worst, mean, tc.maxError, tc.meanError)
			}
			if !tc.lossy && tc.reduce == 0 && !strings.Contains(strings.Join(tc.args, " "), "-s") {
				for c := range source {
					for i := range source[c] {
						if got[c][i] != source[c][i] {
							t.Fatalf("component %d sample %d is %d, source %d", c, i, got[c][i], source[c][i])
						}
					}
				}
			}
		})
	}
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// subsample reads component c of the core image at the origin of each of its
// own w x h samples.
func subsample(image *j2kcore.Image, c int, stream *j2kcore.Codestream, w, h, reduce int) []int32 {
	comp := stream.Components[c]
	fineX, fineY := 255, 255
	for _, other := range stream.Components {
		fineX, fineY = min(fineX, other.DX), min(fineY, other.DY)
	}
	sx, sy := fineX<<reduce, fineY<<reduce
	sxc, syc := comp.DX<<reduce, comp.DY<<reduce
	originX, originY := ceilDiv(int(stream.XOffset), sx), ceilDiv(int(stream.YOffset), sy)
	firstX, firstY := ceilDiv(int(stream.XOffset), sxc), ceilDiv(int(stream.YOffset), syc)
	out := make([]int32, 0, w*h)
	for y := 0; y < h; y++ {
		row := min(max(ceilDiv((firstY+y)*syc, sy)-originY, 0), image.Height-1)
		for x := 0; x < w; x++ {
			col := min(max(ceilDiv((firstX+x)*sxc, sx)-originX, 0), image.Width-1)
			out = append(out, image.Components[c][row*image.Width+col])
		}
	}
	return out
}

func TestReadPlaneRejectsTruncatedOutput(t *testing.T) {
	for _, depth := range []int{8, 16} {
		if plane, _, err := readPlane([]byte{0}, 2, depth, false); err == nil || plane != nil {
			t.Errorf("depth %d: plane %v, error %v", depth, plane, err)
		}
	}
	for _, tc := range []struct {
		raw   []byte
		depth int
		want  []int32
	}{
		{[]byte{255, 2, 3}, 8, []int32{-1, 2}},
		{[]byte{255, 255, 2, 0, 3}, 16, []int32{-1, 2}},
	} {
		plane, rest, err := readPlane(tc.raw, 2, tc.depth, true)
		if err != nil || !reflect.DeepEqual(plane, tc.want) || !reflect.DeepEqual(rest, []byte{3}) {
			t.Errorf("depth %d: plane %v, rest %v, error %v", tc.depth, plane, rest, err)
		}
	}
}

func readPlane(raw []byte, n, depth int, signed bool) ([]int32, []byte, error) {
	size := 1
	if depth > 8 {
		size = 2
	}
	if len(raw) < n*size {
		return nil, nil, fmt.Errorf("OpenJPEG plane: got %d bytes, need %d", len(raw), n*size)
	}
	out := make([]int32, n)
	for i := range out {
		var v int32
		if size == 1 {
			v = int32(raw[i])
			if signed {
				v = int32(int8(raw[i]))
			}
		} else {
			u := binary.LittleEndian.Uint16(raw[2*i:])
			v = int32(u)
			if signed {
				v = int32(int16(u))
			}
		}
		out[i] = v
	}
	return out, raw[n*size:], nil
}

// openJPEGSource is a deterministic synthetic image, one plane per component.
func openJPEGSource(tc openJPEGCase) [][]int32 {
	lo, hi := int64(0), int64(1)<<tc.bits-1
	if tc.signed {
		lo, hi = -(int64(1) << (tc.bits - 1)), int64(1)<<(tc.bits-1)-1
	}
	span := hi - lo + 1
	out := make([][]int32, tc.components)
	for c := range out {
		out[c] = make([]int32, tc.columns*tc.rows)
		for y := 0; y < tc.rows; y++ {
			for x := 0; x < tc.columns; x++ {
				// A smooth gradient with a texture, so that every band and
				// bit plane carries data.
				v := int64(x)*span/int64(tc.columns)/2 + int64(y)*span/int64(tc.rows)/3 + int64((x*131+y*71+c*29+x*y)%97)*span/400
				out[c][y*tc.columns+x] = int32(lo + ((v%span)+span)%span)
			}
		}
	}
	return out
}

// planarBytes is the raw little-endian planar layout opj_compress reads with -F.
func planarBytes(planes [][]int32, bits int) []byte {
	var out []byte
	for _, plane := range planes {
		for _, v := range plane {
			if bits > 8 {
				out = binary.LittleEndian.AppendUint16(out, uint16(v))
			} else {
				out = append(out, byte(v))
			}
		}
	}
	return out
}

func runOpenJPEG(t *testing.T, tool string, args ...string) {
	t.Helper()
	if out, err := exec.Command(tool, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", filepath.Base(tool), args, err, out)
	}
}
