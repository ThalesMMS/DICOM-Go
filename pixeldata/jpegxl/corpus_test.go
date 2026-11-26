package jpegxl_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegxl"
)

// The local JPEGXL-Fixture corpus (not committed) is compared with libjxl
// djxl frame by frame when JPEGXL_FIXTURE_DIR and DICOMGO_JXL_ORACLE=1 are
// set. Unsupported codestreams are counted, not failed.
func TestLocalFixtureCorpusMatchesDjxl(t *testing.T) {
	dir := os.Getenv("JPEGXL_FIXTURE_DIR")
	if dir == "" || os.Getenv("DICOMGO_JXL_ORACLE") != "1" {
		t.Skip("set JPEGXL_FIXTURE_DIR and DICOMGO_JXL_ORACLE=1 to compare the local corpus with djxl")
	}
	djxl, err := exec.LookPath("djxl")
	if err != nil {
		t.Fatal("djxl not found on PATH")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	tmp := t.TempDir()
	counts := map[string]int{}
	for _, path := range paths {
		r, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := object.ReadFile(r)
		r.Close()
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		pixel, err := pixeldata.ExtractView(f.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		m, err := pixeldata.ExtractMetadata(f.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := jpegxl.New().Decode(pixel, f.Dataset)
		if err != nil {
			counts[fmt.Sprintf("%s: %v", f.TransferSyntax.UID, err)]++
			continue
		}
		for i, fragment := range pixel.Sequence.Fragments {
			in, out := filepath.Join(tmp, "f.jxl"), filepath.Join(tmp, "f.pnm")
			if err := os.WriteFile(in, fragment, 0o600); err != nil {
				t.Fatal(err)
			}
			if msg, err := exec.Command(djxl, in, out).CombinedOutput(); err != nil {
				t.Fatalf("%s: djxl: %v %s", filepath.Base(path), err, msg)
			}
			want, err := pnmToNative(out, m)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(frames.Data[i], want) {
				t.Fatalf("%s frame %d differs from djxl", filepath.Base(path), i)
			}
		}
		counts[f.TransferSyntax.UID+": exact"]++
	}
	for k, v := range counts {
		t.Logf("%s: %d files", k, v)
	}
}

func pnmToNative(path string, m pixeldata.Metadata) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fields []string
	pos := 0
	for len(fields) < 4 {
		for pos < len(data) && (data[pos] == ' ' || data[pos] == '\n') {
			pos++
		}
		start := pos
		for pos < len(data) && data[pos] != ' ' && data[pos] != '\n' {
			pos++
		}
		fields = append(fields, string(data[start:pos]))
	}
	pos++
	maxval, _ := strconv.Atoi(fields[3])
	raster := data[pos:]
	n := int(m.Rows) * int(m.Columns) * int(m.SamplesPerPixel)
	out := make([]byte, n*int(m.BitsAllocated/8))
	for i := 0; i < n; i++ {
		v := uint16(raster[i])
		if maxval > 255 {
			v = binary.BigEndian.Uint16(raster[2*i:])
		}
		if m.BitsAllocated == 8 {
			out[i] = byte(v)
		} else {
			binary.LittleEndian.PutUint16(out[2*i:], v)
		}
	}
	return out, nil
}
