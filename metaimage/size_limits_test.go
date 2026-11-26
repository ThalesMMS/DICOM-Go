package metaimage_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/metaimage"
	"github.com/ThalesMMS/dicom-go/nifti"
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
			header := "NDims = 3\nDimSize = " + tc.dimensions + "\nElementType = MET_DOUBLE\n" +
				"CompressedData = True\nElementDataFile = LOCAL\n"
			options := metaimage.ReadOptions{Limits: nifti.ReadLimits{MaxVoxels: math.MaxUint64, MaxPayloadBytes: math.MaxUint64}}
			image, err := metaimage.Read(context.Background(), strings.NewReader(header), options)
			if image != nil || !errors.Is(err, metaimage.ErrLimitExceeded) {
				t.Fatalf("Read returned image present = %t, error = %v; want nil image and %v", image != nil, err, metaimage.ErrLimitExceeded)
			}
		})
	}
}
