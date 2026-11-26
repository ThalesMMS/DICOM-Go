package metaimage_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/metaimage"
)

func TestReferenceAcceptanceMetaImageRejectsInvalidZlibTrailer(t *testing.T) {
	voxels := []byte{1, 42, 255}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(voxels); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	intact := compressed.Bytes()
	corrupt := bytes.Clone(intact)
	corrupt[len(corrupt)-1] ^= 1
	header := fmt.Sprintf(`ObjectType = Image
NDims = 3
DimSize = 3 1 1
ElementType = MET_UCHAR
CompressedData = True
CompressedDataSize = %d
ElementDataFile = LOCAL
`, len(intact))

	for _, tc := range []struct {
		name          string
		data          []byte
		wantZlibError error
	}{
		{name: "valid", data: intact},
		{name: "corrupt_checksum", data: corrupt, wantZlibError: zlib.ErrChecksum},
		{name: "missing_trailer", data: intact[:len(intact)-4], wantZlibError: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zr, err := zlib.NewReader(bytes.NewReader(tc.data))
			if err != nil {
				t.Fatalf("fixture zlib header: %v", err)
			}
			defer zr.Close()
			decoded, zlibErr := io.ReadAll(zr)
			if !bytes.Equal(decoded, voxels) || !errors.Is(zlibErr, tc.wantZlibError) {
				t.Fatalf("fixture decoded to %v with error %v, want %v and %v", decoded, zlibErr, voxels, tc.wantZlibError)
			}

			input := io.MultiReader(bytes.NewBufferString(header), bytes.NewReader(tc.data))
			image, err := metaimage.Read(context.Background(), input, metaimage.ReadOptions{})
			if tc.wantZlibError != nil {
				if err == nil || image != nil {
					t.Fatalf("Read returned image present = %t, error = %v, want rejection of %v without an image", image != nil, err, tc.wantZlibError)
				}
				return
			}
			if err != nil || image == nil || image.Volume == nil {
				t.Fatalf("expected image volume from intact zlib stream, got error %v", err)
			}
			checkMetaImageReferenceValues(t, image, []float64{1, 42, 255})
		})
	}
}
