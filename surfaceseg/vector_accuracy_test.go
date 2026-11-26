package surfaceseg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func surfaceAccuracyDocument(t *testing.T) *Document {
	t.Helper()
	doc := primitiveDocument(t, "points", false)
	doc.Surfaces[0].Mesh.Normals = [][3]float32{{0, 0, 1}, {0, 0, 1}, {0, 0, 1}, {0, 0, 1}}
	return doc
}

func TestReadRejectsMalformedSurfaceVectorAccuracy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accuracy core.Element
	}{
		{"two values", object.FLElement(tagVectorAccuracy, binary.LittleEndian, 0, 0.5)},
		{"one value per point", object.FLElement(tagVectorAccuracy, binary.LittleEndian, 0, 0.5, 0.25, 0.125)},
		{"negative deviation", object.FLElement(tagVectorAccuracy, binary.LittleEndian, -0.5)},
		{"NaN", object.FLElement(tagVectorAccuracy, binary.LittleEndian, float32(math.NaN()))},
		{"infinity", object.FLElement(tagVectorAccuracy, binary.LittleEndian, float32(math.Inf(1)))},
		{"wrong representation", derivedio.Raw(tagVectorAccuracy, core.VROF, []byte{0, 0, 0, 0})},
		{"partial float", derivedio.Raw(tagVectorAccuracy, core.VRFL, []byte{0, 0})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := Write(surfaceAccuracyDocument(t))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			changeFirstSurface(t, file.Dataset, func(surface *object.Object) {
				items, _ := surface.GetSequence(tagSurfaceNormalsSequence)
				items[0].Put(tc.accuracy)
				surface.Put(derivedio.Seq(tagSurfaceNormalsSequence, items[0].ToDataSet()))
			})
			got, err := Read(file.Dataset)
			if got != nil || !errors.Is(err, ErrInvalidObject) || !strings.Contains(err.Error(), "Vector Accuracy") {
				t.Fatalf("Read() returned document=%t, err=%v; want invalid Vector Accuracy and no document", got != nil, err)
			}
		})
	}
}

func TestReadRejectsMalformedSurfaceNormalsSequence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		normals func(core.DataSet) core.Element
	}{
		{"multiple items", func(item core.DataSet) core.Element {
			return derivedio.Seq(tagSurfaceNormalsSequence, item, item)
		}},
		{"nonsequence", func(core.DataSet) core.Element {
			return derivedio.Raw(tagSurfaceNormalsSequence, core.VROF, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := Write(surfaceAccuracyDocument(t))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			changeFirstSurface(t, file.Dataset, func(surface *object.Object) {
				items, _ := surface.GetSequence(tagSurfaceNormalsSequence)
				surface.Put(tc.normals(items[0].ToDataSet()))
			})
			got, err := Read(file.Dataset)
			if got != nil || !errors.Is(err, ErrInvalidObject) || !strings.Contains(err.Error(), "Surface Points Normals Sequence") {
				t.Fatalf("Read() returned document=%t, err=%v; want invalid normals sequence and no document", got != nil, err)
			}
		})
	}
}

func TestSurfaceVectorAccuracyRoundTripAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []float32
	}{
		{"omitted", nil},
		{"single zero standard deviation", []float32{0}},
		{"three XYZ standard deviations", []float32{0.125, 1.25, 0.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := surfaceAccuracyDocument(t)
			doc.Surfaces[0].VectorAccuracy = tc.values
			got, file := roundTrip(t, doc)
			defer file.Close()
			if !slices.Equal(got.Surfaces[0].VectorAccuracy, tc.values) ||
				!slices.Equal(got.Surfaces[0].Mesh.Normals, doc.Surfaces[0].Mesh.Normals) {
				t.Fatalf("Vector Accuracy = %v, normals = %v", got.Surfaces[0].VectorAccuracy, got.Surfaces[0].Mesh.Normals)
			}
			surface := derivedio.Sequence(file.Dataset, tagSurfaceSequence)[0]
			normals := derivedio.Sequence(surface, tagSurfaceNormalsSequence)[0]
			accuracy, present := normals.Get(tagVectorAccuracy)
			if present != (len(tc.values) > 0) || present && accuracy.VR() != core.VRFL {
				t.Fatalf("Vector Accuracy present=%t, VR=%s", present, accuracy.VR())
			}

			// Charge raw and decoded coordinates for four points and normals,
			// three 32-bit indices, and raw and decoded standard deviations.
			limit := int64((4+4)*3*8 + 3*8 + len(tc.values)*8)
			atLimit, err := ReadWithLimits(file.Dataset, ReadLimits{MaxBytes: limit})
			if err != nil || atLimit == nil || !slices.Equal(atLimit.Surfaces[0].VectorAccuracy, tc.values) {
				t.Fatalf("ReadWithLimits(%d) returned document=%t, err=%v", limit, atLimit != nil, err)
			}
			below, err := ReadWithLimits(file.Dataset, ReadLimits{MaxBytes: limit - 1})
			if below != nil || !errors.Is(err, ErrResourceLimitExceeded) {
				t.Fatalf("ReadWithLimits(%d) returned document=%t, err=%v", limit-1, below != nil, err)
			}
		})
	}
}

func TestReadSurfaceVectorAccuracyEmptyFL(t *testing.T) {
	file, err := Write(surfaceAccuracyDocument(t))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	changeFirstSurface(t, file.Dataset, func(surface *object.Object) {
		items, _ := surface.GetSequence(tagSurfaceNormalsSequence)
		items[0].Put(derivedio.Raw(tagVectorAccuracy, core.VRFL, nil))
		surface.SetSequence(tagSurfaceNormalsSequence, items...)
	})
	got, err := Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if got.Surfaces[0].VectorAccuracy != nil || len(got.Surfaces[0].Mesh.Normals) != 4 {
		t.Fatalf("accuracy=%v normals=%v", got.Surfaces[0].VectorAccuracy, got.Surfaces[0].Mesh.Normals)
	}
}

func TestReadSurfaceVectorAccuracyImplicitVR(t *testing.T) {
	doc := surfaceAccuracyDocument(t)
	doc.Surfaces[0].VectorAccuracy = []float32{0.125, 0.25, 0.5}
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var encoded bytes.Buffer
	if err := object.WriteDataSet(&encoded, file.Dataset, transfer.ImplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	dataset, err := object.ReadDataSet(&encoded, transfer.ImplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	read, err := Read(dataset)
	if err != nil {
		t.Fatal(err)
	}
	if got := read.Surfaces[0].VectorAccuracy; !slices.Equal(got, doc.Surfaces[0].VectorAccuracy) {
		t.Fatalf("Vector Accuracy = %v, want %v", got, doc.Surfaces[0].VectorAccuracy)
	}
}

func TestWriteRejectsInvalidSurfaceVectorAccuracy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		values    []float32
		noNormals bool
	}{
		{"no normals", []float32{0.25}, true},
		{"two values", []float32{0, 1}, false},
		{"four values", []float32{0, 1, 2, 3}, false},
		{"negative", []float32{-0.5}, false},
		{"NaN", []float32{float32(math.NaN())}, false},
		{"infinity", []float32{0, 1, float32(math.Inf(1))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := surfaceAccuracyDocument(t)
			doc.Surfaces[0].VectorAccuracy = tc.values
			if tc.noNormals {
				doc.Surfaces[0].Mesh.Normals = nil
			}
			file, err := Write(doc)
			if file != nil || !errors.Is(err, ErrInvalidObject) || !strings.Contains(err.Error(), "Vector Accuracy") {
				t.Fatalf("Write() returned file=%t, err=%v; want invalid Vector Accuracy and no file", file != nil, err)
			}
		})
	}
}
