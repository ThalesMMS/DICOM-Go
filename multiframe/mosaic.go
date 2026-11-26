package multiframe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/siemens"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrNotMosaic reports a data set that is not a Siemens mosaic.
var ErrNotMosaic = errors.New("dicom/multiframe: not a Siemens mosaic")

// Mosaic diagnostics, reported in MosaicGeometry.Diagnostics.
const (
	// DiagnosticMosaicImagesFromAcquisitionMatrix: the slice count came from
	// the Acquisition Matrix (MosaicOptions.ImagesFromAcquisitionMatrix);
	// trailing tiles may be blank.
	DiagnosticMosaicImagesFromAcquisitionMatrix = "mosaic-images-from-acquisition-matrix"
	// DiagnosticMosaicCSACreatorMissing: the CSA header was read without its
	// private creator (MosaicOptions.Lookup.AllowMissingCreator).
	DiagnosticMosaicCSACreatorMissing = "mosaic-csa-creator-missing"
	// DiagnosticMosaicSliceNormalMissing: no CSA SliceNormalVector; slices are
	// assumed stored along row x column.
	DiagnosticMosaicSliceNormalMissing = "mosaic-slice-normal-missing"
	// DiagnosticMosaicSliceNormalInconsistent: SliceNormalVector is neither
	// parallel nor antiparallel to row x column and was ignored.
	DiagnosticMosaicSliceNormalInconsistent = "mosaic-slice-normal-inconsistent"
	// DiagnosticMosaicProtocolPositionMismatch: a slice centre differs from
	// the ASCCONV sSliceArray position by more than 0.01 mm.
	DiagnosticMosaicProtocolPositionMismatch = "mosaic-protocol-position-mismatch"
)

// MosaicGeometry describes how a Siemens mosaic splits into slices. Row and
// column vectors are Image Orientation (Patient); Positions are the Image
// Position (Patient) of each output slice and TileOrder the mosaic tile
// (row-major, zero-based) it comes from. Output slices are ordered along
// Normal = row x column.
type MosaicGeometry struct {
	Images          int
	Grid            int
	Rows, Columns   int
	ImagesSource    string
	Row, Column     [3]float64
	Normal          [3]float64
	PixelSpacing    [2]float64
	SliceSpacing    float64
	Inverted        bool
	Positions       [][3]float64
	TileOrder       []int
	ProtocolChecked bool
	Diagnostics     []string
	sourceRows      int
	sourceColumns   int
}

// MosaicOptions controls SplitMosaic. SeriesInstanceUID and
// SOPInstanceUIDForSlice are required; slice indices are zero-based in
// output order.
type MosaicOptions struct {
	SeriesInstanceUID      string
	SOPInstanceUIDForSlice func(slice int) (string, error)
	DecoderRegistry        pixeldata.Registry
	Limits                 Limits
	// KeepPrivate keeps private attributes, which are removed by default
	// because the CSA headers describe the mosaic, not the slice.
	KeepPrivate bool
	// ImagesFromAcquisitionMatrix derives the slice count from Acquisition
	// Matrix (0018,1310) and In-plane Phase Encoding Direction when neither
	// the CSA header nor (0019,xx0A) gives it.
	ImagesFromAcquisitionMatrix bool
	Lookup                      siemens.LookupOptions
}

// MosaicReport describes a successful split.
type MosaicReport struct {
	Geometry    MosaicGeometry
	OutputBytes int64
}

// MosaicGeometryOf computes the split geometry of a Siemens mosaic from its
// attributes and CSA headers, without reading Pixel Data.
func MosaicGeometryOf(dataset *object.Object, opts MosaicOptions) (MosaicGeometry, error) {
	if !siemens.IsMosaic(dataset) {
		return MosaicGeometry{}, ErrNotMosaic
	}
	var g MosaicGeometry
	rows, ok1 := uint16Value(dataset, tagRows)
	columns, ok2 := uint16Value(dataset, tagColumns)
	if !ok1 || !ok2 || rows == 0 || columns == 0 {
		return g, fmt.Errorf("%w: Rows and Columns are required", ErrNonconformant)
	}
	g.sourceRows, g.sourceColumns = int(rows), int(columns)

	if header, err := siemens.ImageHeader(dataset, opts.Lookup); err == nil && header.CreatorMissing {
		g.Diagnostics = append(g.Diagnostics, DiagnosticMosaicCSACreatorMissing)
	}
	images, source, err := siemens.NumberOfImagesInMosaic(dataset, opts.Lookup)
	if err != nil {
		if !opts.ImagesFromAcquisitionMatrix {
			return g, fmt.Errorf("%w: %w", ErrNonconformant, err)
		}
		images, err = imagesFromAcquisitionMatrix(dataset, g.sourceRows, g.sourceColumns)
		if err != nil {
			return g, err
		}
		source = "acquisition-matrix"
		g.Diagnostics = append(g.Diagnostics, DiagnosticMosaicImagesFromAcquisitionMatrix)
	}
	if images < 1 || images > g.sourceRows*g.sourceColumns {
		return g, fmt.Errorf("%w: NumberOfImagesInMosaic %d", ErrNonconformant, images)
	}
	g.Images, g.ImagesSource = images, source
	g.Grid = 1
	for g.Grid*g.Grid < images {
		g.Grid++
	}
	if g.sourceRows%g.Grid != 0 || g.sourceColumns%g.Grid != 0 {
		return g, fmt.Errorf("%w: %dx%d mosaic is not a %dx%d grid", ErrNonconformant, g.sourceColumns, g.sourceRows, g.Grid, g.Grid)
	}
	g.Rows, g.Columns = g.sourceRows/g.Grid, g.sourceColumns/g.Grid

	orientation, err := dataset.GetFloats(tagImageOrientationPatient)
	if err != nil || len(orientation) != 6 {
		return g, fmt.Errorf("%w: Image Orientation (Patient) is required", ErrNonconformant)
	}
	position, err := dataset.GetFloats(tagImagePositionPatient)
	if err != nil || len(position) != 3 {
		return g, fmt.Errorf("%w: Image Position (Patient) is required", ErrNonconformant)
	}
	row, column, values, err := dataset.SpacingPair(tagPixelSpacing)
	if err != nil || values == object.SpacingValuesAbsent {
		return g, fmt.Errorf("%w: Pixel Spacing is required", ErrNonconformant)
	}
	g.PixelSpacing = [2]float64{row, column}
	var ok3 bool
	g.Row, ok1 = unit([3]float64{orientation[0], orientation[1], orientation[2]})
	g.Column, ok2 = unit([3]float64{orientation[3], orientation[4], orientation[5]})
	g.Normal, ok3 = unit(cross(g.Row, g.Column))
	if !ok1 || !ok2 || !ok3 {
		return g, fmt.Errorf("%w: degenerate Image Orientation (Patient)", ErrNonconformant)
	}
	g.SliceSpacing = positiveFloat(dataset, tagSpacingBetweenSlices)
	if g.SliceSpacing == 0 {
		g.SliceSpacing = positiveFloat(dataset, tagSliceThickness)
	}
	if g.SliceSpacing == 0 && images > 1 {
		return g, fmt.Errorf("%w: Spacing Between Slices or Slice Thickness is required", ErrNonconformant)
	}

	if normal, err := siemens.SliceNormalVector(dataset, opts.Lookup); err != nil {
		g.Diagnostics = append(g.Diagnostics, DiagnosticMosaicSliceNormalMissing)
	} else if n, ok := unit(normal); !ok {
		g.Diagnostics = append(g.Diagnostics, DiagnosticMosaicSliceNormalInconsistent)
	} else {
		switch d := dot(n, g.Normal); {
		case d > 1-1e-4:
		case d < -1+1e-4:
			g.Inverted = true
		default:
			g.Diagnostics = append(g.Diagnostics, DiagnosticMosaicSliceNormalInconsistent)
		}
	}

	// The mosaic position is the corner of the whole mosaic; the first tile
	// sits in its centre block, half the size difference away.
	var origin [3]float64
	for i := range origin {
		origin[i] = position[i] +
			float64(g.sourceColumns-g.Columns)/2*g.PixelSpacing[1]*g.Row[i] +
			float64(g.sourceRows-g.Rows)/2*g.PixelSpacing[0]*g.Column[i]
		if g.Inverted {
			origin[i] -= g.Normal[i] * g.SliceSpacing * float64(images-1)
		}
	}
	g.Positions = make([][3]float64, images)
	g.TileOrder = make([]int, images)
	for k := range g.Positions {
		for i := range origin {
			g.Positions[k][i] = origin[i] + float64(k)*g.SliceSpacing*g.Normal[i]
		}
		g.TileOrder[k] = k
		if g.Inverted {
			g.TileOrder[k] = images - 1 - k
		}
	}
	g.checkProtocol(dataset, opts.Lookup)
	return g, nil
}

// checkProtocol compares each tile centre with its ASCCONV slice position.
func (g *MosaicGeometry) checkProtocol(dataset *object.Object, lookup siemens.LookupOptions) {
	protocol, err := siemens.SeriesProtocol(dataset, lookup)
	if err != nil {
		return
	}
	slices, err := protocol.SliceArray()
	if err != nil || len(slices) != g.Images {
		return
	}
	g.ProtocolChecked = true
	for k, tile := range g.TileOrder {
		var distance float64
		for i := range 3 {
			centre := g.Positions[k][i] + float64(g.Columns)/2*g.PixelSpacing[1]*g.Row[i] + float64(g.Rows)/2*g.PixelSpacing[0]*g.Column[i]
			delta := centre - slices[tile].Position[i]
			distance += delta * delta
		}
		if math.Sqrt(distance) > 0.01 {
			g.Diagnostics = append(g.Diagnostics, DiagnosticMosaicProtocolPositionMismatch)
			return
		}
	}
}

// imagesFromAcquisitionMatrix derives the tile count from the acquired
// matrix, as columns x rows of tiles.
func imagesFromAcquisitionMatrix(dataset *object.Object, rows, columns int) (int, error) {
	matrix, ok := siemens.Uint16s(dataset, tagAcquisitionMatrix)
	if !ok || len(matrix) != 4 {
		return 0, fmt.Errorf("%w: Acquisition Matrix is required to derive the slice count", ErrNonconformant)
	}
	direction, _ := dataset.GetString(tagInPlanePhaseEncodingDirection)
	var tileColumns, tileRows int64
	switch strings.TrimSpace(direction) {
	case "COL":
		tileColumns, tileRows = int64(matrix[3]), int64(matrix[0])
	case "ROW":
		tileColumns, tileRows = int64(matrix[1]), int64(matrix[2])
	}
	if tileColumns <= 0 || tileRows <= 0 {
		return 0, fmt.Errorf("%w: unusable Acquisition Matrix", ErrNonconformant)
	}
	if int64(columns)%tileColumns != 0 || int64(rows)%tileRows != 0 {
		size := max(tileColumns, tileRows)
		if int64(columns)%size != 0 || int64(rows)%size != 0 {
			return 0, fmt.Errorf("%w: Acquisition Matrix does not tile the mosaic", ErrNonconformant)
		}
		tileColumns, tileRows = size, size
	}
	return int(int64(columns) / tileColumns * (int64(rows) / tileRows)), nil
}

// SplitMosaic splits a Siemens mosaic into one classic single-frame image
// per slice with its own geometry. The source is not mutated and the result
// is atomic. Output is Explicit VR Little Endian with native Pixel Data;
// "MOSAIC" is removed from Image Type, and Slice Location is the position
// along the slice normal.
func SplitMosaic(ctx context.Context, src *object.File, opts MosaicOptions) ([]*object.File, MosaicReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limits, err := normalizeLimits(opts.Limits)
	if err != nil {
		return nil, MosaicReport{}, wrap("options", -1, err)
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	if src == nil || src.Dataset == nil {
		return nil, MosaicReport{}, wrap("source", -1, fmt.Errorf("%w: file and dataset are required", ErrInvalidSource))
	}
	sourceSOPUID, err := requiredUID(src.Dataset, tagSOPInstanceUID)
	if err != nil {
		return nil, MosaicReport{}, wrap("source", -1, err)
	}
	sourceClassUID, err := requiredUID(src.Dataset, tagSOPClassUID)
	if err != nil {
		return nil, MosaicReport{}, wrap("source", -1, err)
	}
	seriesUID := strings.TrimSpace(opts.SeriesInstanceUID)
	if !core.IsValidUID(seriesUID) {
		return nil, MosaicReport{}, wrap("options", -1, fmt.Errorf("%w: SeriesInstanceUID must be valid", ErrInvalidOptions))
	}
	if opts.SOPInstanceUIDForSlice == nil {
		return nil, MosaicReport{}, wrap("options", -1, fmt.Errorf("%w: SOPInstanceUIDForSlice is required", ErrInvalidOptions))
	}
	geometry, err := MosaicGeometryOf(src.Dataset, opts)
	if err != nil {
		return nil, MosaicReport{}, wrap("geometry", -1, err)
	}
	if geometry.Images > limits.MaxFrames {
		return nil, MosaicReport{}, wrap("geometry", -1, fmt.Errorf("%w: %d slices", ErrResourceLimit, geometry.Images))
	}

	transcoded, _, err := pixeldata.TranscodeFile(ctx, src, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{DecoderRegistry: opts.DecoderRegistry, Limits: limits})
	if err != nil {
		if errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
			err = errors.Join(ErrResourceLimit, err)
		}
		return nil, MosaicReport{}, wrap("transcode", -1, err)
	}
	defer func() { _ = transcoded.Close() }()
	frames, err := pixeldata.ExtractNativeFramesView(transcoded.Dataset)
	if err != nil {
		return nil, MosaicReport{}, wrap("pixel_data", -1, err)
	}
	metadata := frames.Metadata
	if len(frames.Data) != 1 || metadata.SamplesPerPixel != 1 || (metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16) {
		return nil, MosaicReport{}, wrap("pixel_data", -1, fmt.Errorf("%w: a mosaic must be one frame of 8- or 16-bit monochrome samples", ErrNonconformant))
	}
	sampleBytes := int(metadata.BitsAllocated / 8)
	mosaic := frames.Data[0]
	if len(mosaic) < geometry.sourceRows*geometry.sourceColumns*sampleBytes {
		return nil, MosaicReport{}, wrap("pixel_data", -1, fmt.Errorf("%w: Pixel Data is shorter than the mosaic", ErrNonconformant))
	}

	uids := make(map[string]struct{}, geometry.Images)
	sliceUIDs := make([]string, geometry.Images)
	for k := range sliceUIDs {
		uid, uidErr := opts.SOPInstanceUIDForSlice(k)
		uid = strings.TrimSpace(uid)
		if uidErr != nil {
			return nil, MosaicReport{}, wrap("uid", k, uidErr)
		}
		if _, duplicate := uids[uid]; !core.IsValidUID(uid) || uid == sourceSOPUID || duplicate {
			return nil, MosaicReport{}, wrap("uid", k, fmt.Errorf("%w: generated SOP Instance UID is invalid, repeated or equals the source", ErrInvalidOptions))
		}
		uids[uid] = struct{}{}
		sliceUIDs[k] = uid
	}

	budget := conversionBudget{ctx: ctx, maxElements: limits.MaxElements, maxDepth: limits.MaxDepth}
	outputs := make([]*object.File, 0, geometry.Images)
	closeOutputs := func() {
		for _, file := range outputs {
			_ = file.Close()
		}
	}
	var outputBytes int64
	for k := range geometry.Images {
		if err := ctx.Err(); err != nil {
			closeOutputs()
			return nil, MosaicReport{}, err
		}
		tile := tilePixels(mosaic, geometry, geometry.TileOrder[k], sampleBytes)
		dataset, buildErr := buildMosaicSlice(&budget, transcoded.Dataset, geometry, k, tile, metadata.BitsAllocated, seriesUID, sliceUIDs[k], sourceClassUID, sourceSOPUID, opts.KeepPrivate)
		if buildErr != nil {
			closeOutputs()
			return nil, MosaicReport{}, wrap("slice", k, buildErr)
		}
		file := &object.File{Dataset: dataset, Meta: object.New(std.Dictionary), TransferSyntax: transfer.ExplicitVRLittleEndian}
		if err := file.RebuildFileMeta(); err != nil {
			closeOutputs()
			return nil, MosaicReport{}, wrap("file_meta", k, err)
		}
		counter := &limitWriter{ctx: ctx, remaining: limits.MaxOutputBytes - outputBytes}
		if err := object.WriteFile(counter, file); err != nil {
			closeOutputs()
			if errors.Is(err, ErrResourceLimit) {
				return nil, MosaicReport{}, wrap("output", k, err)
			}
			return nil, MosaicReport{}, wrap("write_validation", k, err)
		}
		outputBytes += counter.written
		outputs = append(outputs, file)
	}
	return outputs, MosaicReport{Geometry: geometry, OutputBytes: outputBytes}, nil
}

// tilePixels copies tile index (row-major in the grid) out of the mosaic.
func tilePixels(mosaic []byte, g MosaicGeometry, tile, sampleBytes int) []byte {
	out := make([]byte, g.Rows*g.Columns*sampleBytes)
	x0 := (tile % g.Grid) * g.Columns
	y0 := (tile / g.Grid) * g.Rows
	rowBytes := g.Columns * sampleBytes
	for y := range g.Rows {
		start := ((y0+y)*g.sourceColumns + x0) * sampleBytes
		copy(out[y*rowBytes:(y+1)*rowBytes], mosaic[start:start+rowBytes])
	}
	return out
}

func buildMosaicSlice(budget *conversionBudget, source *object.Object, g MosaicGeometry, k int, pixels []byte, bitsAllocated uint16, seriesUID, sopUID, sourceClassUID, sourceSOPUID string, keepPrivate bool) (*object.Object, error) {
	elements := make([]core.Element, 0, source.Len())
	for _, element := range source.Elements() {
		tag := element.Tag()
		if _, drop := mosaicDroppedTags[tag]; drop || (tag.IsPrivate() && !keepPrivate) {
			continue
		}
		clone, err := budget.cloneElement(element, 1)
		if err != nil {
			return nil, err
		}
		elements = append(elements, clone)
	}
	dataset := object.FromElements(elements, std.Dictionary)
	dataset.SetValueByteOrder(nil)

	if imageType, ok := source.GetStrings(tagImageType); ok {
		kept := make([]string, 0, len(imageType))
		for _, value := range imageType {
			if !strings.EqualFold(strings.TrimSpace(value), "MOSAIC") {
				kept = append(kept, value)
			}
		}
		dataset.Put(stringsElement(tagImageType, core.VRCS, kept))
	}
	dataset.Put(ui(tagSOPInstanceUID, sopUID))
	dataset.Put(ui(tagSeriesInstanceUID, seriesUID))
	dataset.Put(str(tagInstanceNumber, core.VRIS, strconv.Itoa(k+1)))
	dataset.Put(derivedio.US(tagRows, uint16(g.Rows)))
	dataset.Put(derivedio.US(tagColumns, uint16(g.Columns)))
	position := g.Positions[k]
	dataset.Put(derivedio.DS(tagImagePositionPatient, position[0], position[1], position[2]))
	dataset.Put(derivedio.DS(tagSliceLocation, dot(position, g.Normal)))

	pixelVR := core.VROB
	if bitsAllocated > 8 {
		pixelVR = core.VROW
	}
	if len(pixels)%2 != 0 {
		pixels = append(pixels, 0)
	}
	dataset.Put(core.NewRawElement(tagPixelData, pixelVR, pixels))
	if err := appendSourceReference(dataset, derivation.Source{SOPClassUID: sourceClassUID, SOPInstanceUID: sourceSOPUID}); err != nil {
		return nil, err
	}
	if !dataset.Has(tagDerivationDescription) {
		dataset.Put(str(tagDerivationDescription, core.VRST, fmt.Sprintf("Slice %d of %d split from a Siemens mosaic", g.TileOrder[k]+1, g.Images)))
	}
	return dataset, nil
}

var (
	tagSliceThickness                = core.NewTag(0x0018, 0x0050)
	tagSpacingBetweenSlices          = core.NewTag(0x0018, 0x0088)
	tagAcquisitionMatrix             = core.NewTag(0x0018, 0x1310)
	tagInPlanePhaseEncodingDirection = core.NewTag(0x0018, 0x1312)
	tagSliceLocation                 = core.NewTag(0x0020, 0x1041)
)

// mosaicDroppedTags describe the whole mosaic and would be wrong for a
// slice.
var mosaicDroppedTags = map[core.Tag]struct{}{
	tagNumberOfFrames:           {},
	core.NewTag(0x0028, 0x0106): {}, // Smallest Image Pixel Value
	core.NewTag(0x0028, 0x0107): {}, // Largest Image Pixel Value
	tagPixelData:                {},
}

func positiveFloat(dataset *object.Object, tag core.Tag) float64 {
	value, err := dataset.GetFloat(tag)
	if err != nil || !(value > 0) || math.IsInf(value, 0) {
		return 0
	}
	return value
}

func cross(a, b [3]float64) [3]float64 {
	return [3]float64{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0]}
}

func dot(a, b [3]float64) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

func unit(v [3]float64) ([3]float64, bool) {
	n := math.Sqrt(dot(v, v))
	if !(n > 1e-9) || math.IsInf(n, 0) {
		return v, false
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}, true
}
