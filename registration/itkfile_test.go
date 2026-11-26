package registration

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// The files of testdata/upstream/itk/transforms were written by ITK's
// TransformFileWriter (commit 35b196e1) with
// dicom-go-validation/scripts/itk_transform_files; points.txt holds ITK's
// TransformPoint of three points through each transform.

func itkTransformDir() string {
	return filepath.Join("..", "testdata", "upstream", "itk", "transforms")
}

type itkPointCase struct {
	in, out render.Vec3
}

func itkReferencePoints(t *testing.T) map[string][]itkPointCase {
	t.Helper()
	f, err := os.Open(filepath.Join(itkTransformDir(), "points.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cases := map[string][]itkPointCase{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 7 {
			t.Fatalf("points.txt line %q", scanner.Text())
		}
		var v [6]float64
		for i := range v {
			if v[i], err = strconv.ParseFloat(fields[i+1], 64); err != nil {
				t.Fatal(err)
			}
		}
		cases[fields[0]] = append(cases[fields[0]], itkPointCase{render.Vec3{X: v[0], Y: v[1], Z: v[2]}, render.Vec3{X: v[3], Y: v[4], Z: v[5]}})
	}
	return cases
}

func readITKSample(t *testing.T, name string) (Transform, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(itkTransformDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	transform, err := ReadTransformFile(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return transform, data
}

// TestReadITKWrittenTransformFiles: every file ITK wrote maps the reference
// points as ITK does, with the expected transform family; the composite
// applies its last listed transform first.
func TestReadITKWrittenTransformFiles(t *testing.T) {
	reference := itkReferencePoints(t)
	for _, test := range []struct {
		file, points string
		kind         Kind
		tolerance    float64
	}{
		{"euler.tfm", "euler", KindEuler3D, 1e-9},
		{"euler.mat", "euler", KindEuler3D, 1e-9},
		{"euler_zyx.tfm", "euler_zyx", KindEuler3D, 1e-9},
		{"versor.tfm", "versor", KindVersorRigid3D, 1e-9},
		{"similarity.tfm", "similarity", KindSimilarity3D, 1e-9},
		{"affine.tfm", "affine", KindAffine, 1e-9},
		{"affine.mat", "affine", KindAffine, 1e-9},
		// Single precision parameters.
		{"affine_float.mat", "affine", KindAffine, 1e-4},
		{"composite.tfm", "composite", KindComposite, 1e-9},
	} {
		t.Run(test.file, func(t *testing.T) {
			transform, _ := readITKSample(t, test.file)
			if transform.Kind() != test.kind {
				t.Fatalf("kind %s, want %s", transform.Kind(), test.kind)
			}
			cases := reference[test.points]
			if len(cases) == 0 {
				t.Fatalf("no reference points for %s", test.points)
			}
			for _, c := range cases {
				if got := transform.TransformPoint(c.in); got.Sub(c.out).Length() > test.tolerance*math.Max(1, c.out.Length()) {
					t.Fatalf("%v maps to %v, ITK %v", c.in, got, c.out)
				}
			}
		})
	}
	composite, _ := readITKSample(t, "composite.tfm")
	c := composite.(*Composite)
	if c.Len() != 2 || c.Transform(0).Kind() != KindEuler3D || c.Transform(1).Kind() != KindAffine {
		t.Fatalf("composite application order %s, %s; want the Euler transform (listed last) first", c.Transform(0).Kind(), c.Transform(1).Kind())
	}
}

// TestWriteITKTransformFilesAsITKDoes: writing what was read gives ITK's
// files back byte for byte, text and MATLAB alike.
func TestWriteITKTransformFilesAsITKDoes(t *testing.T) {
	for _, name := range []string{"euler.tfm", "euler_zyx.tfm", "versor.tfm", "similarity.tfm", "affine.tfm", "composite.tfm"} {
		transform, want := readITKSample(t, name)
		var got bytes.Buffer
		if err := WriteITKText(&got, transform); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("%s written:\n%s\nITK wrote:\n%s", name, got.Bytes(), want)
		}
	}
	for _, name := range []string{"euler.mat", "affine.mat"} {
		transform, want := readITKSample(t, name)
		var got bytes.Buffer
		if err := WriteITKMatlab(&got, transform); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("%s: written bytes differ from ITK's", name)
		}
	}
}

// TestReadITKTransformFileVariants: Windows line ends, a byte order mark,
// FixedParameters before Parameters, ANTs' MatrixOffsetTransformBase and a
// big-endian MATLAB file read like the originals.
func TestReadITKTransformFileVariants(t *testing.T) {
	reference := itkReferencePoints(t)["affine"]
	check := func(name string, transform Transform, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, c := range reference {
			if got := transform.TransformPoint(c.in); got.Sub(c.out).Length() > 1e-9*math.Max(1, c.out.Length()) {
				t.Fatalf("%s: %v maps to %v, ITK %v", name, c.in, got, c.out)
			}
		}
	}
	_, text := readITKSample(t, "affine.tfm")
	crlf := bytes.ReplaceAll(text, []byte("\n"), []byte("\r\n"))
	transform, err := ReadTransformFile(append([]byte(utf8BOM), crlf...))
	check("CRLF with BOM", transform, err)

	lines := strings.Split(strings.TrimSpace(string(text)), "\n")
	swapped := strings.Join([]string{lines[0], lines[1], lines[2], lines[4], lines[3]}, "\n")
	transform, err = ReadTransformFile([]byte(swapped))
	check("fixed parameters first", transform, err)

	transform, err = ReadTransformFile([]byte(strings.Replace(string(text), "AffineTransform_double_3_3", "MatrixOffsetTransformBase_double_3_3", 1)))
	check("MatrixOffsetTransformBase", transform, err)

	_, mat := readITKSample(t, "affine.mat")
	transform, err = ReadTransformFile(bigEndianMatlab(t, mat))
	check("big-endian MATLAB", transform, err)
}

// bigEndianMatlab rewrites a little-endian, double-precision Level 4 file
// in big-endian byte order.
func bigEndianMatlab(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	for len(data) > 0 {
		var header [5]int32
		if err := binary.Read(bytes.NewReader(data[:20]), binary.LittleEndian, &header); err != nil {
			t.Fatal(err)
		}
		header[0] += matlabBigEndian
		_ = binary.Write(&out, binary.BigEndian, header)
		data = data[20:]
		out.Write(data[:header[4]])
		data = data[header[4]:]
		for i := int32(0); i < header[1]; i++ {
			_ = binary.Write(&out, binary.BigEndian, math.Float64frombits(binary.LittleEndian.Uint64(data)))
			data = data[8:]
		}
	}
	return out.Bytes()
}

func TestReadITKTransformFileRejects(t *testing.T) {
	_, text := readITKSample(t, "affine.tfm")
	_, mat := readITKSample(t, "affine.mat")
	for name, data := range map[string][]byte{
		"empty":            nil,
		"no header":        []byte("Transform: AffineTransform_double_3_3\n"),
		"other version":    []byte("#Insight Transform File V2.0\n"),
		"b-spline grid":    []byte(strings.Replace(string(text), "AffineTransform_double_3_3", "BSplineTransform_double_3_3", 1)),
		"two-dimensional":  []byte(strings.Replace(string(text), "AffineTransform_double_3_3", "AffineTransform_double_2_2", 1)),
		"missing fixed":    []byte(strings.Join(strings.Split(string(text), "\n")[:4], "\n")),
		"short parameters": []byte(strings.Replace(string(text), "Parameters: 1.1 ", "Parameters: ", 1)),
		"not a number":     []byte(strings.Replace(string(text), "-4 6 1", "-4 6 nan", 1)),
		"component file":   []byte(ITKTextHeader + "\n#Transform 0\nTransform: CompositeTransform_double_3_3\nComponentTransformFile: affine.tfm\n"),
		"late composite":   append(append([]byte{}, text...), []byte("Transform: CompositeTransform_double_3_3\n")...),
		"no transform":     []byte(ITKTextHeader + "\n"),
		"truncated MATLAB": mat[:len(mat)-3],
		"MATLAB matrix":    append([]byte{0, 0, 0, 0, 2, 0, 0, 0, 2, 0, 0, 0}, mat[12:]...),
	} {
		if _, err := ReadTransformFile(data); !errors.Is(err, ErrTransformFile) {
			t.Errorf("%s: %v", name, err)
		}
	}
	twice := append(append([]byte{}, text...), text[len(ITKTextHeader)+1:]...)
	if _, err := ReadTransformFile(twice); !errors.Is(err, ErrTransformFile) || !strings.Contains(err.Error(), "2 transforms") {
		t.Fatalf("two transforms without a composite: %v", err)
	}
	composite, _ := NewComposite(NewEuler3D(), NewAffine())
	if err := WriteITKMatlab(&bytes.Buffer{}, composite); !errors.Is(err, ErrTransformFile) {
		t.Fatalf("composite to MATLAB: %v", err)
	}
	nested, _ := NewComposite(composite, NewAffine())
	if err := WriteITKText(&bytes.Buffer{}, nested); !errors.Is(err, ErrTransformFile) {
		t.Fatalf("nested composite: %v", err)
	}
}

// TestITKReadsWrittenTransformFiles runs ITK on the files this package
// writes when ITK_TRANSFORM_TOOL names the itkgen tool of
// dicom-go-validation/scripts/itk_transform_files.
func TestITKReadsWrittenTransformFiles(t *testing.T) {
	tool := os.Getenv("ITK_TRANSFORM_TOOL")
	if tool == "" {
		t.Skip("ITK_TRANSFORM_TOOL not set")
	}
	dir := t.TempDir()
	reference := itkReferencePoints(t)
	var files []string
	want := map[string]string{}
	for _, sample := range []struct{ name, points string }{{"euler.tfm", "euler"}, {"versor.tfm", "versor"}, {"similarity.tfm", "similarity"}, {"affine.tfm", "affine"}, {"composite.tfm", "composite"}} {
		transform, _ := readITKSample(t, sample.name)
		for _, format := range []string{"tfm", "mat"} {
			if format == "mat" && transform.Kind() == KindComposite {
				continue
			}
			path := filepath.Join(dir, strings.TrimSuffix(sample.name, ".tfm")+"_go."+format)
			var b bytes.Buffer
			write := WriteITKText
			if format == "mat" {
				write = WriteITKMatlab
			}
			if err := write(&b, transform); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			files = append(files, path)
			want[filepath.ToSlash(path)] = sample.points
		}
	}
	out, err := exec.Command(tool, append([]string{"read"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", tool, err, out)
	}
	checked := 0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 7 || strings.HasPrefix(line, "#") {
			continue
		}
		points, ok := want[filepath.ToSlash(fields[0])]
		if !ok {
			t.Fatalf("unexpected output %q", line)
		}
		var v [6]float64
		for i := range v {
			v[i], _ = strconv.ParseFloat(fields[i+1], 64)
		}
		in, got := render.Vec3{X: v[0], Y: v[1], Z: v[2]}, render.Vec3{X: v[3], Y: v[4], Z: v[5]}
		for _, c := range reference[points] {
			if c.in == in && got.Sub(c.out).Length() > 1e-9*math.Max(1, c.out.Length()) {
				t.Fatalf("ITK maps %v through %s to %v, want %v", in, fields[0], got, c.out)
			}
		}
		checked++
	}
	if checked != 3*len(files) {
		t.Fatalf("ITK mapped %d points, want %d:\n%s", checked, 3*len(files), out)
	}
	t.Logf("ITK read %d files written by dicom-go:\n%s", len(files), out)
}
