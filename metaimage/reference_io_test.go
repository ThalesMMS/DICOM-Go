package metaimage_test

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/metaimage"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

func TestReferenceMetaImageScalarWireTypes(t *testing.T) {
	cases := []struct {
		element  string
		datatype int16
		values   any
		want     []float64
	}{
		{"MET_CHAR", nifti.DatatypeInt8, []int8{-128, 17, 127}, []float64{-128, 17, 127}},
		{"MET_UCHAR", nifti.DatatypeUint8, []uint8{0, 128, 255}, []float64{0, 128, 255}},
		{"MET_SHORT", nifti.DatatypeInt16, []int16{-32768, 0x1234, 32767}, []float64{-32768, 0x1234, 32767}},
		{"MET_USHORT", nifti.DatatypeUint16, []uint16{0, 0x8123, 65535}, []float64{0, 0x8123, 65535}},
		{"MET_INT", nifti.DatatypeInt32, []int32{-2147483648, 0x12345678, 2147483647}, []float64{-2147483648, 0x12345678, 2147483647}},
		{"MET_UINT", nifti.DatatypeUint32, []uint32{0, 0x87654321, 4294967295}, []float64{0, 0x87654321, 4294967295}},
		{"MET_FLOAT", nifti.DatatypeFloat32, []float32{-2.25, 0.03125, 65000.5}, []float64{-2.25, 0.03125, 65000.5}},
		{"MET_DOUBLE", nifti.DatatypeFloat64, []float64{-2.25, 1.0 / 3, 4294967295.25}, []float64{-2.25, 1.0 / 3, 4294967295.25}},
	}
	for _, tc := range cases {
		for _, endian := range []struct {
			name  string
			order binary.ByteOrder
			big   bool
		}{
			{"little", binary.LittleEndian, false},
			{"big", binary.BigEndian, true},
		} {
			t.Run(tc.element+"/"+endian.name, func(t *testing.T) {
				var file bytes.Buffer
				fmt.Fprintf(&file, "NDims = 1\nDimSize = 3\nElementType = %s\nElementByteOrderMSB = %t\nElementSpacing = 1.25\nOffset = -7\nElementDataFile = LOCAL\n", tc.element, endian.big)
				if err := binary.Write(&file, endian.order, tc.values); err != nil {
					t.Fatal(err)
				}
				image, err := metaimage.Read(context.Background(), &file, metaimage.ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if dim, datatype := image.Volume.Dimensions, image.Volume.Header.Datatype; dim != [4]int{3, 1, 1, 1} || datatype != tc.datatype {
					t.Fatalf("dimensions %v and datatype %d, want [3 1 1 1] and %d", dim, datatype, tc.datatype)
				}
				checkMetaImageReferenceValues(t, image, tc.want)
				if point := image.Volume.Geometry.IndexToLPS.TransformPoint(render.Vec3{X: 2}); point != (render.Vec3{X: -4.5}) {
					t.Errorf("last voxel at %v, want (-4.5, 0, 0)", point)
				}

				var exported bytes.Buffer
				if err := metaimage.Write(&exported, image.Volume, metaimage.WriteOptions{}); err != nil {
					t.Fatal(err)
				}
				header, voxels, ok := bytes.Cut(exported.Bytes(), []byte("ElementDataFile = LOCAL\n"))
				if !ok || !bytes.Contains(header, []byte("ElementType = "+tc.element+"\n")) || !bytes.Contains(header, []byte("BinaryDataByteOrderMSB = False\n")) {
					t.Fatalf("missing inline little-endian %s header fields", tc.element)
				}
				var wantPayload bytes.Buffer
				if err := binary.Write(&wantPayload, binary.LittleEndian, tc.values); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(voxels, wantPayload.Bytes()) {
					t.Errorf("payload %x, want %x", voxels, wantPayload.Bytes())
				}
			})
		}
	}
}

// TestMetaImageHeader in VTK checks physical metadata survives a write/read.
// Check the 2D embedded basis against LPS points from a hand-authored header.
func TestReferenceMetaImage2DBasis(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
	}{
		{"canonical", "TransformMatrix = 0 1 1 0\nOffset = 11 -2\nElementSpacing = 0.5 3\n"},
		{"aliases", "Rotation = 0 1 1 0\nPosition = 11 -2\nElementSize = 0.5 3\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := "NDims = 2\nDimSize = 2 3\nElementType = MET_UCHAR\n" + tc.fields + "ElementDataFile = LOCAL\n\x01\x02\x03\x04\x05\x06"
			image, err := metaimage.Read(context.Background(), strings.NewReader(file), metaimage.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if dims, spacing := image.Volume.Dimensions, image.Volume.Geometry.Spacing; dims != [4]int{2, 3, 1, 1} || spacing != [3]float64{0.5, 3, 1} {
				t.Errorf("dimensions %v and spacing %v, want [2 3 1 1] and [0.5 3 1]", dims, spacing)
			}
			for _, p := range []struct{ index, lps render.Vec3 }{
				{render.Vec3{}, render.Vec3{X: 11, Y: -2}},
				{render.Vec3{X: 1, Y: 2}, render.Vec3{X: 17, Y: -1.5}},
				{render.Vec3{Z: 1}, render.Vec3{X: 11, Y: -2, Z: -1}},
			} {
				if got := image.Volume.Geometry.IndexToLPS.TransformPoint(p.index); got != p.lps {
					t.Errorf("index %v at %v, want %v", p.index, got, p.lps)
				}
			}
			if value := image.Volume.Stored(image.Volume.Index(1, 2, 0, 0)); value != 6 {
				t.Errorf("voxel (1, 2) = %v, want 6", value)
			}
		})
	}
}

// Adapt ITK's MetaImage metadata preservation and VTK's present, absent,
// and blank Name checks to Go's raw header API.
func TestReferenceMetaImageRawMetadata(t *testing.T) {
	const standard = "ObjectType = Image\r\nNDims = 3\r\nDimSize = 3 1 1\r\nElementType = MET_UCHAR\r\n"
	const precision = "1.2345678901234567"
	for _, tc := range []struct {
		name  string
		line  string
		value string
	}{
		{"present", "NaMe = Signal = magnitude\r\n", "Signal = magnitude"},
		{"absent", "", ""},
		{"blank", "NaMe = \t \r\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const payload = "\x0a\x3d\xff"
			r := bufio.NewReader(strings.NewReader(standard + "Calibration = " + precision + "\r\n" + tc.line + "ElementDataFile = LOCAL\r\n" + payload))
			h, err := metaimage.ParseHeader(r)
			if err != nil {
				t.Fatal(err)
			}
			wantKeys := []string{"ObjectType", "NDims", "DimSize", "ElementType", "Calibration"}
			if tc.line != "" {
				wantKeys = append(wantKeys, "NaMe")
			}
			wantKeys = append(wantKeys, "ElementDataFile")
			if !slices.Equal(h.Keys, wantKeys) || h.Fields["calibration"] != precision {
				t.Errorf("header keys or calibration changed: %v, %q", h.Keys, h.Fields["calibration"])
			}
			if value, present := h.Fields["name"]; present != (tc.line != "") || value != tc.value {
				t.Errorf("name = %q (present: %t), want %q (present: %t)", value, present, tc.value, tc.line != "")
			}
			body, err := io.ReadAll(r)
			if err != nil || string(body) != payload {
				t.Errorf("payload starts with %x, want %x; read error %v", body, payload, err)
			}
		})
	}
}

func TestReferenceMetaImageDetachedCompressionAndLimits(t *testing.T) {
	const prefix = "data-prefix"
	const baseHeader = "ObjectType = Image\nNDims = 2\nDimSize = 3 1\nElementType = MET_USHORT\nBinaryDataByteOrderMSB = True\n"
	// Big-endian voxels 0x0102, 0x89ab, and 0xff10.
	raw := []byte{1, 2, 0x89, 0xab, 0xff, 0x10}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	wrapped := fmt.Sprintf("CompressedData = True\nCompressedDataSize = %d\nHeaderSize = %d\n", compressed.Len(), len(prefix))
	for _, tc := range []struct {
		name    string
		fields  string
		body    []byte
		limits  nifti.ReadLimits
		wantErr error
	}{
		{name: "exact budgets", fields: wrapped, body: append([]byte(prefix), compressed.Bytes()...), limits: nifti.ReadLimits{MaxVoxels: 3, MaxPayloadBytes: 6}},
		{name: "voxel budget", fields: wrapped, body: append([]byte(prefix), compressed.Bytes()...), limits: nifti.ReadLimits{MaxVoxels: 2}, wantErr: metaimage.ErrLimitExceeded},
		{name: "decoded byte budget", fields: wrapped, body: append([]byte(prefix), compressed.Bytes()...), limits: nifti.ReadLimits{MaxPayloadBytes: 5}, wantErr: metaimage.ErrLimitExceeded},
		{name: "short zlib bound", fields: "CompressedData = True\nCompressedDataSize = 1\n", body: compressed.Bytes(), wantErr: metaimage.ErrMalformedHeader},
		{name: "raw short payload", body: raw[:len(raw)-1], wantErr: nifti.ErrTruncatedData},
		{name: "short preamble", fields: "HeaderSize = 7\n", body: raw, wantErr: metaimage.ErrTruncatedData},
		{name: "short end-aligned payload", fields: "HeaderSize = -1\n", body: raw[:len(raw)-1], wantErr: metaimage.ErrTruncatedData},
		{name: "compressed end offset", fields: "HeaderSize = -1\nCompressedData = True\n", body: compressed.Bytes(), wantErr: metaimage.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "data"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "data", "voxels.raw"), tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			head := filepath.Join(dir, "volume.mhd")
			if err := os.WriteFile(head, []byte(baseHeader+tc.fields+"ElementDataFile = data/voxels.raw\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			image, err := metaimage.ReadFile(context.Background(), head, metaimage.ReadOptions{Limits: tc.limits})
			if tc.wantErr != nil {
				if image != nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("image present: %t, error %v, want %v", image != nil, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			checkMetaImageReferenceValues(t, image, []float64{0x0102, 0x89ab, 0xff10})
		})
	}
}

func checkMetaImageReferenceValues(t *testing.T, image *metaimage.Image, want []float64) {
	t.Helper()
	if got := image.Volume.Len(); got != len(want) {
		t.Fatalf("voxel count %d, want %d", got, len(want))
	}
	for index, value := range want {
		if got := image.Volume.Stored(index); got != value {
			t.Errorf("voxel %d = %v, want %v", index, got, value)
		}
	}
}
