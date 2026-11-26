package hdf5

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fixtures were written by the HDF5 C library in the layout of ITK's
// HDF5TransformIO; see registration/testdata/itkhdf5/README.md.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "registration", "testdata", "itkhdf5", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func open(t testing.TB, data []byte) *File {
	t.Helper()
	f, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestOpenReadsBothSuperblockFormats(t *testing.T) {
	for _, c := range []struct {
		name       string
		superblock int
	}{{"composite.h5", 2}, {"composite_v0.h5", 0}} {
		data := fixture(t, c.name)
		if !IsHDF5(data) {
			t.Fatalf("%s: no signature", c.name)
		}
		f := open(t, data)
		if f.Superblock != c.superblock {
			t.Fatalf("%s: superblock %d, want %d", c.name, f.Superblock, c.superblock)
		}
		root, err := f.Links("/")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, l := range root {
			names = append(names, l.Name)
		}
		got := strings.Join(sorted(names), ",")
		if got != "HDFVersion,ITKVersion,OSName,OSVersion,TransformGroup" {
			t.Fatalf("%s: root links %s", c.name, got)
		}
		if v, err := f.ReadString("/ITKVersion"); err != nil || v != "6.0.0" {
			t.Fatalf("%s: ITKVersion %q, %v", c.name, v, err)
		}
		if v, err := f.ReadString("/HDFVersion"); err != nil || !strings.HasPrefix(v, "HDF5 library version: 1.") {
			t.Fatalf("%s: HDFVersion %q, %v", c.name, v, err)
		}
		for i, want := range []string{"CompositeTransform_double_3_3", "AffineTransform_double_3_3", "Euler3DTransform_double_3_3"} {
			v, err := f.ReadString("/TransformGroup/" + strconv.Itoa(i) + "/TransformType")
			if err != nil || v != want {
				t.Fatalf("%s: transform %d type %q, %v", c.name, i, v, err)
			}
		}
		values, err := f.ReadFloat64s("/TransformGroup/2/TransformParameters", 100)
		if err != nil {
			t.Fatal(err)
		}
		want := []float64{0.1, -0.2, 0.3, 5, -6, 7}
		for i := range want {
			if values[i] != want[i] {
				t.Fatalf("%s: parameters %v", c.name, values)
			}
		}
		if f.Exists("/TransformGroup/2/Nothing") || !f.Exists("/TransformGroup/2") {
			t.Fatalf("%s: Exists", c.name)
		}
		if _, err := f.ReadFloat64s("/TransformGroup/2/TransformParameters", 5); !errors.Is(err, ErrFormat) {
			t.Fatalf("%s: element limit: %v", c.name, err)
		}
		if _, err := f.ReadFloat64s("/TransformGroup/9/TransformParameters", 5); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: missing path: %v", c.name, err)
		}
		if _, err := f.ReadFloat64s("/TransformGroup/0/TransformType", 5); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s: string as floats: %v", c.name, err)
		}
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// TestChunkedFilteredDataset reads a 360-element dataset stored in one
// element chunks, shuffled and deflated, indexed by a multi-level B-tree.
func TestChunkedFilteredDataset(t *testing.T) {
	for _, name := range []string{"field_chunks_v0.h5", "affine_field.h5"} {
		f := open(t, fixture(t, name))
		path := "/TransformGroup/0/TransformParameters"
		if name == "affine_field.h5" {
			path = "/TransformGroup/2/TransformParameters"
		}
		values, err := f.ReadFloat64s(path, 1000)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(values) != 360 {
			t.Fatalf("%s: %d values", name, len(values))
		}
		for i, v := range values {
			if want := float64((i*37)%101-50) / 64; v != want {
				t.Fatalf("%s: value %d is %v, want %v", name, i, v, want)
			}
		}
	}
}

func TestFloat32AndManySymbolTableNodes(t *testing.T) {
	f := open(t, fixture(t, "affine_float_misspelled_v0.h5"))
	values, err := f.ReadFloat64s("/TransformGroup/0/TranformParameters", 100)
	if err != nil {
		t.Fatal(err)
	}
	if values[0] != float64(float32(1.1)) || values[9] != -4 {
		t.Fatalf("float parameters %v", values)
	}
	f = open(t, fixture(t, "many_v0.h5"))
	links, err := f.Links("/TransformGroup")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 10 {
		t.Fatalf("%d links", len(links))
	}
	if v, err := f.ReadString("/TransformGroup/9/TransformType"); err != nil || v != "Euler3DTransform_double_3_3" {
		t.Fatalf("transform 9: %q %v", v, err)
	}
}

func TestDenseLinkStorage(t *testing.T) {
	f := open(t, fixture(t, "many_dense.h5"))
	links, err := f.Links("/TransformGroup")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 10 {
		t.Fatalf("dense group: got %d links, want 10", len(links))
	}
	for i := range links {
		name := "/TransformGroup/" + strconv.Itoa(i) + "/TransformType"
		v, err := f.ReadString(name)
		if err != nil || (i == 0 && v != "CompositeTransform_double_3_3") ||
			(i > 0 && v != "Euler3DTransform_double_3_3") {
			t.Fatalf("%s: %q %v", name, v, err)
		}
	}
}

func TestOpenRejectsOtherFiles(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("not an HDF5 file at all"), append(append([]byte{}, Signature...), 9)} {
		if _, err := Open(bytes.NewReader(data), int64(len(data))); err == nil {
			t.Fatalf("%q opened", data)
		}
	}
}

// TestUserBlock: a superblock after a 512-byte user block is found, and
// addresses are relative to it.
func TestUserBlock(t *testing.T) {
	data := append(make([]byte, 512), fixture(t, "composite_v0.h5")...)
	f := open(t, data)
	if v, err := f.ReadString("/TransformGroup/1/TransformType"); err != nil || v != "AffineTransform_double_3_3" {
		t.Fatalf("type %q %v", v, err)
	}
}

// walk reads everything a transform reader would.
func walk(f *File) {
	links, err := f.Links("/TransformGroup")
	if err != nil {
		return
	}
	for i := range links {
		base := "/TransformGroup/" + strconv.Itoa(i)
		_, _ = f.ReadString(base + "/TransformType")
		for _, name := range []string{"TransformParameters", "TransformFixedParameters", "TranformParameters", "TranformFixedParameters"} {
			_, _ = f.ReadFloat64s(base+"/"+name, 1<<20)
		}
	}
}

// TestDamagedFilesFailWithoutPanicking truncates and corrupts every
// fixture: reads return errors, never panic or run away.
func TestDamagedFilesFailWithoutPanicking(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "registration", "testdata", "itkhdf5"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".h5") {
			continue
		}
		data := fixture(t, e.Name())
		for cut := 0; cut < len(data); cut += 97 {
			if f, err := Open(bytes.NewReader(data[:cut]), int64(cut)); err == nil {
				walk(f)
			}
		}
		for at := 0; at < len(data); at += 113 {
			for _, flip := range []byte{0xFF, 0x01} {
				damaged := append([]byte(nil), data...)
				damaged[at] ^= flip
				if f, err := Open(bytes.NewReader(damaged), int64(len(damaged))); err == nil {
					walk(f)
				}
			}
		}
	}
}

func FuzzRead(f *testing.F) {
	for _, name := range []string{"composite.h5", "composite_v0.h5", "affine_field.h5"} {
		f.Add(fixture(f, name))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if file, err := Open(bytes.NewReader(data), int64(len(data))); err == nil {
			walk(file)
		}
	})
}

func BenchmarkReadChunkedDataset(b *testing.B) {
	data := fixture(b, "field_chunks_v0.h5")
	b.SetBytes(360 * 8)
	for i := 0; i < b.N; i++ {
		f, err := Open(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := f.ReadFloat64s("/TransformGroup/0/TransformParameters", 1000); err != nil {
			b.Fatal(err)
		}
	}
}
