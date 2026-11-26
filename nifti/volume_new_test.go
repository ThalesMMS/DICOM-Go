package nifti_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

// TestNewVolume builds volumes as the NRRD and MetaImage readers do (#1078).
func TestNewVolume(t *testing.T) {
	leftHanded := render.GeometryAffine{0.5, 0, 0, -10, 0, 0.75, 0, 20, 0, 0, -2, 5, 0, 0, 0, 1}
	payload := []byte{1, 0, 2, 0, 3, 0, 4, 0, 99}
	volume, err := nifti.NewVolume(context.Background(), bytes.NewReader(payload), nifti.NewVolumeOptions{
		Datatype: nifti.DatatypeUint16, Dimensions: [4]int{2, 2}, IndexToLPS: leftHanded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if volume.Dimensions != [4]int{2, 2, 1, 1} || volume.Stored(3) != 4 || volume.BytesPerVoxel() != 2 {
		t.Fatalf("volume %v %v", volume.Dimensions, volume.Stored(3))
	}
	if volume.Geometry.IndexToLPS != leftHanded || !volume.Geometry.LeftHanded || volume.Geometry.Spacing != [3]float64{0.5, 0.75, 2} {
		t.Fatalf("geometry %+v", volume.Geometry)
	}
	if len(volume.Warnings) != 1 || volume.Warnings[0] != nifti.WarningTrailingData {
		t.Fatalf("warnings %v", volume.Warnings)
	}
	var written bytes.Buffer
	if err := volume.WritePayload(&written); err != nil || !bytes.Equal(written.Bytes(), payload[:8]) {
		t.Fatalf("payload %v %v", written.Bytes(), err)
	}
	big, err := nifti.NewVolume(context.Background(), bytes.NewReader([]byte{0, 5}), nifti.NewVolumeOptions{Datatype: nifti.DatatypeInt16, Dimensions: [4]int{1, 1, 1, 1}, IndexToLPS: leftHanded, BigEndian: true})
	if err != nil || big.Stored(0) != 5 {
		t.Fatalf("big-endian %v %v", big, err)
	}
	sheared := leftHanded
	sheared[1] = 0.3
	for name, tc := range map[string]struct {
		options nifti.NewVolumeOptions
		data    []byte
		err     error
	}{
		"datatype":  {nifti.NewVolumeOptions{Datatype: 128, IndexToLPS: leftHanded}, nil, nifti.ErrUnsupportedDatatype},
		"sheared":   {nifti.NewVolumeOptions{Datatype: nifti.DatatypeUint8, IndexToLPS: sheared}, []byte{1}, nifti.ErrNonOrthogonalSForm},
		"truncated": {nifti.NewVolumeOptions{Datatype: nifti.DatatypeUint8, Dimensions: [4]int{2, 1, 1, 1}, IndexToLPS: leftHanded}, []byte{1}, nifti.ErrTruncatedData},
		"limit":     {nifti.NewVolumeOptions{Datatype: nifti.DatatypeUint8, Dimensions: [4]int{100, 100, 100, 1}, IndexToLPS: leftHanded, Limits: nifti.ReadLimits{MaxVoxels: 1000}}, nil, nifti.ErrLimitExceeded},
	} {
		if _, err := nifti.NewVolume(context.Background(), bytes.NewReader(tc.data), tc.options); !errors.Is(err, tc.err) {
			t.Errorf("%s: %v, want %v", name, err, tc.err)
		}
	}
}
