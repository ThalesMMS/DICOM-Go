package nrrd_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/nrrd"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/seg"
)

// obliqueAffine rotates 30° about z, with spacings 0.7, 0.8 and 2.5 mm.
func obliqueAffine() render.GeometryAffine {
	c, s := math.Cos(math.Pi/6), math.Sin(math.Pi/6)
	return render.GeometryAffine{
		0.7 * c, -0.8 * s, 0, -12.5,
		0.7 * s, 0.8 * c, 0, 40.25,
		0, 0, 2.5, -7,
		0, 0, 0, 1,
	}
}

func int16Volume(t *testing.T, dims [4]int) *nifti.Volume {
	t.Helper()
	var payload bytes.Buffer
	n := dims[0] * dims[1] * dims[2] * max(1, dims[3])
	for i := 0; i < n; i++ {
		_ = binary.Write(&payload, binary.LittleEndian, int16(i*37-500))
	}
	volume, err := nifti.NewVolume(context.Background(), &payload, nifti.NewVolumeOptions{Datatype: nifti.DatatypeInt16, Dimensions: dims, IndexToLPS: obliqueAffine()})
	if err != nil {
		t.Fatal(err)
	}
	return volume
}

func requireSameVolume(t *testing.T, got, want *nifti.Volume) {
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
	for _, dims := range [][4]int{{5, 4, 3, 1}, {3, 2, 2, 3}} {
		want := int16Volume(t, dims)
		for _, raw := range []bool{false, true} {
			var encoded bytes.Buffer
			if err := nrrd.Write(&encoded, want, nrrd.WriteOptions{Raw: raw, KeyValues: map[string]string{"Source": "dicom-go\ntest"}}); err != nil {
				t.Fatal(err)
			}
			image, err := nrrd.Read(context.Background(), &encoded, nrrd.ReadOptions{})
			if err != nil {
				t.Fatalf("dims %v raw %v: %v", dims, raw, err)
			}
			requireSameVolume(t, image.Volume, want)
			if image.Header.KeyValues["Source"] != "dicom-go\ntest" || image.Header.Space != "left-posterior-superior" {
				t.Fatalf("header %+v", image.Header)
			}
			if (dims[3] > 1) != (image.NonSpatialAxis == 3) {
				t.Fatalf("non-spatial axis %d", image.NonSpatialAxis)
			}
		}
	}
}

// segReferences is a synthetic axial series: 4 columns (0.5 mm), 3 rows
// (0.75 mm), 3 slices 2.5 mm apart from (-20, 30, 10) LPS.
func segReferences() []nifti.SEGReferenceImage {
	var references []nifti.SEGReferenceImage
	for slice := 0; slice < 3; slice++ {
		references = append(references, nifti.SEGReferenceImage{
			Geometry: render.SliceGeometry{
				Origin: render.Vec3{X: -20, Y: 30, Z: 10 + 2.5*float64(slice)},
				RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, Normal: render.Vec3{Z: 1},
				ColSpacing: 0.5, RowSpacing: 0.75, Rows: 3, Columns: 4,
			},
			Image: seg.ReferencedImage{
				SeriesInstanceUID: "2.25.1078.1",
				SOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
				SOPInstanceUID:    fmt.Sprintf("2.25.1078.1.%d", slice),
			},
		})
	}
	return references
}

// slicerSegmentation is a two-layer Slicer segmentation written in RAS, as
// Slicer does: layers interleaved (list axis first), the Liver in layer 0 and
// an overlapping Tumor in layer 1.
func slicerSegmentation() []byte {
	header := `NRRD0004
# Complete NRRD file format specification at:
# http://teem.sourceforge.net/nrrd/format.html
type: unsigned char
dimension: 4
space: right-anterior-superior
sizes: 2 4 3 3
space directions: none (-0.5,0,0) (0,-0.75,0) (0,0,2.5)
kinds: list domain domain domain
encoding: raw
space origin: (20,-30,10)
Segment0_Color:=0.901961 0.694118 0.54902
Segment0_ID:=Segment_1
Segment0_LabelValue:=1
Segment0_Layer:=0
Segment0_Name:=Liver
Segment0_Tags:=Segmentation.Status:inprogress|TerminologyEntry:Segmentation category and type - 3D Slicer General Anatomy list~SCT^123037004^Anatomical Structure~SCT^10200004^Liver~^^~Anatomic codes - DICOM master list~^^~^^|
Segment1_Color:=0.2 0.8 0.2
Segment1_ID:=Segment_2
Segment1_LabelValue:=1
Segment1_Layer:=1
Segment1_Name:=Tumor
Segmentation_MasterRepresentation:=Binary labelmap

`
	data := make([]byte, 2*4*3*3)
	set := func(layer, column, row, slice int) { data[2*(column+4*(row+3*slice))+layer] = 1 }
	set(0, 1, 0, 0)
	set(0, 1, 1, 0)
	set(0, 2, 1, 1)
	set(1, 2, 1, 1) // overlaps the Liver
	set(1, 3, 2, 2)
	return append([]byte(header), data...)
}

func TestSlicerSegmentationToSEG(t *testing.T) {
	image, err := nrrd.Read(context.Background(), bytes.NewReader(slicerSegmentation()), nrrd.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if image.Volume.Dimensions != [4]int{4, 3, 3, 2} || image.NonSpatialAxis != 0 {
		t.Fatalf("dimensions %v non-spatial %d", image.Volume.Dimensions, image.NonSpatialAxis)
	}
	if origin := image.Volume.Geometry.IndexToLPS.TransformPoint(render.Vec3{}); origin != (render.Vec3{X: -20, Y: 30, Z: 10}) {
		t.Fatalf("RAS origin read as %v LPS", origin)
	}
	segments, err := image.Header.SlicerSegments()
	if err != nil || len(segments) != 2 || segments[0].Name != "Liver" || segments[1].Layer != 1 {
		t.Fatalf("segments %+v %v", segments, err)
	}
	if segments[0].Type.CodeValue != "10200004" || segments[0].Category.CodingSchemeDesignator != "SCT" {
		t.Fatalf("terminology %+v %+v", segments[0].Category, segments[0].Type)
	}
	options := nifti.SEGOptions{
		SOPInstanceUID: "2.25.1078.2", StudyInstanceUID: "2.25.1078.3", SeriesInstanceUID: "2.25.1078.4",
		FrameOfReferenceUID: "2.25.1078.5", References: segReferences(),
	}
	want := map[int]map[string][][2]int{
		0: {"2.25.1078.1.0": {{1, 0}, {1, 1}}, "2.25.1078.1.1": {{2, 1}}},
		1: {"2.25.1078.1.1": {{2, 1}}, "2.25.1078.1.2": {{3, 2}}},
	}
	for layer, name := range []string{"Liver", "Tumor"} {
		document, _, err := image.SEGDocument(layer, options)
		if err != nil {
			t.Fatalf("layer %d: %v", layer, err)
		}
		if len(document.Segments) != 1 || document.Segments[0].Label != name {
			t.Fatalf("layer %d segments %+v", layer, document.Segments)
		}
		got := map[string][][2]int{}
		for _, frame := range document.Frames {
			for row := 0; row < frame.Mask.Rows; row++ {
				for column := 0; column < frame.Mask.Columns; column++ {
					if frame.Mask.Get(column, row) {
						got[frame.ReferencedSOPInstanceUID] = append(got[frame.ReferencedSOPInstanceUID], [2]int{column, row})
					}
				}
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want[layer]) {
			t.Fatalf("layer %d pixels %v, want %v", layer, got, want[layer])
		}
	}
}

func encodedHeader(encoding, endian, extra string) string {
	header := "NRRD0005\ntype: short\ndimension: 3\nspace: LPS\nsizes: 2 2 1\nspace directions: (1,0,0) (0,1,0) (0,0,1)\nencoding: " + encoding + "\n"
	if endian != "" {
		header += "endian: " + endian + "\n"
	}
	return header + extra + "\n"
}

// TestEncodings reads the same four samples in every encoding.
func TestEncodings(t *testing.T) {
	want := []int16{1, -2, 300, -400}
	little, big := new(bytes.Buffer), new(bytes.Buffer)
	_ = binary.Write(little, binary.LittleEndian, want)
	_ = binary.Write(big, binary.BigEndian, want)
	bz2, _ := hex.DecodeString("425a68393141592653597ee83263000000d080e000000440000001a00022186830056100c2ee48a70a120fdd064c60")
	for _, tc := range []struct {
		name string
		file string
	}{
		{"raw little", encodedHeader("raw", "little", "") + little.String()},
		{"raw big", encodedHeader("raw", "big", "") + big.String()},
		{"text", encodedHeader("ascii", "", "") + "1 -2\n300\t-400\n"},
		{"hex", encodedHeader("hex", "big", "") + hex.EncodeToString(big.Bytes())[:6] + "\n" + hex.EncodeToString(big.Bytes())[6:]},
		{"bzip2", encodedHeader("bz2", "little", "") + string(bz2)},
		{"byte skip", encodedHeader("raw", "little", "byte skip: 3\n") + "abc" + little.String()},
	} {
		image, err := nrrd.Read(context.Background(), strings.NewReader(tc.file), nrrd.ReadOptions{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for i, value := range want {
			if got := image.Volume.Stored(i); got != float64(value) {
				t.Fatalf("%s: sample %d = %v, want %d", tc.name, i, got, value)
			}
		}
	}
}

func TestDetachedHeader(t *testing.T) {
	directory := t.TempDir()
	payload := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	if err := os.WriteFile(filepath.Join(directory, "image.raw"), append([]byte("junk header "), payload...), 0o644); err != nil {
		t.Fatal(err)
	}
	header := encodedHeader("raw", "little", "data file: image.raw\nbyte skip: -1\n")
	path := filepath.Join(directory, "image.nhdr")
	if err := os.WriteFile(path, []byte(header), 0o644); err != nil {
		t.Fatal(err)
	}
	image, err := nrrd.ReadFile(context.Background(), path, nrrd.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if image.Volume.Stored(3) != 4 {
		t.Fatalf("last sample %v", image.Volume.Stored(3))
	}
	if _, err := nrrd.Read(context.Background(), strings.NewReader(header), nrrd.ReadOptions{}); !errors.Is(err, nrrd.ErrUnsupported) {
		t.Fatalf("detached header from a stream: %v", err)
	}
	for _, name := range []string{"../image.raw", filepath.Join(directory, "image.raw"), "LIST"} {
		escape := filepath.Join(directory, "escape.nhdr")
		if err := os.WriteFile(escape, []byte(encodedHeader("raw", "little", "data file: "+filepath.ToSlash(name)+"\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := nrrd.ReadFile(context.Background(), escape, nrrd.ReadOptions{}); err == nil {
			t.Fatalf("data file %q accepted", name)
		}
	}
}

func TestSpacesAndErrors(t *testing.T) {
	spacings := "NRRD0004\ntype: uint8\ndimension: 3\nsizes: 2 1 1\nspacings: 0.5 0.5 3\nencoding: raw\n\n\x01\x02"
	if _, err := nrrd.Read(context.Background(), strings.NewReader(spacings), nrrd.ReadOptions{}); !errors.Is(err, nrrd.ErrNoPatientSpace) {
		t.Fatalf("spacings only: %v", err)
	}
	image, err := nrrd.Read(context.Background(), strings.NewReader(spacings), nrrd.ReadOptions{AllowNonAnatomicalSpace: true})
	if err != nil || image.Volume.Geometry.Spacing != [3]float64{0.5, 0.5, 3} || len(image.Warnings) == 0 || image.Warnings[0] != nrrd.WarningNonAnatomicalSpace {
		t.Fatalf("permitted spacings-only file: %+v %v", image, err)
	}
	// LAS flips the anterior axis only; a 2D image gets a unit normal.
	las := "NRRD0004\ntype: uint8\ndimension: 2\nspace: left-anterior-superior\nsizes: 2 1\nspace directions: (0.5,0,0) (0,2,0)\nspace origin: (1,2,3)\nencoding: raw\n\n\x01\x02"
	image, err = nrrd.Read(context.Background(), strings.NewReader(las), nrrd.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := image.Volume.Geometry.IndexToLPS; got.TransformPoint(render.Vec3{}) != (render.Vec3{X: 1, Y: -2, Z: 3}) || got[5] != -2 || image.Volume.Dimensions != [4]int{2, 1, 1, 1} {
		t.Fatalf("LAS geometry %v dims %v", got, image.Volume.Dimensions)
	}
	for name, tc := range map[string]struct {
		file string
		err  error
	}{
		"not NRRD":      {"P5\n", nrrd.ErrNotNRRD},
		"64-bit type":   {"NRRD0004\ntype: long long\ndimension: 1\nsizes: 1\nencoding: raw\nendian: little\nspace: LPS\nspace directions: (1,0,0)\n\n12345678", nrrd.ErrUnsupported},
		"no endian":     {"NRRD0004\ntype: short\ndimension: 3\nspace: LPS\nsizes: 1 1 1\nspace directions: (1,0,0) (0,1,0) (0,0,1)\nencoding: raw\n\n12", nrrd.ErrMalformedHeader},
		"sizes":         {"NRRD0004\ntype: uint8\ndimension: 3\nsizes: 1 1\nencoding: raw\n\n", nrrd.ErrMalformedHeader},
		"truncated":     {encodedHeader("raw", "little", "") + "12", nil},
		"scanner space": {"NRRD0004\ntype: uint8\ndimension: 3\nspace: scanner-xyz\nsizes: 1 1 1\nspace directions: (1,0,0) (0,1,0) (0,0,1)\nencoding: raw\n\n\x00", nrrd.ErrNoPatientSpace},
		"two lists":     {"NRRD0004\ntype: uint8\ndimension: 4\nspace: LPS\nsizes: 1 1 1 1\nspace directions: none (1,0,0) (0,1,0) none\nencoding: raw\n\n\x00", nrrd.ErrUnsupported},
	} {
		_, err := nrrd.Read(context.Background(), strings.NewReader(tc.file), nrrd.ReadOptions{})
		if err == nil || (tc.err != nil && !errors.Is(err, tc.err)) {
			t.Errorf("%s: %v, want %v", name, err, tc.err)
		}
	}
	big := "NRRD0004\ntype: uint8\ndimension: 3\nspace: LPS\nsizes: 1000 1000 1000\nspace directions: (1,0,0) (0,1,0) (0,0,1)\nencoding: raw\n\n"
	if _, err := nrrd.Read(context.Background(), strings.NewReader(big), nrrd.ReadOptions{Limits: nifti.ReadLimits{MaxVoxels: 1 << 20}}); !errors.Is(err, nrrd.ErrLimitExceeded) {
		t.Fatalf("limits: %v", err)
	}
}

func FuzzReadNRRD(f *testing.F) {
	f.Add(slicerSegmentation())
	f.Add([]byte(encodedHeader("ascii", "", "") + "1 2 3 4"))
	f.Add([]byte("NRRD0004\ntype: uint8\ndimension: 2\nspace: LAS\nsizes: 2 1\nspace directions: (0.5,0,0) (0,2,0)\nencoding: hex\n\n0102"))
	f.Fuzz(func(t *testing.T, data []byte) {
		limits := nifti.ReadLimits{MaxVoxels: 1 << 16, MaxPayloadBytes: 1 << 18}
		image, err := nrrd.Read(context.Background(), bytes.NewReader(data), nrrd.ReadOptions{Limits: limits, AllowNonAnatomicalSpace: true})
		if err != nil {
			return
		}
		var encoded bytes.Buffer
		if err := nrrd.Write(&encoded, image.Volume, nrrd.WriteOptions{}); err != nil {
			return
		}
		again, err := nrrd.Read(context.Background(), &encoded, nrrd.ReadOptions{Limits: limits})
		if err != nil {
			t.Fatalf("rewritten file does not read back: %v", err)
		}
		if again.Volume.Dimensions != image.Volume.Dimensions {
			t.Fatalf("dimensions %v, want %v", again.Volume.Dimensions, image.Volume.Dimensions)
		}
	})
	_ = bufio.NewReader
}
