package metaimage_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/metaimage"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

// versionHeader is a 2×2×1 MET_SHORT header whose version-specific lines are
// filled in by each case.
const versionHeader = `ObjectType = Image
NDims = 3
%s
BinaryData = True
BinaryDataByteOrderMSB = False
CompressedData = False
ElementSpacing = 0.5 0.75 2
DimSize = 2 2 1
ElementType = MET_SHORT
ElementDataFile = LOCAL
`

func readVersionFile(t *testing.T, geometry string) (*metaimage.Image, error) {
	t.Helper()
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.LittleEndian, []int16{5, -6, 700, -800})
	file := strings.Replace(versionHeader, "%s", geometry, 1) + payload.String()
	return metaimage.Read(context.Background(), strings.NewReader(file), metaimage.ReadOptions{})
}

// TestFileFormatVersion1ElementGeometry reads the image geometry of a
// MetaIO FileFormatVersion 1 file from ElementOrigin and ElementDirection,
// as ITK's MetaImageIO does (MetaIO dc60586892, ITK dfd2d42f56).
func TestFileFormatVersion1ElementGeometry(t *testing.T) {
	const element = "ElementOrigin = -10 20 5\nElementDirection = 0 1 0 -1 0 0 0 0 1"
	// ElementDirection row i is axis i: i runs along +y (0.5 mm), j along -x
	// (0.75 mm), from (-10, 20, 5).
	elementAffine := render.GeometryAffine{
		0, -0.75, 0, -10,
		0.5, 0, 0, 20,
		0, 0, 2, 5,
		0, 0, 0, 1,
	}
	identityAffine := render.GeometryAffine{
		0.5, 0, 0, 0,
		0, 0.75, 0, 0,
		0, 0, 2, 0,
		0, 0, 0, 1,
	}
	for _, tc := range []struct {
		name     string
		geometry string
		affine   render.GeometryAffine
		warnings []string
	}{
		{"version 1 identity object transform",
			"FileFormatVersion = 1\nTransformMatrix = 1 0 0 0 1 0 0 0 1\nOffset = 0 0 0\nCenterOfRotation = 0 0 0\n" + element,
			elementAffine, nil},
		{"version 1 without object transform", "FileFormatVersion = 1\n" + element, elementAffine, nil},
		// ITK reads the element geometry and drops the object-to-parent
		// transform; it is reported, not applied.
		{"version 1 non-identity object transform",
			"FileFormatVersion = 1\nTransformMatrix = 0 0 1 0 1 0 -1 0 0\nOffset = 7 8 9\n" + element,
			elementAffine, []string{metaimage.WarningObjectTransformIgnored}},
		{"version 1 object offset only",
			"FileFormatVersion = 1\nOffset = 7 8 9\n" + element,
			elementAffine, []string{metaimage.WarningObjectTransformIgnored}},
		// Without element fields ITK falls back to Offset and
		// TransformMatrix; the version 0 aliases are not read.
		{"version 1 falls back to Offset and TransformMatrix",
			"FileFormatVersion = 1\nTransformMatrix = 0 1 0 -1 0 0 0 0 1\nOffset = -10 20 5",
			elementAffine, nil},
		{"version 1 ignores version 0 aliases",
			"FileFormatVersion = 1\nPosition = -10 20 5\nRotation = 0 1 0 -1 0 0 0 0 1",
			identityAffine, nil},
		// Version 0 (absent or explicit) keeps Offset and TransformMatrix
		// as the image geometry; element fields are not read.
		{"version 0 absent",
			"TransformMatrix = 1 0 0 0 1 0 0 0 1\nOffset = 0 0 0\n" + element,
			identityAffine, nil},
		{"version 0 explicit",
			"FileFormatVersion = 0\nTransformMatrix = 0 1 0 -1 0 0 0 0 1\nOffset = -10 20 5\nElementOrigin = 1 2 3",
			elementAffine, nil},
		// MetaIO reads Offset, Position, Origin and then ImagePosition, each
		// replacing the one before (#2041).
		{"version 0 Origin over Offset",
			"TransformMatrix = 0 1 0 -1 0 0 0 0 1\nOffset = 1 2 3\nOrigin = -10 20 5",
			elementAffine, nil},
		{"version 0 Position over Offset",
			"TransformMatrix = 0 1 0 -1 0 0 0 0 1\nPosition = -10 20 5\nOffset = 1 2 3",
			elementAffine, nil},
		{"version 0 Origin over Position",
			"TransformMatrix = 0 1 0 -1 0 0 0 0 1\nOrigin = -10 20 5\nPosition = 1 2 3",
			elementAffine, nil},
		{"version 0 ImagePosition over Origin and Offset",
			"TransformMatrix = 0 1 0 -1 0 0 0 0 1\nOffset = 4 5 6\nOrigin = 1 2 3\nImagePosition = -10 20 5",
			elementAffine, nil},
		{"version 1 ImagePosition over Offset",
			"FileFormatVersion = 1\nTransformMatrix = 0 1 0 -1 0 0 0 0 1\nOffset = 1 2 3\nImagePosition = -10 20 5",
			elementAffine, nil},
		{"version 1 ElementOrigin over ImagePosition",
			"FileFormatVersion = 1\nImagePosition = 1 2 3\n" + element,
			elementAffine, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image, err := readVersionFile(t, tc.geometry)
			if err != nil {
				t.Fatal(err)
			}
			if got := image.Volume.Geometry.IndexToLPS; got != tc.affine {
				t.Errorf("IndexToLPS %v, want %v", got, tc.affine)
			}
			if !slices.Equal(image.Warnings, tc.warnings) {
				t.Errorf("warnings %v, want %v", image.Warnings, tc.warnings)
			}
			if got := image.Volume.Stored(3); got != -800 {
				t.Errorf("last voxel %v", got)
			}
		})
	}
}

func TestFileFormatVersionRejections(t *testing.T) {
	for _, tc := range []struct {
		geometry string
		want     error
	}{
		{"FileFormatVersion = 2\nElementOrigin = 0 0 0", metaimage.ErrUnsupported},
		{"FileFormatVersion = one", metaimage.ErrMalformedHeader},
		{"FileFormatVersion = -1", metaimage.ErrMalformedHeader},
		{"FileFormatVersion = 1\nElementOrigin = 1 2", metaimage.ErrMalformedHeader},
		{"FileFormatVersion = 1\nElementDirection = 1 0 0 0 1 0 0 0", metaimage.ErrMalformedHeader},
	} {
		if image, err := readVersionFile(t, tc.geometry); image != nil || !errors.Is(err, tc.want) {
			t.Errorf("%q: image %v, error %v, want %v", tc.geometry, image, err, tc.want)
		}
	}
}

// TestWriteKeepsFileFormatVersion0 checks that Write produces version 0
// geometry and refuses extra fields that would turn it into version 1.
func TestWriteKeepsFileFormatVersion0(t *testing.T) {
	volume := obliqueVolume(t, [4]int{2, 2, 1, 1}, nifti.DatatypeInt16)
	var encoded bytes.Buffer
	if err := metaimage.Write(&encoded, volume, metaimage.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded.String(), "FileFormatVersion") || strings.Contains(encoded.String(), "ElementOrigin") {
		t.Fatalf("written header declares version 1 fields:\n%s", encoded.String())
	}
	image, err := metaimage.Read(context.Background(), &encoded, metaimage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if image.Header.FileFormatVersion != 0 {
		t.Fatalf("FileFormatVersion %d", image.Header.FileFormatVersion)
	}
	requireSame(t, image.Volume, volume)
	for _, key := range []string{"FileFormatVersion", "ElementOrigin", "ElementDirection"} {
		err := metaimage.Write(new(bytes.Buffer), volume, metaimage.WriteOptions{Fields: map[string]string{key: "1"}})
		if !errors.Is(err, metaimage.ErrMalformedHeader) {
			t.Errorf("extra field %s: %v", key, err)
		}
	}
}
