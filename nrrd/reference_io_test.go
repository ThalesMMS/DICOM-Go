package nrrd_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/nrrd"
	"github.com/ThalesMMS/dicom-go/render"
)

func gzipNRRDTestPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

// The ITK scalar I/O matrix and VTK TestNrrdReader use alternate
// encodings of the same data. Here both the samples and physical coordinates
// are known independently of the Go writer.
func TestNRRDReferenceScalarEncodings(t *testing.T) {
	want := []float64{-100.5, 0.125, 65536.00000000001, -1e-12, 83.75, -9.5, 42, 0}
	for _, tc := range []struct {
		name     string
		encoding string
		endian   string
		order    binary.ByteOrder
	}{
		{"ascii", "ascii", "", nil},
		{"raw-le", "raw", "little", binary.LittleEndian},
		{"raw-be", "raw", "big", binary.BigEndian},
		{"gzip-le", "gzip", "little", binary.LittleEndian},
		{"gzip-be", "gzip", "big", binary.BigEndian},
	} {
		for _, location := range []string{"attached", "detached"} {
			t.Run(tc.name+"/"+location, func(t *testing.T) {
				header := "NRRD0005\ntype: double\ndimension: 3\nsizes: 2 2 2\nspace: RAS\n" +
					"space directions: (0,-0.75,0) (0.625,0,0) (0,0,2.25)\nspace origin: (2.125,-3.5,7.75)\n" +
					"encoding: " + tc.encoding + "\n"
				var payload []byte
				if tc.order == nil {
					payload = []byte("-100.5 0.125\n65536.00000000001 -1e-12\t83.75 -9.5 42 0\n")
				} else {
					header += "endian: " + tc.endian + "\n"
					for _, value := range want {
						var encoded [8]byte
						tc.order.PutUint64(encoded[:], math.Float64bits(value))
						payload = append(payload, encoded[:]...)
					}
					if tc.encoding == "gzip" {
						payload = gzipNRRDTestPayload(t, payload)
					}
				}
				var image *nrrd.Image
				var err error
				if location == "detached" {
					dir := t.TempDir()
					path := filepath.Join(dir, "volume.nhdr")
					if err := os.WriteFile(filepath.Join(dir, "voxels.bin"), payload, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(header+"data file: voxels.bin\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					image, err = nrrd.ReadFile(context.Background(), path, nrrd.ReadOptions{})
				} else {
					header = strings.ReplaceAll(header+"\n", "\n", "\r\n")
					image, err = nrrd.Read(context.Background(), bytes.NewReader(append([]byte(header), payload...)), nrrd.ReadOptions{})
				}
				if err != nil {
					t.Fatal(err)
				}
				if image.Volume.Dimensions != [4]int{2, 2, 2, 1} || image.Volume.Header.Datatype != nifti.DatatypeFloat64 || image.NonSpatialAxis != -1 {
					t.Fatalf("dimensions %v, datatype %d, non-spatial axis %d", image.Volume.Dimensions, image.Volume.Header.Datatype, image.NonSpatialAxis)
				}
				for i, expected := range want {
					if got := image.Volume.Stored(i); got != expected {
						t.Errorf("sample %d = %.17g, want %.17g", i, got, expected)
					}
				}
				for _, point := range []struct{ index, lps render.Vec3 }{
					{render.Vec3{}, render.Vec3{X: -2.125, Y: 3.5, Z: 7.75}},
					{render.Vec3{X: 1}, render.Vec3{X: -2.125, Y: 4.25, Z: 7.75}},
					{render.Vec3{Y: 1, Z: 1}, render.Vec3{X: -2.75, Y: 3.5, Z: 10}},
				} {
					if got := image.Volume.Geometry.IndexToLPS.TransformPoint(point.index); got != point.lps {
						t.Errorf("index %v maps to LPS %v, want %v", point.index, got, point.lps)
					}
				}
			})
		}
	}
}

// ITK uses separate fixtures for fast and slow component axes. A Go volume
// stores a full spatial grid per component, regardless of input order.
func TestNRRDReferenceComponentAxis(t *testing.T) {
	const spatial = "(1,0,0) (0,2,0) (0,0,3)"
	const metadata = "\nmeasurement frame: (0,1,0) (-1,0,0) (0,0,1)\nspace units: \"cm\" \"cm\" \"cm\"\n"
	want := []int16{101, 102, 103, 104, -201, -202, -203, -204, 301, 302, 303, 304}
	for _, tc := range []struct {
		name, sizes, directions, kinds string
		axis                           int
		samples                        []int16
	}{
		{"fastest", "3 2 1 2", "none " + spatial, "vector domain domain domain", 0,
			[]int16{101, -201, 301, 102, -202, 302, 103, -203, 303, 104, -204, 304}},
		{"slowest", "2 1 2 3", spatial + " none", "domain domain domain vector", 3,
			[]int16{101, 102, 103, 104, -201, -202, -203, -204, 301, 302, 303, 304}},
	} {
		for _, encoding := range []string{"raw", "gzip"} {
			t.Run(tc.name+"/"+encoding, func(t *testing.T) {
				header := fmt.Sprintf("NRRD0005\ntype: short\ndimension: 4\nspace: LPS\nsizes: %s\nspace directions: %s\nkinds: %s\nendian: big\nencoding: %s%s\n",
					tc.sizes, tc.directions, tc.kinds, encoding, metadata)
				var payload []byte
				for _, sample := range tc.samples {
					payload = binary.BigEndian.AppendUint16(payload, uint16(sample))
				}
				if encoding == "gzip" {
					payload = gzipNRRDTestPayload(t, payload)
				}
				image, err := nrrd.Read(context.Background(), bytes.NewReader(append([]byte(header), payload...)), nrrd.ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if image.NonSpatialAxis != tc.axis || image.Volume.Dimensions != [4]int{2, 1, 2, 3} {
					t.Fatalf("non-spatial axis %d, dimensions %v", image.NonSpatialAxis, image.Volume.Dimensions)
				}
				for i, expected := range want {
					if got := image.Volume.Stored(i); got != float64(expected) {
						t.Errorf("sample %d = %v, want %v", i, got, expected)
					}
				}
				point := image.Volume.Geometry.IndexToLPS.TransformPoint(render.Vec3{X: 1, Z: 1})
				if point != (render.Vec3{X: 1, Z: 3}) {
					t.Errorf("index (1, 0, 1) maps to %v, want (1, 0, 3)", point)
				}
				for _, warning := range []string{nrrd.WarningMeasurementFrameIgnored, nrrd.WarningSpaceUnits} {
					if !slices.Contains(image.Warnings, warning) {
						t.Errorf("missing warning %q in %v", warning, image.Warnings)
					}
				}
				var rewritten bytes.Buffer
				if err := nrrd.Write(&rewritten, image.Volume, nrrd.WriteOptions{Raw: true, FourthAxisKind: "vector"}); err != nil {
					t.Fatal(err)
				}
				fields, samples, ok := bytes.Cut(rewritten.Bytes(), []byte("\n\n"))
				if !ok || !bytes.Contains(fields, []byte("\nkinds: domain domain domain vector\nendian: little\n")) || len(samples) != 2*len(want) {
					t.Fatalf("rewritten header %q with %d payload bytes", fields, len(samples))
				}
				for i, expected := range want {
					if got := int16(binary.LittleEndian.Uint16(samples[i*2:])); got != expected {
						t.Errorf("rewritten sample %d = %d, want %d", i, got, expected)
					}
				}
			})
		}
	}
}

func TestNRRDReferenceCustomMetadata(t *testing.T) {
	const fields = "NRRD0005\ntype: uchar\ndimension: 3\nsizes: 1 1 1\nspace: LPS\n" +
		"space directions: (1,0,0) (0,1,0) (0,0,1)\nencoding: raw\ncenterings: cell node cell\nkinds: none domain domain\n"
	const highPrecision = "3.141592653589793"
	const escaped = `first\nsecond\\line`
	const metadata = "NRRD_pixel_original_axis:=2\nNRRD_kinds[0]_vendor:=" + escaped + "\nhigh_precision_double:=" + highPrecision + "\n"
	image, err := nrrd.Read(context.Background(), strings.NewReader(fields+metadata+"\n\x17"), nrrd.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if image.Header.Fields["centers"] != "cell node cell" || image.Header.Kinds[0] != "none" {
		t.Errorf("centers %q, kinds %v", image.Header.Fields["centers"], image.Header.Kinds)
	}
	wantKeys := []string{"NRRD_pixel_original_axis", "NRRD_kinds[0]_vendor", "high_precision_double"}
	if got := image.Header.KeyValueKeys(); !slices.Equal(got, wantKeys) {
		t.Errorf("keys %v, want %v", got, wantKeys)
	}
	if got := image.Header.KeyValues["NRRD_kinds[0]_vendor"]; got != "first\nsecond\\line" {
		t.Errorf("decoded vendor metadata %q", got)
	}
	var output bytes.Buffer
	if err := nrrd.Write(&output, image.Volume, nrrd.WriteOptions{Raw: true, KeyValues: image.Header.KeyValues}); err != nil {
		t.Fatal(err)
	}
	header, payload, ok := strings.Cut(output.String(), "\n\n")
	wantMetadata := "\nNRRD_kinds[0]_vendor:=" + escaped + "\nNRRD_pixel_original_axis:=2\nhigh_precision_double:=" + highPrecision
	if !ok || payload != "\x17" || !strings.HasSuffix(header, wantMetadata) {
		t.Fatalf("header %q, payload %x", header, payload)
	}
}

func TestNRRDReferenceReadRejections(t *testing.T) {
	const header = "NRRD0005\nspace: LPS\nspace directions: (1,0,0) (0,1,0) (0,0,1)\n"
	const scalar = header + "dimension: 3\nsizes: 2 1 1\n"
	for _, tc := range []struct {
		name   string
		file   string
		limits nifti.ReadLimits
		want   error
	}{
		{"extra-size-at-maximum-dimension", "NRRD0004\ntype: uchar\ndimension: 16\nsizes:" + strings.Repeat(" 1", 17) + "\nencoding: raw\n\n", nifti.ReadLimits{}, nrrd.ErrMalformedHeader},
		{"invalid-origin", scalar + "type: uchar\nspace origin: (0,nan,0)\nencoding: raw\n\n\x01\x02", nifti.ReadLimits{}, nrrd.ErrMalformedHeader},
		{"signed-text-overflow", scalar + "type: int8\nencoding: text\n\n127 -129", nifti.ReadLimits{}, nrrd.ErrMalformedHeader},
		{"unsigned-text-overflow", scalar + "type: uint32\nencoding: text\n\n4294967295 4294967296", nifti.ReadLimits{}, nrrd.ErrMalformedHeader},
		{"truncated-text", scalar + "type: short\nencoding: text\n\n42", nifti.ReadLimits{}, nrrd.ErrTruncatedData},
		{"truncated-gzip-data", scalar + "type: uchar\nencoding: gzip\n\n" + string(gzipNRRDTestPayload(t, []byte{1})), nifti.ReadLimits{}, nifti.ErrTruncatedData},
		{"voxel-limit", scalar + "type: uchar\nencoding: gzip\n\n", nifti.ReadLimits{MaxVoxels: 1}, nrrd.ErrLimitExceeded},
		{"decoded-byte-limit", scalar + "type: double\nendian: big\nencoding: gzip\n\n", nifti.ReadLimits{MaxPayloadBytes: 15}, nrrd.ErrLimitExceeded},
		{"component-axis-between-spatial-axes", "NRRD0005\ntype: uchar\ndimension: 4\nsizes: 1 2 1 1\nspace: LPS\nspace directions: (1,0,0) none (0,1,0) (0,0,1)\nencoding: raw\n\n\x01\x02", nifti.ReadLimits{}, nrrd.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image, err := nrrd.Read(context.Background(), strings.NewReader(tc.file), nrrd.ReadOptions{Limits: tc.limits})
			if image != nil || !errors.Is(err, tc.want) {
				t.Fatalf("image %v, error %v, want nil and %v", image, err, tc.want)
			}
		})
	}
}
