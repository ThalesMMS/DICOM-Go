package surfaceseg

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestReferenceSurfaceNormalCoordinatesCountTowardReadBudget(t *testing.T) {
	for _, tc := range []struct {
		name          string
		retired       bool
		wireIndexSize int64
	}{
		{"long indices", false, 4},
		{"retired indices", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := primitiveDocument(t, "points", tc.retired)
			doc.Surfaces[0].Mesh.Normals = [][3]float32{{0, 0, 1}, {0, 0, 1}, {0, 0, 1}, {0, 0, 1}}
			file, err := Write(doc)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()

			// Four points and four normals have three float32 coordinates
			// each. The read budget charges the raw and decoded values plus
			// three wire indices and their reconstructed uint32 values.
			const coordinatesBytes = 2 * (4 + 4) * 3 * 4
			limit := int64(coordinatesBytes) + 3*(tc.wireIndexSize+4)
			atLimit, err := ReadWithLimits(file.Dataset, ReadLimits{MaxBytes: limit})
			if err != nil {
				t.Fatalf("ReadWithLimits(%d) error = %v", limit, err)
			}
			if !slices.Equal(atLimit.Surfaces[0].Mesh.Normals, doc.Surfaces[0].Mesh.Normals) {
				t.Fatalf("normals = %v, want %v", atLimit.Surfaces[0].Mesh.Normals, doc.Surfaces[0].Mesh.Normals)
			}
			below, err := ReadWithLimits(file.Dataset, ReadLimits{MaxBytes: limit - 1})
			if below != nil || !errors.Is(err, ErrResourceLimitExceeded) {
				t.Fatalf("ReadWithLimits(%d) = %v, %v; want nil and ErrResourceLimitExceeded", limit-1, below, err)
			}
		})
	}
}

func TestReferenceSurfaceInvalidPointAndNormalCoordinates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sequence core.Tag
		update   func(*object.Object)
	}{
		{"NaN point", tagSurfacePointsSequence, func(item *object.Object) {
			raw := make([]byte, 4*3*4)
			binary.LittleEndian.PutUint32(raw, math.Float32bits(float32(math.NaN())))
			item.Put(derivedio.Raw(tagPointCoordinatesData, core.VROF, raw))
		}},
		{"normal count differs from point count", tagSurfaceNormalsSequence, func(item *object.Object) {
			item.Put(derivedio.UL(tagNumberOfVectors, 3))
		}},
		{"non-three-dimensional normal", tagSurfaceNormalsSequence, func(item *object.Object) {
			item.Put(derivedio.US(tagVectorDimensionality, 2))
		}},
		{"normal coordinate payload is too short", tagSurfaceNormalsSequence, func(item *object.Object) {
			item.Put(derivedio.Raw(tagVectorCoordinateData, core.VROF, make([]byte, 4*3*3)))
		}},
		{"infinite normal", tagSurfaceNormalsSequence, func(item *object.Object) {
			raw := make([]byte, 4*3*4)
			binary.LittleEndian.PutUint32(raw, math.Float32bits(float32(math.Inf(1))))
			item.Put(derivedio.Raw(tagVectorCoordinateData, core.VROF, raw))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := primitiveDocument(t, "points", false)
			doc.Surfaces[0].Mesh.Normals = [][3]float32{{0, 0, 1}, {0, 0, 1}, {0, 0, 1}, {0, 0, 1}}
			file, err := Write(doc)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			changeFirstSurface(t, file.Dataset, func(surface *object.Object) {
				items, _ := surface.GetSequence(tc.sequence)
				if len(items) != 1 {
					t.Fatalf("fixture sequence %s has %d items, want 1", tc.sequence, len(items))
				}
				tc.update(items[0])
				surface.Put(derivedio.Seq(tc.sequence, items[0].ToDataSet()))
			})
			out, err := Read(file.Dataset)
			if out != nil || !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("Read() = %v, %v; want nil and ErrInvalidObject", out, err)
			}
		})
	}
}
