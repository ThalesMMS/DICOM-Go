package jpegls_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type lsImage struct {
	samples, allocated, stored uint16
	photometric                string
}

const lsRows, lsColumns = 41, 57

func (m lsImage) pixels() []byte {
	sampleBytes := int(m.allocated / 8)
	top := uint32(1)<<m.stored - 1
	out := make([]byte, lsRows*lsColumns*int(m.samples)*sampleBytes)
	state := uint32(12345)
	i := 0
	for y := 0; y < lsRows; y++ {
		for x := 0; x < lsColumns; x++ {
			for s := 0; s < int(m.samples); s++ {
				state = state*1103515245 + 12345
				value := (uint32(x*y*7+s*300) + (state>>20)%16) & top
				if y > 25 {
					value = top / 3
				}
				if sampleBytes == 2 {
					binary.LittleEndian.PutUint16(out[i:], uint16(value))
				} else {
					out[i] = byte(value)
				}
				i += sampleBytes
			}
		}
	}
	return out
}

func (m lsImage) object(pixels []byte) *object.Object {
	vr := core.VROB
	if m.allocated > 8 {
		vr = core.VROW
	}
	elements := []core.Element{
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.953.3"),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, nil, lsRows),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0011), core.VRUS, nil, lsColumns),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0002), core.VRUS, nil, m.samples),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, m.photometric),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, nil, m.allocated),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0101), core.VRUS, nil, m.stored),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0102), core.VRUS, nil, m.stored-1),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, nil, 0),
		core.NewRawElement(core.TagPixelData, vr, pixels),
	}
	if m.samples == 3 {
		elements = append(elements, dicomtest.Uint16Element(core.NewTag(0x0028, 0x0006), core.VRUS, nil, 0))
	}
	return object.FromElements(elements, nil)
}

// lsSamples returns the samples of a native data set, color-by-pixel.
func lsSamples(t *testing.T, dataset *object.Object, m lsImage) []int {
	t.Helper()
	raw, ok := dataset.GetRaw(core.TagPixelData)
	if !ok {
		t.Fatal("no Pixel Data")
	}
	count := lsRows * lsColumns * int(m.samples)
	out := make([]int, count)
	for i := range out {
		if m.allocated == 16 {
			out[i] = int(binary.LittleEndian.Uint16(raw[2*i:]))
		} else {
			out[i] = int(raw[i])
		}
	}
	if planar, err := dataset.GetInt(core.NewTag(0x0028, 0x0006)); err == nil && planar == 1 && m.samples == 3 {
		pixels := lsRows * lsColumns
		interleaved := make([]int, count)
		for p := 0; p < pixels; p++ {
			for s := 0; s < 3; s++ {
				interleaved[p*3+s] = out[s*pixels+p]
			}
		}
		out = interleaved
	}
	return out
}

// TestDecoderAgainstDCMCJPLS decodes the JPEG-LS streams DCMTK dcmcjpls
// (CharLS) writes: lossless in raw and cooked mode, line and sample
// interleaved color, custom thresholds and reset (an LSE segment),
// non-standard zero padding, and near-lossless with NEAR 2 and 5. Lossless
// samples must equal the originals; near-lossless samples must equal
// dcmdjpls's decoding of the same stream (JPEG-LS decoding is exact) and lie
// within NEAR of the originals. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestDecoderAgainstDCMCJPLS(t *testing.T) {
	dcmcjpls := dcmtkoracle.Tool(t, "dcmcjpls")
	dcmdjpls := dcmtkoracle.Tool(t, "dcmdjpls")
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	images := []struct {
		name  string
		image lsImage
	}{
		{"mono8", lsImage{1, 8, 8, "MONOCHROME2"}},
		{"mono12", lsImage{1, 16, 12, "MONOCHROME2"}},
		{"mono16", lsImage{1, 16, 16, "MONOCHROME2"}},
		{"rgb8", lsImage{3, 8, 8, "RGB"}},
	}
	modes := []struct {
		name string
		args []string
		near int
	}{
		{"lossless", []string{"+el"}, 0},
		{"cooked", []string{"+el", "+pc"}, 0},
		{"sample-interleaved", []string{"+el", "+is"}, 0},
		{"thresholds", []string{"+el", "+t1", "2", "+t2", "5", "+t3", "12", "+rs", "32"}, 0},
		{"zero-padding", []string{"+el", "+pz"}, 0},
		{"near2", []string{"+en", "+md", "2"}, 2},
		{"near5", []string{"+en", "+md", "5"}, 5},
	}
	for _, img := range images {
		m := img.image
		pixels := m.pixels()
		original := lsSamples(t, m.object(pixels), m)
		input := filepath.Join(dir, img.name+".dcm")
		var encoded bytes.Buffer
		if err := object.WriteFile(&encoded, &object.File{Dataset: m.object(pixels), TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input, encoded.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, mode := range modes {
			if mode.name == "thresholds" && m.stored > 8 {
				continue // the thresholds above suit 8-bit samples
			}
			t.Run(img.name+"-"+mode.name, func(t *testing.T) {
				tolerance := dcmtkoracle.Exact
				if mode.near > 0 {
					tolerance = fmt.Sprintf("exact against dcmdjpls, within NEAR=%d of the original", mode.near)
				}
				compressed := filepath.Join(dir, img.name+"-"+mode.name+".dcm")
				dcmtkoracle.Run(t, tolerance, dcmcjpls, append(append([]string{}, mode.args...), input, compressed)...)
				file, err := object.OpenFile(compressed)
				if err != nil {
					t.Fatal(err)
				}
				native, _, err := pixeldata.TranscodeDataSet(context.Background(), file.Dataset, file.TransferSyntax, transfer.ExplicitVRLittleEndian,
					pixeldata.TranscodeOptions{DecoderRegistry: decoders, AllowLossy: true})
				if err != nil {
					t.Fatalf("decoding %s: %v", file.TransferSyntax.UID, err)
				}
				got := lsSamples(t, native, m)
				want := original
				if mode.near > 0 {
					reference := filepath.Join(dir, img.name+"-"+mode.name+"-dcmtk.dcm")
					dcmtkoracle.Run(t, tolerance, dcmdjpls, compressed, reference)
					referenceFile, err := object.OpenFile(reference)
					if err != nil {
						t.Fatal(err)
					}
					want = lsSamples(t, referenceFile.Dataset, m)
					for i := range original {
						if d := got[i] - original[i]; d > mode.near || d < -mode.near {
							t.Fatalf("sample %d is %d, beyond NEAR of %d", i, got[i], original[i])
						}
					}
				}
				var mismatches []string
				for i := range want {
					if got[i] != want[i] && len(mismatches) < 5 {
						mismatches = append(mismatches, fmt.Sprintf("%d: %d want %d", i, got[i], want[i]))
					}
				}
				if len(mismatches) > 0 {
					t.Fatalf("samples differ: %s", strings.Join(mismatches, "; "))
				}
			})
		}
	}
}
