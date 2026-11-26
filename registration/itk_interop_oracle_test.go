package registration

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

type itk1345Descriptor struct {
	Class      string              `json:"class"`
	Dimension  int                 `json:"dimension"`
	Parameters []float64           `json:"parameters"`
	Fixed      []float64           `json:"fixed"`
	Matrix     []float64           `json:"matrix"`
	Components []itk1345Descriptor `json:"components"`
}
type itk1345Point struct{ Input, Output []float64 }
type itk1345File struct {
	Base64 string `json:"base64"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type itk1345Case struct {
	itk1345Descriptor
	Name   string                 `json:"name"`
	Points []itk1345Point         `json:"points"`
	Files  map[string]itk1345File `json:"files"`
}

func itk1345Fixtures(t *testing.T) []itk1345Case {
	t.Helper()
	b, err := os.ReadFile("testdata/itk1345/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 8<<20 {
		t.Fatal("fixture size admission")
	}
	var fixture struct {
		Provenance struct {
			Versions struct {
				SimpleITK string `json:"simpleitk"`
				ITK       string `json:"itk"`
			}
			NotImplementation bool `json:"not_implementation_source"`
		} `json:"provenance"`
		Cases []itk1345Case `json:"cases"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Provenance.Versions.SimpleITK != "2.5.3" || fixture.Provenance.Versions.ITK != "5.4" || !fixture.Provenance.NotImplementation || len(fixture.Cases) != 25 {
		t.Fatal("oracle provenance/count")
	}
	return fixture.Cases
}
func itk1345CompareVector(t *testing.T, label string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s length%d want%d", label, len(got), len(want))
	}
	for i, v := range want {
		if !finite(got[i]) || math.Abs(got[i]-v) > 2e-11*math.Max(1, math.Abs(v)) {
			t.Fatalf("%s[%d] got%.17g want%.17g", label, i, got[i], v)
		}
	}
}
func itk1345CompareMetadata(t *testing.T, tr itkParameters, d itk1345Descriptor) {
	t.Helper()
	if len(d.Components) > 0 {
		switch c := tr.(type) {
		case *Composite:
			if c.Len() != len(d.Components) {
				t.Fatal("composite component count")
			}
			for i, kid := range d.Components {
				itk1345CompareMetadata(t, c.Transform(c.Len()-1-i), kid)
			}
		case *Composite2D:
			if c.Len() != len(d.Components) {
				t.Fatal("2D composite component count")
			}
			for i, kid := range d.Components {
				itk1345CompareMetadata(t, c.Transform(c.Len()-1-i), kid)
			}
		default:
			t.Fatalf("expected composite got%T", tr)
		}
		return
	}
	itk1345CompareVector(t, "parameters", tr.Parameters(), d.Parameters)
	itk1345CompareVector(t, "fixed", tr.FixedParameters(), d.Fixed)
	if len(d.Matrix) > 0 {
		var matrix []float64
		switch m := tr.(type) {
		case MatrixTransform:
			a := m.Matrix()
			for _, row := range a {
				matrix = append(matrix, row[:]...)
			}
		case MatrixTransform2D:
			a := m.Matrix()
			for _, row := range a {
				matrix = append(matrix, row[:]...)
			}
		}
		itk1345CompareVector(t, "matrix", matrix, d.Matrix)
	}
}
func itk1345ComparePoints(t *testing.T, tr itkParameters, c itk1345Case) {
	t.Helper()
	itk1345CompareMetadata(t, tr, c.itk1345Descriptor)
	for i, p := range c.Points {
		var got []float64
		switch v := tr.(type) {
		case Transform:
			q := v.TransformPoint(render.Vec3{X: p.Input[0], Y: p.Input[1], Z: p.Input[2]})
			got = []float64{q.X, q.Y, q.Z}
		case Transform2D:
			q := v.TransformPoint(Point2{p.Input[0], p.Input[1]})
			got = q[:]
		}
		itk1345CompareVector(t, fmt.Sprintf("point%d", i), got, p.Output)
	}
}
func itk1345Read(t *testing.T, dimension int, format string, b []byte) (itkParameters, error) {
	t.Helper()
	r := bytes.NewReader(b)
	if dimension == 2 {
		var ts []Transform2D
		var err error
		switch format {
		case "h5":
			ts, err = ReadITKHDF52D(r, int64(len(b)))
		case "tfm":
			ts, err = ReadITKText2D(r)
		case "mat":
			ts, err = ReadITKMatlab2D(r)
		default:
			t.Fatal("unknown format")
		}
		if err != nil {
			return nil, err
		}
		if len(ts) != 1 {
			t.Fatal("typed2D record count", len(ts))
		}
		return ts[0], nil
	}
	var ts []Transform
	var err error
	switch format {
	case "h5":
		ts, err = ReadITKHDF5(r, int64(len(b)))
	case "tfm":
		ts, err = ReadITKText(r)
	case "mat":
		ts, err = ReadITKMatlab(r)
	default:
		t.Fatal("unknown format")
	}
	if err != nil {
		return nil, err
	}
	if len(ts) != 1 {
		t.Fatal("3D record count", len(ts))
	}
	return ts[0], nil
}
func itk1345Create(t *testing.T, d itk1345Descriptor) itkParameters {
	t.Helper()
	if len(d.Components) > 0 {
		if d.Dimension == 2 {
			var ts []Transform2D
			for i := len(d.Components) - 1; i >= 0; i-- {
				ts = append(ts, itk1345Create(t, d.Components[i]).(Transform2D))
			}
			c, err := NewComposite2D(ts...)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		var ts []Transform
		for i := len(d.Components) - 1; i >= 0; i-- {
			ts = append(ts, itk1345Create(t, d.Components[i]).(Transform))
		}
		c, err := NewComposite(ts...)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	name := fmt.Sprintf("%s_double_%d_%d", d.Class, d.Dimension, d.Dimension)
	if d.Class == "BSplineTransform" {
		name = "BSplineTransform_double_3_3"
	}
	tr, err := newITKParameters(name, d.Dimension)
	if err != nil {
		t.Fatal(err)
	}
	if err := setITKParameters(tr, d.Parameters, d.Fixed); err != nil {
		t.Fatal(err)
	}
	return tr
}
func TestITK1345ReadPublicInterchange(t *testing.T) {
	for _, c := range itk1345Fixtures(t) {
		for format, f := range c.Files {
			t.Run(c.Name+"/"+format, func(t *testing.T) {
				b, err := base64.StdEncoding.DecodeString(f.Base64)
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(b)
				if len(b) != f.Bytes || hex.EncodeToString(hash[:]) != f.SHA256 {
					t.Fatal("public fixture byte provenance")
				}
				tr, err := itk1345Read(t, c.Dimension, format, b)
				if err != nil {
					t.Fatal(err)
				}
				itk1345ComparePoints(t, tr, c)
			})
		}
	}
}
func TestITK1345ExportInterchange(t *testing.T) {
	output := os.Getenv("ITK1345_OUTPUT_DIR")
	if output != "" {
		if err := os.MkdirAll(output, 0755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := []map[string]any{}
	for _, c := range itk1345Fixtures(t) {
		tr := itk1345Create(t, c.itk1345Descriptor)
		itk1345ComparePoints(t, tr, c)
		for format := range c.Files {
			t.Run(c.Name+"/"+format, func(t *testing.T) {
				var b bytes.Buffer
				var err error
				if c.Dimension == 2 {
					v := tr.(Transform2D)
					switch format {
					case "h5":
						err = WriteITKHDF52D(&b, v)
					case "tfm":
						err = WriteITKText2D(&b, v)
					case "mat":
						err = WriteITKMatlab2D(&b, v)
					}
				} else {
					v := tr.(Transform)
					switch format {
					case "h5":
						err = WriteITKHDF5(&b, v)
					case "tfm":
						err = WriteITKText(&b, v)
					case "mat":
						err = WriteITKMatlab(&b, v)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				actual, err := itk1345Read(t, c.Dimension, format, b.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				itk1345ComparePoints(t, actual, c)
				if output != "" {
					name := c.Name + "." + format
					if err := os.WriteFile(filepath.Join(output, name), b.Bytes(), 0644); err != nil {
						t.Fatal(err)
					}
					hash := sha256.Sum256(b.Bytes())
					manifest = append(manifest, map[string]any{"name": c.Name, "file": name, "format": format, "dimension": c.Dimension, "bytes": b.Len(), "sha256": hex.EncodeToString(hash[:])})
				}
			})
		}
	}
	if output != "" {
		b, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(b, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestITK1345ReadPublicRewrites(t *testing.T) {
	// The committed public fixtures above always run. A qualification campaign
	// can additionally supply real ITK rewrites; absence is not a skipped test.
	input := os.Getenv("ITK1345_INPUT_DIR")
	if input == "" {
		return
	}
	for _, c := range itk1345Fixtures(t) {
		for format := range c.Files {
			t.Run(c.Name+"/"+format, func(t *testing.T) {
				b, err := os.ReadFile(filepath.Join(input, c.Name+"."+format))
				if err != nil {
					t.Fatal(err)
				}
				tr, err := itk1345Read(t, c.Dimension, format, b)
				if err != nil {
					t.Fatal(err)
				}
				itk1345ComparePoints(t, tr, c)
			})
		}
	}
}
