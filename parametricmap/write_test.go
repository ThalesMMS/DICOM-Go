package parametricmap_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/fitmap"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parametricmap"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

const sourceSOPClass = "1.2.840.10008.5.1.4.1.1.4" // MR Image Storage

func TestLocalTexturePMAPSourceFramesRoundTrip(t *testing.T) {
	doc, _ := t2MapDocument(t)
	doc.Frames[0].Sources[0].Frames = []int{2, 4}
	file, err := parametricmap.Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	m, err := parametricmap.Read(file)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range m.References {
		if ref.SOPInstanceUID == doc.Frames[0].Sources[0].SOPInstanceUID && len(ref.Frames) == 2 && ref.Frames[0] == 2 && ref.Frames[1] == 4 {
			found = true
		}
	}
	if !found {
		t.Fatal("selected source frames discarded", m.References)
	}
	doc.Frames[0].Sources[0].Frames = []int{0}
	if _, err := parametricmap.Write(doc); err == nil {
		t.Fatal("zero referenced frame accepted")
	}
}

// t2MapDocument fits a synthetic two-slice, four-echo T2 phantom (T2 = 25
// ms + column, background outside a disc) and describes it as a map.
func t2MapDocument(t *testing.T) (*parametricmap.Document, []float64) {
	t.Helper()
	const rows, columns, slices = 6, 8, 2
	echoes := []float64{10, 20, 40, 80}
	images := make([][]float32, len(echoes))
	var truth []float64
	for k, te := range echoes {
		images[k] = make([]float32, rows*columns*slices)
		for i := range images[k] {
			x, y := i%columns, (i/columns)%rows
			if (x-4)*(x-4)+(y-3)*(y-3) > 9 {
				continue
			}
			images[k][i] = float32(1000 * math.Exp(-te/(25+float64(x))))
		}
	}
	result, err := fitmap.Fit(context.Background(), fitmap.T2, echoes, images, fitmap.Options{Threshold: 1})
	if err != nil {
		t.Fatal(err)
	}
	truth = result.Values
	quantity, units := fitmap.T2.Quantity(), fitmap.T2.Units()
	doc := &parametricmap.Document{
		SOPInstanceUID: "2.25.1149.1", StudyInstanceUID: "2.25.1149.2", SeriesInstanceUID: "2.25.1149.3",
		FrameOfReferenceUID: "2.25.1149.4", SourceSeriesInstanceUID: "2.25.1149.5",
		SeriesDescription: "T2 map", Rows: rows, Columns: columns,
		Quantity:             parametricmap.Code{Value: quantity.Value, Scheme: quantity.Scheme, Meaning: quantity.Meaning},
		Units:                parametricmap.Code{Value: units.Value, Scheme: units.Scheme, Meaning: units.Meaning},
		DerivedPixelContrast: "T2_MAP", DerivationDescription: "Log-linear mono-exponential fit over 4 echoes",
		ContentDate: "20261007", ContentTime: "120000",
	}
	for z := 0; z < slices; z++ {
		values := make([]float32, rows*columns)
		for i := range values {
			values[i] = float32(result.Values[z*rows*columns+i])
		}
		doc.Frames = append(doc.Frames, parametricmap.DocumentFrame{
			Geometry: render.SliceGeometry{
				Origin: render.Vec3{X: -10, Y: 5, Z: 3 * float64(z)},
				RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, Normal: render.Vec3{Z: 1},
				RowSpacing: 0.9, ColSpacing: 0.8, Rows: rows, Columns: columns,
			},
			Values:  values,
			Sources: []parametricmap.Reference{{SOPClassUID: sourceSOPClass, SOPInstanceUID: "2.25.1149.10." + string(rune('1'+z))}},
		})
	}
	return doc, truth
}

// TestWrittenMapReadsBack: a fitted T2 map written as a Parametric Map
// reads back through the library with its values, geometry, quantity,
// units and source references, and meets the Parametric Map IOD.
func TestWrittenMapReadsBack(t *testing.T) {
	doc, truth := t2MapDocument(t)
	file, err := parametricmap.Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	m, err := parametricmap.Read(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if m.Payload != parametricmap.PayloadFloat32 || m.NumberOfFrames != 2 || m.Rows != 6 || m.Columns != 8 ||
		m.Quantity.Value != "113065" || m.Units.Value != "ms" || m.FrameOfReferenceUID != "2.25.1149.4" {
		t.Fatalf("map %+v", m)
	}
	if len(m.References) != 2 || m.References[0].SOPClassUID != sourceSOPClass {
		t.Fatalf("references %+v", m.References)
	}
	measured := 0
	for frame := 0; frame < 2; frame++ {
		values, err := m.FrameValues(frame)
		if err != nil {
			t.Fatal(err)
		}
		g := m.Frames[frame].Geometry
		if g.Origin.Z != 3*float64(frame) || g.RowSpacing != 0.9 || g.ColSpacing != 0.8 || m.Frames[frame].DimensionIndexValues[0] != frame+1 {
			t.Fatalf("frame %d geometry %+v, dimensions %v", frame, g, m.Frames[frame].DimensionIndexValues)
		}
		for i, v := range values {
			want := float64(float32(truth[frame*48+i]))
			if math.IsNaN(want) {
				if parametricmap.Measured(v) {
					t.Fatalf("frame %d pixel %d: %v where the fit had no value", frame, i, v)
				}
				continue
			}
			measured++
			if v != want || math.Abs(v-(25+float64(i%8))) > 1e-3 {
				t.Fatalf("frame %d pixel %d: %v, want %v", frame, i, v, want)
			}
		}
	}
	if measured == 0 {
		t.Fatal("no measured pixel")
	}
	if probe, ok, err := m.SamplePatient(render.Vec3{X: -10 + 4*0.8, Y: 5 + 3*0.9, Z: 3}); err != nil || !ok || math.Abs(probe-29) > 1e-3 {
		t.Fatalf("probe %v %v %v, want 29 ms", probe, ok, err)
	}

	report, err := iod.Validate(parsed.Dataset, iod.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.IOD != "Parametric Map" {
		t.Fatalf("IOD %q", report.IOD)
	}
	for _, finding := range report.Errors() {
		t.Errorf("IOD: %s", finding)
	}

}

func TestWriteRejectsIncompleteDocuments(t *testing.T) {
	for name, mutate := range map[string]func(*parametricmap.Document){
		"nil frames":   func(d *parametricmap.Document) { d.Frames = nil },
		"no frame UID": func(d *parametricmap.Document) { d.FrameOfReferenceUID = "" },
		"no quantity":  func(d *parametricmap.Document) { d.Quantity = parametricmap.Code{} },
		"short frame":  func(d *parametricmap.Document) { d.Frames[1].Values = d.Frames[1].Values[:3] },
		"bad geometry": func(d *parametricmap.Document) { d.Frames[0].Geometry.RowSpacing = 0 },
		"oblique frame": func(d *parametricmap.Document) {
			d.Frames[1].Geometry.RowDir = render.Vec3{Y: 1}
			d.Frames[1].Geometry.ColDir = render.Vec3{X: 1}
		},
		"source series":   func(d *parametricmap.Document) { d.SourceSeriesInstanceUID = "" },
		"anonymous image": func(d *parametricmap.Document) { d.Frames[0].Sources[0].SOPInstanceUID = "" },
	} {
		doc, _ := t2MapDocument(t)
		mutate(doc)
		if _, err := parametricmap.Write(doc); err == nil {
			t.Errorf("%s: written", name)
		} else if !errors.Is(err, parametricmap.ErrInvalidObject) && !errors.Is(err, parametricmap.ErrMissingMapping) &&
			!errors.Is(err, parametricmap.ErrGeometryMismatch) && !errors.Is(err, parametricmap.ErrUnsupportedDimensions) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := parametricmap.Write(nil); !errors.Is(err, parametricmap.ErrInvalidObject) {
		t.Errorf("nil document: %v", err)
	}
}
