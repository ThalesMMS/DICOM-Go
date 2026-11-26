package metaimage_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/metaimage"
)

func TestReadVerifiesZlibPayload(t *testing.T) {
	voxels := []byte{1, 42, 255}
	compress := func(data []byte) []byte {
		t.Helper()
		var compressed bytes.Buffer
		zw := zlib.NewWriter(&compressed)
		if _, err := zw.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return compressed.Bytes()
	}
	intact := compress(voxels)
	corrupt := bytes.Clone(intact)
	corrupt[len(corrupt)-1] ^= 1
	header := `ObjectType = Image
NDims = 3
DimSize = 3 1 1
ElementType = MET_UCHAR
CompressedData = True
`

	for _, tc := range []struct {
		name      string
		data      []byte
		size      int
		wantError error
	}{
		{name: "valid", data: intact, size: len(intact)},
		{name: "valid_without_size", data: intact},
		{name: "corrupt_checksum", data: corrupt, size: len(corrupt), wantError: zlib.ErrChecksum},
		{name: "missing_trailer", data: intact[:len(intact)-4], size: len(intact), wantError: metaimage.ErrTruncatedData},
		{name: "trailer_beyond_limit", data: intact, size: len(intact) - 1, wantError: metaimage.ErrTruncatedData},
		{name: "extra_voxel", data: compress([]byte{1, 42, 255, 3}), wantError: metaimage.ErrMalformedHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := bytes.NewBufferString(header)
			if tc.size > 0 {
				fmt.Fprintf(wire, "CompressedDataSize = %d\n", tc.size)
			}
			wire.WriteString("ElementDataFile = LOCAL\n")
			wire.Write(tc.data)

			image, err := metaimage.Read(context.Background(), wire, metaimage.ReadOptions{})
			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) || image != nil {
					t.Fatalf("Read returned image present = %t, error = %v; want nil image and %v", image != nil, err, tc.wantError)
				}
				return
			}
			if err != nil || image == nil || image.Volume == nil {
				t.Fatalf("expected image volume from intact zlib stream, got error %v", err)
			}
			for i, want := range voxels {
				if got := image.Volume.Stored(i); got != float64(want) {
					t.Fatalf("voxel %d = %v, want %v", i, got, want)
				}
			}
		})
	}
}
