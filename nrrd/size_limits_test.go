package nrrd_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/nrrd"
)

func TestReadRejectsOverflowingSizes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dimensions string
	}{
		{name: "voxel_count", dimensions: "1073741824 1073741824 16"},
		{name: "payload_count", dimensions: "1073741824 1073741824 2"},
		{name: "signed_payload_count", dimensions: "1073741824 1073741824 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := "NRRD0005\ntype: double\ndimension: 3\nsizes: " + tc.dimensions + "\n" +
				"space: left-posterior-superior\nspace directions: (1,0,0) (0,1,0) (0,0,1)\n" +
				"encoding: text\n\n"
			options := nrrd.ReadOptions{Limits: nifti.ReadLimits{MaxVoxels: math.MaxUint64, MaxPayloadBytes: math.MaxUint64}}
			image, err := nrrd.Read(context.Background(), strings.NewReader(header), options)
			if image != nil || !errors.Is(err, nrrd.ErrLimitExceeded) {
				t.Fatalf("Read returned image present = %t, error = %v; want nil image and %v", image != nil, err, nrrd.ErrLimitExceeded)
			}
		})
	}
}
