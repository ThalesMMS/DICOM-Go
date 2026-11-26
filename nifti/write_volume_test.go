package nifti_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

// TestWriteVolumeRoundTrip: a volume written by WriteVolume reads back with
// the same grid, geometry, datatype and voxels, plain or gzip-compressed.
func TestWriteVolumeRoundTrip(t *testing.T) {
	// An oblique, anisotropic grid: i along (0.6, 0.8, 0) * 0.5 mm, j along
	// (-0.8, 0.6, 0) * 0.75 mm, k along z * 2.5 mm.
	affine := render.GeometryAffine{
		0.3, -0.6, 0, -20,
		0.4, 0.45, 0, 30,
		0, 0, 2.5, 10,
		0, 0, 0, 1,
	}
	dims := [4]int{4, 3, 2, 1}
	payload := make([]byte, 0, 2*24)
	for v := 0; v < 24; v++ {
		payload = binary.LittleEndian.AppendUint16(payload, uint16(v*1000))
	}
	volume, err := nifti.NewVolume(context.Background(), bytes.NewReader(payload), nifti.NewVolumeOptions{Datatype: nifti.DatatypeUint16, Dimensions: dims, IndexToLPS: affine})
	if err != nil {
		t.Fatal(err)
	}
	for _, compression := range []nifti.Compression{nifti.CompressionNone, nifti.CompressionGZIP} {
		var buf bytes.Buffer
		if err := nifti.WriteVolume(&buf, volume, compression); err != nil {
			t.Fatal(err)
		}
		if gz := bytes.HasPrefix(buf.Bytes(), []byte{0x1f, 0x8b}); gz != (compression == nifti.CompressionGZIP) {
			t.Fatalf("%v: gzip header %v", compression, gz)
		}
		got, err := nifti.Read(context.Background(), &buf, nifti.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Dimensions != dims || got.Header.Datatype != nifti.DatatypeUint16 {
			t.Fatalf("%v: dims %v datatype %d", compression, got.Dimensions, got.Header.Datatype)
		}
		for i := range affine {
			if math.Abs(got.Geometry.IndexToLPS[i]-affine[i]) > 1e-5 {
				t.Fatalf("%v: IndexToLPS %v, want %v", compression, got.Geometry.IndexToLPS, affine)
			}
		}
		for v := 0; v < 24; v++ {
			if got.Value(v) != float64(v*1000) {
				t.Fatalf("%v: voxel %d = %v", compression, v, got.Value(v))
			}
		}
	}
	if err := nifti.WriteVolume(&bytes.Buffer{}, nil, nifti.CompressionNone); !errors.Is(err, nifti.ErrInvalidSource) {
		t.Fatalf("nil volume: %v", err)
	}
}

// TestSEGReferences: the planes of a classic series carry their geometry
// and identifiers.
func TestSEGReferences(t *testing.T) {
	geometry := imagebuild.Geometry{Rows: 3, Columns: 4, PixelSpacing: [2]float64{0.75, 0.5},
		RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 2.5}
	slices := make([]imagebuild.Slice, 2)
	for z := range slices {
		slices[z] = imagebuild.Slice{Values: make([]float64, 12), Position: [3]float64{-20, 30, 10 + 2.5*float64(z)}}
	}
	files, err := imagebuild.Build(geometry, slices, imagebuild.Options{IOD: imagebuild.CT, ContentTime: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	references, err := nifti.SEGReferences(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 2 {
		t.Fatalf("%d references", len(references))
	}
	for z, reference := range references {
		g := reference.Geometry
		if g.Rows != 3 || g.Columns != 4 || g.RowSpacing != 0.75 || g.ColSpacing != 0.5 || g.Origin.Z != 10+2.5*float64(z) || g.Normal.Z != 1 {
			t.Fatalf("slice %d geometry %+v", z, g)
		}
		uid, _ := files[z].GetUID(core.NewTag(0x0008, 0x0018))
		if reference.Image.SOPInstanceUID != uid || reference.Image.SeriesInstanceUID == "" || reference.Image.SOPClassUID != "1.2.840.10008.5.1.4.1.1.2" || reference.Image.Frames != nil {
			t.Fatalf("slice %d image %+v", z, reference.Image)
		}
	}
	files[1].Dataset.Remove(core.NewTag(0x0020, 0x0032))
	if _, err := nifti.SEGReferences(files); !errors.Is(err, nifti.ErrInvalidGeometry) {
		t.Fatalf("missing position: %v", err)
	}
}
