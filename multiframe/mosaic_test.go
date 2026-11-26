package multiframe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/siemens"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type mosaicFixture struct {
	images        int
	tileRows      int
	tileColumns   int
	grid          int
	normal        []string // CSA SliceNormalVector; nil omits it
	protocol      string   // CSA series MrPhoenixProtocol; "" omits it
	omitImages    bool
	acqMatrix     []uint16
	phaseEncoding string
}

var (
	mosaicRow    = [3]float64{0.9993014484, -0.0117647762, 0.0354711897}
	mosaicColumn = [3]float64{-0.0000000171, 0.9491553448, 0.3148080868}
	mosaicIPP    = [3]float64{-490.15, -457.77, -197.01}
)

const mosaicSpacing = 6.125

func dsValues(values ...float64) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = strconv.FormatFloat(v, 'g', -1, 64)
	}
	return out
}

// mosaicTestFile builds a 16-bit mosaic whose samples encode tile, row and
// column: tile*1000 + y*32 + x.
func mosaicTestFile(t *testing.T, f mosaicFixture) *object.File {
	t.Helper()
	rows, columns := f.tileRows*f.grid, f.tileColumns*f.grid
	pixels := make([]byte, rows*columns*2)
	for tile := range f.images {
		for y := range f.tileRows {
			for x := range f.tileColumns {
				offset := ((tile/f.grid*f.tileRows+y)*columns + tile%f.grid*f.tileColumns + x) * 2
				binary.LittleEndian.PutUint16(pixels[offset:], uint16(tile*1000+y*32+x))
			}
		}
	}
	var imageElements []siemens.CSAElement
	if !f.omitImages {
		imageElements = append(imageElements, siemens.CSAElement{Name: "NumberOfImagesInMosaic", VM: 1, VR: "US", SyngoDT: 10, Items: 6, Values: []string{strconv.Itoa(f.images)}})
	}
	if f.normal != nil {
		imageElements = append(imageElements, siemens.CSAElement{Name: "SliceNormalVector", VM: 3, VR: "FD", SyngoDT: 4, Items: 6, Values: f.normal})
	}
	imageCSA, err := siemens.EncodeCSA(imageElements)
	if err != nil {
		t.Fatal(err)
	}
	elements := []core.Element{
		stringsElement(tagImageType, core.VRCS, []string{"ORIGINAL", "PRIMARY", "M", "ND", "MOSAIC"}),
		ui(tagSOPClassUID, MRImageStorageUID),
		ui(tagSOPInstanceUID, "1.2.826.0.1.3680043.10.1.980.1"),
		ui(tagStudyInstanceUID, "1.2.826.0.1.3680043.10.1.980.2"),
		ui(tagSeriesInstanceUID, "1.2.826.0.1.3680043.10.1.980.3"),
		str(tagModality, core.VRCS, "MR"),
		str(tagManufacturer, core.VRLO, "SIEMENS"),
		str(tagSliceThickness, core.VRDS, "4.9"),
		str(tagSpacingBetweenSlices, core.VRDS, strconv.FormatFloat(mosaicSpacing, 'g', -1, 64)),
		stringsElement(tagImagePositionPatient, core.VRDS, dsValues(mosaicIPP[:]...)),
		stringsElement(tagImageOrientationPatient, core.VRDS, dsValues(append(mosaicRow[:], mosaicColumn[:]...)...)),
		us(tagSamplesPerPixel, 1),
		str(tagPhotometricInterpretation, core.VRCS, "MONOCHROME2"),
		us(tagRows, uint16(rows)),
		us(tagColumns, uint16(columns)),
		stringsElement(tagPixelSpacing, core.VRDS, []string{"3", "2.5"}),
		us(tagBitsAllocated, 16),
		us(tagBitsStored, 12),
		us(tagHighBit, 11),
		us(tagPixelRepresentation, 0),
		us(core.NewTag(0x0028, 0x0107), 4095),
		str(core.NewTag(0x0029, 0x0010), core.VRLO, "SIEMENS CSA HEADER"),
		core.NewRawElement(core.NewTag(0x0029, 0x1010), core.VROB, imageCSA),
		core.NewRawElement(tagPixelData, core.VROW, pixels),
	}
	if f.acqMatrix != nil {
		elements = append(elements, us(tagAcquisitionMatrix, f.acqMatrix...), str(tagInPlanePhaseEncodingDirection, core.VRCS, f.phaseEncoding))
	}
	if f.protocol != "" {
		seriesCSA, err := siemens.EncodeCSA([]siemens.CSAElement{{Name: "MrPhoenixProtocol", VM: 1, VR: "UN", Items: 6, Values: []string{f.protocol}}})
		if err != nil {
			t.Fatal(err)
		}
		elements = append(elements, core.NewRawElement(core.NewTag(0x0029, 0x1020), core.VROB, seriesCSA))
	}
	dataset := object.FromElements(elements, std.Dictionary)
	file := &object.File{Dataset: dataset, Meta: object.New(std.Dictionary), TransferSyntax: transfer.ExplicitVRLittleEndian}
	if err := file.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	return file
}

func mosaicOptions() MosaicOptions {
	return MosaicOptions{
		SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.980.4",
		SOPInstanceUIDForSlice: func(slice int) (string, error) {
			return fmt.Sprintf("1.2.826.0.1.3680043.10.1.980.5.%d", slice+1), nil
		},
	}
}

// expectedMosaicPosition is the corner of output slice k, computed directly
// from the definitions: mosaic corner, half the size difference along the
// row and column, then k spacings along row x column.
func expectedMosaicPosition(f mosaicFixture, k int, inverted bool) [3]float64 {
	normal := cross(mosaicRow, mosaicColumn)
	normal, _ = unit(normal)
	row, _ := unit(mosaicRow)
	column, _ := unit(mosaicColumn)
	var p [3]float64
	for i := range p {
		p[i] = mosaicIPP[i] + float64((f.grid-1)*f.tileColumns)/2*2.5*row[i] + float64((f.grid-1)*f.tileRows)/2*3*column[i] + float64(k)*mosaicSpacing*normal[i]
		if inverted {
			p[i] -= float64(f.images-1) * mosaicSpacing * normal[i]
		}
	}
	return p
}

func formatNormal(n [3]float64, sign float64) []string {
	return []string{strconv.FormatFloat(sign*n[0], 'g', -1, 64), strconv.FormatFloat(sign*n[1], 'g', -1, 64), strconv.FormatFloat(sign*n[2], 'g', -1, 64)}
}

func TestSplitMosaicTilesAndGeometry(t *testing.T) {
	normal, _ := unit(cross(mosaicRow, mosaicColumn))
	for _, inverted := range []bool{false, true} {
		sign := 1.0
		if inverted {
			sign = -1
		}
		f := mosaicFixture{images: 7, tileRows: 4, tileColumns: 5, grid: 3, normal: formatNormal(normal, sign)}
		outputs, report, err := SplitMosaic(context.Background(), mosaicTestFile(t, f), mosaicOptions())
		if err != nil {
			t.Fatal(err)
		}
		g := report.Geometry
		if len(outputs) != 7 || g.Images != 7 || g.Grid != 3 || g.Rows != 4 || g.Columns != 5 || g.Inverted != inverted || g.ImagesSource != "csa" || len(g.Diagnostics) != 0 {
			t.Fatalf("inverted=%v: geometry %+v", inverted, g)
		}
		for k, file := range outputs {
			tile := k
			if inverted {
				tile = 6 - k
			}
			ds := file.Dataset
			raw, _ := ds.GetRaw(tagPixelData)
			if len(raw) != 4*5*2 || binary.LittleEndian.Uint16(raw) != uint16(tile*1000) || binary.LittleEndian.Uint16(raw[len(raw)-2:]) != uint16(tile*1000+3*32+4) {
				t.Fatalf("inverted=%v slice %d: pixels of the wrong tile", inverted, k)
			}
			position, _ := ds.GetFloats(tagImagePositionPatient)
			want := expectedMosaicPosition(f, k, inverted)
			for i := range 3 {
				if math.Abs(position[i]-want[i]) > 1e-6 {
					t.Fatalf("inverted=%v slice %d: position %v, want %v", inverted, k, position, want)
				}
			}
			rows, _ := uint16Value(ds, tagRows)
			number, _ := ds.GetInt(tagInstanceNumber)
			imageType, _ := ds.GetStrings(tagImageType)
			if rows != 4 || number != int64(k+1) || slices.Contains(imageType, "MOSAIC") || ds.Has(core.NewTag(0x0029, 0x1010)) || ds.Has(core.NewTag(0x0028, 0x0107)) {
				t.Fatalf("slice %d attributes: rows %d number %d type %v", k, rows, number, imageType)
			}
			if series, _ := ds.GetString(tagSeriesInstanceUID); series != "1.2.826.0.1.3680043.10.1.980.4" {
				t.Fatalf("series %q", series)
			}
			assertPart10RoundTrip(t, file)
		}
		closeFiles(outputs)
	}
}

func TestSplitMosaicProtocolCheckAndDiagnostics(t *testing.T) {
	normal, _ := unit(cross(mosaicRow, mosaicColumn))
	f := mosaicFixture{images: 4, tileRows: 4, tileColumns: 4, grid: 2, normal: formatNormal(normal, 1)}
	// Slice centres, with slice 2 optionally moved 1 mm along x.
	protocol := func(shift float64) string {
		var b strings.Builder
		b.WriteString("### ASCCONV BEGIN ###\nsSliceArray.lSize = 4\n")
		for k := range 4 {
			p := expectedMosaicPosition(f, k, false)
			for i := range 3 {
				p[i] += 2*2.5*mosaicRow[i] + 2*3*mosaicColumn[i]
			}
			if k == 2 {
				p[0] += shift
			}
			fmt.Fprintf(&b, "sSliceArray.asSlice[%d].sPosition.dSag = %.9f\nsSliceArray.asSlice[%d].sPosition.dCor = %.9f\nsSliceArray.asSlice[%d].sPosition.dTra = %.9f\n", k, p[0], k, p[1], k, p[2])
		}
		b.WriteString("### ASCCONV END ###\n")
		return b.String()
	}
	f.protocol = protocol(0)
	g, err := MosaicGeometryOf(mosaicTestFile(t, f).Dataset, mosaicOptions())
	if err != nil || !g.ProtocolChecked || len(g.Diagnostics) != 0 {
		t.Fatalf("consistent protocol: %+v, %v", g, err)
	}
	f.protocol = protocol(1)
	g, _ = MosaicGeometryOf(mosaicTestFile(t, f).Dataset, mosaicOptions())
	if !slices.Contains(g.Diagnostics, DiagnosticMosaicProtocolPositionMismatch) {
		t.Fatalf("mismatch not reported: %v", g.Diagnostics)
	}

	f = mosaicFixture{images: 4, tileRows: 4, tileColumns: 4, grid: 2}
	g, _ = MosaicGeometryOf(mosaicTestFile(t, f).Dataset, mosaicOptions())
	if !slices.Equal(g.Diagnostics, []string{DiagnosticMosaicSliceNormalMissing}) {
		t.Fatalf("missing normal: %v", g.Diagnostics)
	}
	f.normal = []string{"1", "0", "0"}
	g, _ = MosaicGeometryOf(mosaicTestFile(t, f).Dataset, mosaicOptions())
	if !slices.Equal(g.Diagnostics, []string{DiagnosticMosaicSliceNormalInconsistent}) || g.Inverted {
		t.Fatalf("inconsistent normal: %v", g.Diagnostics)
	}
}

func TestSplitMosaicSliceCountFallbacks(t *testing.T) {
	f := mosaicFixture{images: 4, tileRows: 4, tileColumns: 4, grid: 2, omitImages: true, acqMatrix: []uint16{4, 0, 0, 4}, phaseEncoding: "COL"}
	file := mosaicTestFile(t, f)
	if _, err := MosaicGeometryOf(file.Dataset, mosaicOptions()); !errors.Is(err, ErrNonconformant) {
		t.Fatalf("strict: %v", err)
	}
	opts := mosaicOptions()
	opts.ImagesFromAcquisitionMatrix = true
	g, err := MosaicGeometryOf(file.Dataset, opts)
	if err != nil || g.Images != 4 || g.ImagesSource != "acquisition-matrix" || !slices.Contains(g.Diagnostics, DiagnosticMosaicImagesFromAcquisitionMatrix) {
		t.Fatalf("fallback: %+v, %v", g, err)
	}
	// (0019,xx0A) of "SIEMENS MR HEADER".
	file.Dataset.Put(str(core.NewTag(0x0019, 0x0010), core.VRLO, "SIEMENS MR HEADER"))
	file.Dataset.Put(us(core.NewTag(0x0019, 0x100a), 3))
	g, err = MosaicGeometryOf(file.Dataset, mosaicOptions())
	if err != nil || g.Images != 3 || g.ImagesSource != "mr-header" {
		t.Fatalf("mr header: %+v, %v", g, err)
	}
}

func TestSplitMosaicRejectsNonMosaicsAndBadGrids(t *testing.T) {
	f := mosaicFixture{images: 4, tileRows: 4, tileColumns: 4, grid: 2}
	file := mosaicTestFile(t, f)
	file.Dataset.Put(stringsElement(tagImageType, core.VRCS, []string{"ORIGINAL", "PRIMARY"}))
	if _, _, err := SplitMosaic(context.Background(), file, mosaicOptions()); !errors.Is(err, ErrNotMosaic) {
		t.Fatalf("non-mosaic: %v", err)
	}
	file = mosaicTestFile(t, f)
	file.Dataset.Put(us(tagColumns, 9))
	if _, err := MosaicGeometryOf(file.Dataset, mosaicOptions()); !errors.Is(err, ErrNonconformant) {
		t.Fatalf("bad grid: %v", err)
	}
	opts := mosaicOptions()
	opts.SOPInstanceUIDForSlice = func(int) (string, error) { return "1.2.3", nil }
	if _, _, err := SplitMosaic(context.Background(), mosaicTestFile(t, f), opts); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("duplicate UIDs: %v", err)
	}
}

// gdcmMosaicSlices is what GDCM 3.2.5 SplitMosaicFilter computes for the
// gdcmData SIEMENS_MOSAIC_12BitsStored-16BitsJPEG.dcm (the oracle program is
// testdata/gdcm_mosaic_oracle.cxx): origin, z spacing and the FNV-1a 64 hash
// of each slice.
var (
	gdcmMosaicOrigin  = [3]float64{-106.4215371764, -97.8146522218, -62.5020451471}
	gdcmMosaicSpacing = 6.1250000081
	gdcmMosaicHashes  = []uint64{
		0x940c1d64be7f9b47, 0x94ab623fceaf7054, 0xe71acc5289878951, 0xc64a1a3da9c4ccc0,
		0x80b4650b262cbcc5, 0xab99648977958031, 0x024e56044d06bef5, 0x93d099debcd1787b,
		0xcf73ef7c7b72bb50, 0x4d9dee7c4f3598b9, 0xb51125e674397ff4, 0x05fb3d763328cc96,
		0xe9e0939ef89b8046, 0xbb98c0310c7547e7, 0x831bd4ccf7b89480, 0xce7855ae9166aec4,
		0xeb65a91d071fcca9, 0x6e07b20d3b89cfa8,
	}
)

// TestSplitMosaicMatchesGDCM splits the gdcmData Siemens mosaic and compares
// slice count, geometry and pixels with GDCM. Enable with
// DICOMGO_GDCMDATA_ROOT.
func TestSplitMosaicMatchesGDCM(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("opt-in: DICOMGO_GDCMDATA_ROOT")
	}
	src, err := object.OpenFile(filepath.Join(root, "SIEMENS_MOSAIC_12BitsStored-16BitsJPEG.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	registry := pixeldata.NewMemoryRegistry()
	if err := builtin.Register(registry); err != nil {
		t.Fatal(err)
	}
	// The data set disagrees with its JPEG frame header; decompress with
	// the opt-in reconciliation first, as the gdcmData sweep does.
	native, err := pixeldata.DecompressFile(src, pixeldata.DecompressOptions{Registry: registry, Recovery: pixeldata.DecodeRecovery{ReconcileWithCodestream: true}})
	if err != nil {
		t.Fatal(err)
	}
	outputs, report, err := SplitMosaic(context.Background(), native, mosaicOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(outputs)
	g := report.Geometry
	if len(outputs) != 18 || g.Rows != 64 || g.Columns != 64 || g.Inverted {
		t.Fatalf("geometry %+v", g)
	}
	if math.Abs(g.SliceSpacing-gdcmMosaicSpacing) > 1e-6 {
		t.Fatalf("spacing %v, GDCM %v", g.SliceSpacing, gdcmMosaicSpacing)
	}
	for k, file := range outputs {
		position, _ := file.Dataset.GetFloats(tagImagePositionPatient)
		for i := range 3 {
			want := gdcmMosaicOrigin[i] + float64(k)*gdcmMosaicSpacing*g.Normal[i]
			if math.Abs(position[i]-want) > 1e-4 {
				t.Fatalf("slice %d position %v, GDCM %v", k, position, want)
			}
		}
		raw, _ := file.Dataset.GetRaw(tagPixelData)
		h := fnv.New64a()
		_, _ = h.Write(raw)
		if h.Sum64() != gdcmMosaicHashes[k] {
			t.Fatalf("slice %d pixels differ from GDCM", k)
		}
	}
}
