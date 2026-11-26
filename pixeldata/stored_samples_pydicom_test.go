package pixeldata_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const storedSamplesPydicomEnableEnv = "DICOM_GO_PYDICOM_STORED_SAMPLES"

type storedSampleOracleCase struct {
	name   string
	format pixeldata.SampleFormat
	values []int64 // interpreted values; encoded as BitsStored two's complement
}

func storedSampleOracleCases() []storedSampleOracleCase {
	f := func(allocated, stored int, signed bool) pixeldata.SampleFormat {
		return pixeldata.SampleFormat{BitsAllocated: allocated, BitsStored: stored, HighBit: stored - 1, Signed: signed}
	}
	return []storedSampleOracleCase{
		{"s13", f(16, 13, true), []int64{-4096, -2000, -1, 0, 1, 2000, 4095, -2048}},
		{"s12", f(16, 12, true), []int64{-2048, -1024, -1, 0, 1, 1024, 2047, 7}},
		{"u12", f(16, 12, false), []int64{0, 1, 2047, 2048, 4095, 100, 3000, 4094}},
		{"s16", f(16, 16, true), []int64{-32768, -1, 0, 32767, -12345, 12345, 1, -2}},
		{"u16", f(16, 16, false), []int64{0, 65535, 32768, 32767, 1, 65534, 4096, 12345}},
		{"s8", f(8, 8, true), []int64{-128, -1, 0, 127, -64, 64, 1, -2}},
		{"u8", f(8, 8, false), []int64{0, 255, 128, 127, 1, 254, 16, 200}},
		{"s32", f(32, 32, true), []int64{-1 << 31, -1, 0, 1<<31 - 1, -123456789, 123456789, 1, -2}},
		{"u32", f(32, 32, false), []int64{0, 1<<32 - 1, 1 << 31, 1<<31 - 1, 1, 4000000000, 65536, 7}},
		{"s24", f(32, 24, true), []int64{-1 << 23, -1, 0, 1<<23 - 1, -70000, 70000, 1, -2}},
	}
}

func (c storedSampleOracleCase) dataset(index int) *object.Object {
	width := c.format.BitsAllocated / 8
	pixels := make([]byte, 0, width*len(c.values))
	mask := uint64(1)<<c.format.BitsStored - 1
	for _, v := range c.values {
		raw := uint64(v) & mask
		switch width {
		case 1:
			pixels = append(pixels, byte(raw))
		case 2:
			pixels = binary.LittleEndian.AppendUint16(pixels, uint16(raw))
		case 4:
			pixels = binary.LittleEndian.AppendUint32(pixels, uint32(raw))
		}
	}
	us := func(element, v uint16) core.Element {
		return dicomtest.Uint16Element(core.NewTag(0x0028, element), core.VRUS, binary.LittleEndian, v)
	}
	pr := uint16(0)
	if c.format.Signed {
		pr = 1
	}
	vr := core.VROW
	if width == 1 {
		vr = core.VROB
	}
	return object.FromElements([]core.Element{
		dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, fmt.Sprintf("2.25.935%d", index)),
		us(0x0002, 1),
		dicomtest.StringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		us(0x0010, 2),
		us(0x0011, uint16(len(c.values)/2)),
		us(0x0100, uint16(c.format.BitsAllocated)),
		us(0x0101, uint16(c.format.BitsStored)),
		us(0x0102, uint16(c.format.HighBit)),
		us(0x0103, pr),
		core.NewRawElement(core.TagPixelData, vr, pixels),
	}, nil)
}

// TestPydicomStoredSamplesOracle compares every interpreted sample with
// pydicom 3.0.2/NumPy 2.1.3 pixel_array for little- and big-endian Part 10
// files written by Go. Enable with DICOM_GO_PYDICOM_STORED_SAMPLES=1 and
// DICOM_GO_PYTHON; without the gate the test skips and qualifies nothing.
func TestPydicomStoredSamplesOracle(t *testing.T) {
	if os.Getenv(storedSamplesPydicomEnableEnv) != "1" {
		t.Skip("set " + storedSamplesPydicomEnableEnv + "=1 and DICOM_GO_PYTHON to run the pydicom stored-sample oracle")
	}
	python := strings.TrimSpace(os.Getenv("DICOM_GO_PYTHON"))
	if python == "" {
		t.Fatal("DICOM_GO_PYTHON is required when the pydicom stored-sample oracle is enabled")
	}
	ctx := context.Background()
	dir := t.TempDir()
	type goSide struct {
		values []int64
	}
	expected := map[string]goSide{}
	for index, c := range storedSampleOracleCases() {
		little := c.dataset(index)
		big, _, err := pixeldata.TranscodeDataSet(ctx, little, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []struct {
			suffix  string
			dataset *object.Object
			syntax  transfer.Syntax
		}{{"le", little, transfer.ExplicitVRLittleEndian}, {"be", big, transfer.ExplicitVRBigEndian}} {
			name := c.name + "-" + v.suffix
			writeWidePart10(t, filepath.Join(dir, name+".dcm"), v.dataset, v.syntax)
			file, err := object.OpenFile(filepath.Join(dir, name+".dcm"))
			if err != nil {
				t.Fatal(err)
			}
			native, err := pixeldata.ExtractNativeFrames(file.Dataset)
			_ = file.Close()
			if err != nil {
				t.Fatal(err)
			}
			got, err := pixeldata.StoredSamples[int64](ctx, native.Data[0], native.Metadata.SampleFormat(v.syntax.ByteOrder), 0)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			expected[name] = goSide{values: got}
			for i, want := range c.values {
				if got[i] != want {
					t.Fatalf("%s: Go sample %d = %d, want %d", name, i, got[i], want)
				}
			}
		}
	}
	script := filepath.Join(dir, "stored_samples_oracle.py")
	if err := os.WriteFile(script, []byte(storedSamplesPydicomScript), 0o600); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(runCtx, python, script, dir).Output()
	if err != nil {
		t.Fatalf("pydicom stored-sample oracle failed: %v\n%s", err, output)
	}
	var oracle map[string][]int64
	if err := json.Unmarshal(output, &oracle); err != nil {
		t.Fatalf("oracle output: %v\n%s", err, output)
	}
	if len(oracle) != len(expected) {
		t.Fatalf("oracle read %d files, want %d", len(oracle), len(expected))
	}
	for name, side := range expected {
		values := oracle[name]
		if len(values) != len(side.values) {
			t.Fatalf("%s: pydicom %d samples, Go %d", name, len(values), len(side.values))
		}
		for i := range values {
			if values[i] != side.values[i] {
				t.Fatalf("%s: sample %d pydicom %d, Go %d", name, i, values[i], side.values[i])
			}
		}
	}
}

const storedSamplesPydicomScript = `import json, pathlib, sys
import pydicom
root = pathlib.Path(sys.argv[1])
out = {}
for path in sorted(root.glob("*.dcm")):
    ds = pydicom.dcmread(path)
    out[path.stem] = [int(v) for v in ds.pixel_array.reshape(-1)]
print(json.dumps(out))
`
