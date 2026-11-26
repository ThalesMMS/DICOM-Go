package metaimage_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/metaimage"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

func obliqueVolume(t *testing.T, dims [4]int, datatype int16) *nifti.Volume {
	t.Helper()
	c, s := math.Cos(math.Pi/5), math.Sin(math.Pi/5)
	affine := render.GeometryAffine{
		0.6 * c, 0, -1.5 * s, 10,
		0, 0.9, 0, -20.5,
		0.6 * s, 0, 1.5 * c, 3.25,
		0, 0, 0, 1,
	}
	var payload bytes.Buffer
	n := dims[0] * dims[1] * dims[2] * max(1, dims[3])
	for i := 0; i < n; i++ {
		switch datatype {
		case nifti.DatatypeFloat32:
			_ = binary.Write(&payload, binary.LittleEndian, float32(i)*0.25-3)
		default:
			_ = binary.Write(&payload, binary.LittleEndian, int16(i*41-700))
		}
	}
	volume, err := nifti.NewVolume(context.Background(), &payload, nifti.NewVolumeOptions{Datatype: datatype, Dimensions: dims, IndexToLPS: affine})
	if err != nil {
		t.Fatal(err)
	}
	return volume
}

func requireSame(t *testing.T, got, want *nifti.Volume) {
	t.Helper()
	if got.Dimensions != want.Dimensions || got.Header.Datatype != want.Header.Datatype {
		t.Fatalf("dimensions %v datatype %d, want %v %d", got.Dimensions, got.Header.Datatype, want.Dimensions, want.Header.Datatype)
	}
	for i := range got.Geometry.IndexToLPS {
		if math.Abs(got.Geometry.IndexToLPS[i]-want.Geometry.IndexToLPS[i]) > 1e-12 {
			t.Fatalf("IndexToLPS %v, want %v", got.Geometry.IndexToLPS, want.Geometry.IndexToLPS)
		}
	}
	for i := 0; i < want.Len(); i++ {
		if got.Stored(i) != want.Stored(i) {
			t.Fatalf("voxel %d = %v, want %v", i, got.Stored(i), want.Stored(i))
		}
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		dims     [4]int
		datatype int16
	}{{[4]int{4, 3, 2, 1}, nifti.DatatypeInt16}, {[4]int{3, 2, 2, 2}, nifti.DatatypeFloat32}} {
		want := obliqueVolume(t, tc.dims, tc.datatype)
		for _, compress := range []bool{false, true} {
			var encoded bytes.Buffer
			if err := metaimage.Write(&encoded, want, metaimage.WriteOptions{Compress: compress, Fields: map[string]string{"Modality": "MET_MOD_CT"}}); err != nil {
				t.Fatal(err)
			}
			image, err := metaimage.Read(context.Background(), &encoded, metaimage.ReadOptions{})
			if err != nil {
				t.Fatalf("%v compress %v: %v", tc.dims, compress, err)
			}
			requireSame(t, image.Volume, want)
			if image.Header.Fields["modality"] != "MET_MOD_CT" || len(image.Warnings) != 0 {
				t.Fatalf("fields %v warnings %v", image.Header.Fields, image.Warnings)
			}
		}
	}
	if err := metaimage.Write(new(bytes.Buffer), obliqueVolume(t, [4]int{1, 1, 1, 1}, nifti.DatatypeInt16), metaimage.WriteOptions{Fields: map[string]string{"DimSize": "9"}}); !errors.Is(err, metaimage.ErrMalformedHeader) {
		t.Fatalf("reserved field accepted: %v", err)
	}
}

// itkHeader is a header in the form ITK's MetaImageIO writes.
const itkHeader = `ObjectType = Image
NDims = 3
BinaryData = True
BinaryDataByteOrderMSB = %s
CompressedData = False
TransformMatrix = 0 1 0 -1 0 0 0 0 1
Offset = -10 20 5
CenterOfRotation = 0 0 0
AnatomicalOrientation = %s
ElementSpacing = 0.5 0.75 2
DimSize = 2 2 1
ElementType = MET_SHORT
ElementDataFile = %s
`

func TestITKStyleFiles(t *testing.T) {
	values := []int16{5, -6, 700, -800}
	little, big := new(bytes.Buffer), new(bytes.Buffer)
	_ = binary.Write(little, binary.LittleEndian, values)
	_ = binary.Write(big, binary.BigEndian, values)
	check := func(name string, image *metaimage.Image, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, v := range values {
			if image.Volume.Stored(i) != float64(v) {
				t.Fatalf("%s: voxel %d = %v", name, i, image.Volume.Stored(i))
			}
		}
		// Row i of TransformMatrix is axis i: i runs along +y (0.5 mm), j
		// along -x (0.75 mm).
		a := image.Volume.Geometry.IndexToLPS
		if a.TransformPoint(render.Vec3{X: 1, Y: 1}) != (render.Vec3{X: -10.75, Y: 20.5, Z: 5}) {
			t.Fatalf("%s: geometry %v", name, a)
		}
	}
	image, err := metaimage.Read(context.Background(), strings.NewReader(sprintf(itkHeader, "False", "ALI", "LOCAL")+little.String()), metaimage.ReadOptions{})
	check("mha", image, err)
	if len(image.Warnings) != 0 {
		t.Fatalf("orientation warning for a consistent header: %v", image.Warnings)
	}
	image, err = metaimage.Read(context.Background(), strings.NewReader(sprintf(itkHeader, "True", "RAI", "LOCAL")+big.String()), metaimage.ReadOptions{})
	check("big-endian mha", image, err)
	if len(image.Warnings) != 1 || image.Warnings[0] != metaimage.WarningOrientationIgnored {
		t.Fatalf("inconsistent orientation: %v", image.Warnings)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "image.raw"), append([]byte("preamble"), little.Bytes()...), 0o644); err != nil {
		t.Fatal(err)
	}
	mhd := filepath.Join(directory, "image.mhd")
	if err := os.WriteFile(mhd, []byte(strings.Replace(sprintf(itkHeader, "False", "ALI", "image.raw"), "DimSize", "HeaderSize = -1\nDimSize", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	image, err = metaimage.ReadFile(context.Background(), mhd, metaimage.ReadOptions{})
	check("mhd", image, err)
	for _, name := range []string{"../image.raw", "LIST"} {
		escape := filepath.Join(directory, "escape.mhd")
		if err := os.WriteFile(escape, []byte(sprintf(itkHeader, "False", "ALI", name)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := metaimage.ReadFile(context.Background(), escape, metaimage.ReadOptions{}); err == nil {
			t.Fatalf("data file %q accepted", name)
		}
	}
	if _, err := metaimage.Read(context.Background(), strings.NewReader(sprintf(itkHeader, "False", "ALI", "image.raw")), metaimage.ReadOptions{}); !errors.Is(err, metaimage.ErrUnsupported) {
		t.Fatalf("separate data file from a stream: %v", err)
	}
}

func sprintf(format string, args ...string) string {
	for _, arg := range args {
		format = strings.Replace(format, "%s", arg, 1)
	}
	return format
}

func TestErrors(t *testing.T) {
	base := "ObjectType = Image\nNDims = 2\nDimSize = 2 1\nElementType = MET_UCHAR\n"
	for name, tc := range map[string]struct {
		file string
		err  error
	}{
		"empty":        {"", metaimage.ErrNotMetaImage},
		"no data file": {base, metaimage.ErrMalformedHeader},
		"channels":     {strings.Replace(base, "DimSize", "ElementNumberOfChannels = 3\nDimSize", 1) + "ElementDataFile = LOCAL\n", metaimage.ErrUnsupported},
		"64-bit":       {strings.Replace(base, "MET_UCHAR", "MET_LONG_LONG", 1) + "ElementDataFile = LOCAL\n", metaimage.ErrUnsupported},
		"object":       {strings.Replace(base, "Image", "Tube", 1) + "ElementDataFile = LOCAL\n", metaimage.ErrUnsupported},
		"dims":         {strings.Replace(base, "2 1", "2", 1) + "ElementDataFile = LOCAL\n", metaimage.ErrMalformedHeader},
		"spacing":      {base + "ElementSpacing = 1 0\nElementDataFile = LOCAL\n", metaimage.ErrMalformedHeader},
		"truncated":    {base + "ElementDataFile = LOCAL\n\x01", nil},
		"repeated":     {base + "NDims = 2\nElementDataFile = LOCAL\n", metaimage.ErrMalformedHeader},
	} {
		_, err := metaimage.Read(context.Background(), strings.NewReader(tc.file), metaimage.ReadOptions{})
		if err == nil || (tc.err != nil && !errors.Is(err, tc.err)) {
			t.Errorf("%s: %v, want %v", name, err, tc.err)
		}
	}
	// ElementSize stands in for a missing ElementSpacing; 2D gets a unit
	// normal.
	image, err := metaimage.Read(context.Background(), strings.NewReader(base+"ElementSize = 0.5 3\nElementDataFile = LOCAL\n\x01\x02"), metaimage.ReadOptions{})
	if err != nil || image.Volume.Geometry.Spacing != [3]float64{0.5, 3, 1} {
		t.Fatalf("ElementSize: %v %v", image, err)
	}
	huge := "ObjectType = Image\nNDims = 3\nDimSize = 1000 1000 1000\nElementType = MET_UCHAR\nElementDataFile = LOCAL\n"
	if _, err := metaimage.Read(context.Background(), strings.NewReader(huge), metaimage.ReadOptions{Limits: nifti.ReadLimits{MaxVoxels: 1 << 20}}); !errors.Is(err, metaimage.ErrLimitExceeded) {
		t.Fatalf("limits: %v", err)
	}
}

func FuzzReadMetaImage(f *testing.F) {
	f.Add([]byte(sprintf(itkHeader, "False", "ALI", "LOCAL") + "\x01\x00\x02\x00\x03\x00\x04\x00"))
	f.Add([]byte("NDims = 2\nDimSize = 2 1\nElementType = MET_FLOAT\nCompressedData = True\nElementDataFile = LOCAL\nx\x9c"))
	f.Fuzz(func(t *testing.T, data []byte) {
		limits := nifti.ReadLimits{MaxVoxels: 1 << 16, MaxPayloadBytes: 1 << 18}
		image, err := metaimage.Read(context.Background(), bytes.NewReader(data), metaimage.ReadOptions{Limits: limits})
		if err != nil {
			return
		}
		var encoded bytes.Buffer
		if err := metaimage.Write(&encoded, image.Volume, metaimage.WriteOptions{Compress: true}); err != nil {
			return
		}
		again, err := metaimage.Read(context.Background(), &encoded, metaimage.ReadOptions{Limits: limits})
		if err != nil {
			t.Fatalf("rewritten file does not read back: %v", err)
		}
		if again.Volume.Dimensions != image.Volume.Dimensions {
			t.Fatalf("dimensions %v, want %v", again.Volume.Dimensions, image.Volume.Dimensions)
		}
	})
}
