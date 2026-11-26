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
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrNotUIHGrid means the VFRAME marker or UIH private creator is absent.
var ErrNotUIHGrid = errors.New("dicom/multiframe: not a UIH grid image")

const uihCreator = "Image Private Header"
const uihGeometryTolerance = 1e-4

// UIHGridOptions controls SplitUIHGrid. Identities must differ from the source;
// the callback receives the zero-based row-major tile index. Private attributes
// are removed because their semantics describe the grid, not the output slice.
type UIHGridOptions struct {
	SeriesInstanceUID      string
	SOPInstanceUIDForSlice func(slice int) (string, error)
	DecoderRegistry        pixeldata.Registry
	Limits                 Limits
}

// UIHGridGeometry retains each tile's explicit position and orientation. Tiles
// are row-major, with GridColumns=ceil(sqrt(Images)) and the required GridRows.
// Positions are not reconstructed from the grid origin, normal or spacing.
type UIHGridGeometry struct {
	Images, GridRows, GridColumns int
	Rows, Columns                 int
	Normal                        [3]float64
	PrivateNormal                 [3]float64
	PixelSpacing                  [2]float64
	Positions                     [][3]float64
	Orientations                  [][6]float64
	sourceRows, sourceColumns     int
}

// UIHGridReport contains detached geometry and aggregate encoded output bytes.
type UIHGridReport struct {
	Geometry    UIHGridGeometry
	OutputBytes int64
}

// UIHGridGeometryOf validates the known UIH VFRAME layout without reading Pixel
// Data. Limits apply before geometry allocations. Unsupported or ambiguous
// geometry is an error; no Siemens-style origin correction is applied.
func UIHGridGeometryOf(dataset *object.Object, opts UIHGridOptions) (UIHGridGeometry, error) {
	limits, err := normalizeLimits(opts.Limits)
	if err != nil {
		return UIHGridGeometry{}, err
	}
	return uihGridGeometry(context.Background(), dataset, limits)
}

func uihGridGeometry(ctx context.Context, dataset *object.Object, limits Limits) (UIHGridGeometry, error) {
	fail := func(reason string) (UIHGridGeometry, error) {
		return UIHGridGeometry{}, fmt.Errorf("%w: UIH %s", ErrNonconformant, reason)
	}
	if dataset == nil {
		return UIHGridGeometry{}, ErrNotUIHGrid
	}
	if err := ctx.Err(); err != nil {
		return UIHGridGeometry{}, err
	}
	if dataset.Len() > limits.MaxElements {
		return UIHGridGeometry{}, ErrResourceLimit
	}
	imageType, err := uihText(dataset, tagImageType, core.VRCS, 128)
	if err != nil || len(imageType) < 2 || strings.TrimSpace(imageType[len(imageType)-1]) != "VFRAME" {
		return UIHGridGeometry{}, ErrNotUIHGrid
	}
	countTag, err := dataset.PrivateTag(0x0065, uihCreator, 0x50)
	if errors.Is(err, dictionary.ErrPrivateCreatorMissing) {
		return UIHGridGeometry{}, ErrNotUIHGrid
	}
	if err != nil {
		return fail("private reservation is invalid or ambiguous")
	}
	classes, err := uihText(dataset, tagSOPClassUID, core.VRUI, 64)
	if err != nil || len(classes) != 1 || strings.TrimSpace(classes[0]) != MRImageStorageUID {
		return UIHGridGeometry{}, ErrUnsupportedSOPClass
	}
	counts, err := uihPrivateFloats(dataset, countTag, 1)
	if err != nil || counts[0] < 1 || counts[0] != math.Trunc(counts[0]) {
		return fail("slice count must be a positive integer DS")
	}
	if counts[0] > float64(limits.MaxFrames) {
		return UIHGridGeometry{}, ErrResourceLimit
	}
	rows, okRows := uihDimension(dataset, tagRows)
	columns, okColumns := uihDimension(dataset, tagColumns)
	if !okRows || !okColumns || rows == 0 || columns == 0 {
		return fail("Rows and Columns are required")
	}
	sourcePixels := uint64(rows) * uint64(columns)
	if counts[0] > float64(sourcePixels) {
		return fail("slice count exceeds pixel count")
	}
	images := int(counts[0]) // sourcePixels bounds this before narrowing
	if sourcePixels > limits.MaxPixels {
		return UIHGridGeometry{}, ErrResourceLimit
	}
	if dataset.Has(tagSharedFunctionalGroupsSequence) || dataset.Has(tagPerFrameFunctionalGroupsSequence) {
		return fail("functional groups are unsupported in this classic grid layout")
	}
	if dataset.Has(tagNumberOfFrames) {
		values, err := uihText(dataset, tagNumberOfFrames, core.VRIS, 12)
		if err != nil || len(values) != 1 {
			return fail("source must contain one grid frame")
		}
		count, err := strconv.Atoi(strings.TrimSpace(values[0]))
		if err != nil || count != 1 {
			return fail("source must contain one grid frame")
		}
	}
	for _, e := range dataset.Elements() {
		if e.Tag().Group >= 0x6000 && e.Tag().Group <= 0x60ff {
			return fail("grid-relative overlays are unsupported")
		}
	}
	g := UIHGridGeometry{Images: images, sourceRows: int(rows), sourceColumns: int(columns)}
	for g.GridColumns = 1; g.GridColumns < (images-1)/g.GridColumns+1; g.GridColumns++ {
	}
	g.GridRows = (images-1)/g.GridColumns + 1
	if int(rows)%g.GridRows != 0 || int(columns)%g.GridColumns != 0 {
		return fail("dimensions do not divide the tile grid")
	}
	g.Rows, g.Columns = int(rows)/g.GridRows, int(columns)/g.GridColumns
	iop, err := uihOrientation(dataset)
	if err != nil {
		return fail("Image Orientation (Patient) must be orthonormal")
	}
	row, _ := unit([3]float64{iop[0], iop[1], iop[2]})
	column, _ := unit([3]float64{iop[3], iop[4], iop[5]})
	g.Normal, _ = unit(cross(row, column))
	spacing, err := uihFloats(dataset, tagPixelSpacing, 2)
	if err != nil || spacing[0] <= 0 || spacing[1] <= 0 {
		return fail("Pixel Spacing must be a positive pair")
	}
	g.PixelSpacing = [2]float64{spacing[0], spacing[1]}
	normalTag, _ := dataset.PrivateTag(0x0065, uihCreator, 0x14)
	normal, err := uihPrivateFloats(dataset, normalTag, 3)
	if err != nil {
		return fail("private slice normal is required")
	}
	g.PrivateNormal = [3]float64{normal[0], normal[1], normal[2]}
	if math.Abs(dot(g.PrivateNormal, g.PrivateNormal)-1) > uihGeometryTolerance || math.Abs(math.Abs(dot(g.PrivateNormal, g.Normal))-1) > uihGeometryTolerance {
		return fail("private slice normal disagrees with orientation")
	}
	frameTag, _ := dataset.PrivateTag(0x0065, uihCreator, 0x51)
	element, ok := dataset.Get(frameTag)
	sequence, isSequence := element.Value.(core.SequenceValue)
	if !ok || (element.VR() != core.VRSQ && element.VR() != core.VRUN) || !isSequence || len(sequence.Items) != images {
		return fail("frame sequence must contain one item per tile")
	}
	if limits.MaxDepth < 2 {
		return UIHGridGeometry{}, ErrResourceLimit
	}
	visits := dataset.Len() + images
	if visits > limits.MaxElements {
		return UIHGridGeometry{}, ErrResourceLimit
	}
	for _, item := range sequence.Items {
		if err := ctx.Err(); err != nil {
			return UIHGridGeometry{}, err
		}
		if len(item.Elements) > limits.MaxElements-visits {
			return UIHGridGeometry{}, ErrResourceLimit
		}
		visits += len(item.Elements)
	}
	var advertised float64
	for _, tag := range []core.Tag{tagSpacingBetweenSlices, tagSliceThickness} {
		if dataset.Has(tag) {
			v, err := uihFloats(dataset, tag, 1)
			if err != nil || v[0] <= 0 {
				return fail("slice thickness/spacing must be positive when present")
			}
			if tag == tagSpacingBetweenSlices {
				advertised = v[0]
			}
		}
	}
	if int64(images) > limits.MaxOutputBytes/72 { // 3+6 float64 values per tile
		return UIHGridGeometry{}, ErrResourceLimit
	}
	g.Positions = make([][3]float64, images)
	g.Orientations = make([][6]float64, images)
	var previous, direction float64
	for k, item := range sequence.Items {
		if err := ctx.Err(); err != nil {
			return UIHGridGeometry{}, err
		}
		seen := map[core.Tag]bool{}
		for _, e := range item.Elements {
			if e.Tag() == tagImagePositionPatient || e.Tag() == tagImageOrientationPatient || e.Tag() == tagPixelSpacing {
				if seen[e.Tag()] {
					return fail("duplicate frame geometry attribute")
				}
				seen[e.Tag()] = true
			}
		}
		frame := object.FromElements(item.Elements, std.Dictionary)
		position, err := uihFloats(frame, tagImagePositionPatient, 3)
		if err != nil {
			return fail("every frame must have a finite Image Position (Patient)")
		}
		g.Positions[k] = [3]float64{position[0], position[1], position[2]}
		g.Orientations[k] = iop
		if frame.Has(tagImageOrientationPatient) {
			orientation, err := uihOrientation(frame)
			if err != nil {
				return fail("frame orientation is invalid")
			}
			for i := range orientation {
				if math.Abs(orientation[i]-iop[i]) > uihGeometryTolerance {
					return fail("frame orientation disagrees with common orientation")
				}
			}
			g.Orientations[k] = orientation
		}
		if frame.Has(tagPixelSpacing) {
			v, err := uihFloats(frame, tagPixelSpacing, 2)
			if err != nil || v[0] <= 0 || v[1] <= 0 || !uihClose(v[0], spacing[0]) || !uihClose(v[1], spacing[1]) {
				return fail("frame Pixel Spacing disagrees with common spacing")
			}
		}
		projection := dot(g.Positions[k], g.Normal)
		if math.IsNaN(projection) || math.IsInf(projection, 0) {
			return fail("frame projection is not finite")
		}
		if k > 0 {
			delta := projection - previous
			if math.IsNaN(delta) || math.IsInf(delta, 0) || math.Abs(delta) <= uihGeometryTolerance || (k > 1 && delta*direction <= 0) {
				return fail("frame positions must be distinct and monotonic along the normal")
			}
			if advertised > 0 && !uihClose(math.Abs(delta), advertised) {
				return fail("frame positions disagree with Spacing Between Slices")
			}
			direction = delta
		}
		previous = projection
	}
	return g, nil
}

func uihClose(a, b float64) bool {
	return math.Abs(a-b) <= uihGeometryTolerance*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func uihDimension(dataset *object.Object, tag core.Tag) (uint16, bool) {
	e, ok := dataset.Get(tag)
	if !ok || e.VR() != core.VRUS {
		return 0, false
	}
	switch v := e.Value.(type) {
	case core.Uint16Value:
		if len(v) != 1 {
			return 0, false
		}
	case core.RawValue:
		if len(v) != 2 {
			return 0, false
		}
	default:
		return 0, false
	}
	return uint16Value(dataset, tag)
}

func uihOrientation(dataset *object.Object) ([6]float64, error) {
	v, err := uihFloats(dataset, tagImageOrientationPatient, 6)
	if err != nil {
		return [6]float64{}, err
	}
	r, c := [3]float64{v[0], v[1], v[2]}, [3]float64{v[3], v[4], v[5]}
	if math.Abs(dot(r, r)-1) > uihGeometryTolerance || math.Abs(dot(c, c)-1) > uihGeometryTolerance || math.Abs(dot(r, c)) > uihGeometryTolerance {
		return [6]float64{}, ErrNonconformant
	}
	return [6]float64{v[0], v[1], v[2], v[3], v[4], v[5]}, nil
}

// Bound actual text before GetFloats/GetStrings allocates its VM result; do not
// trust an authored Header.Length. DS components have at most 16 bytes.
func uihText(dataset *object.Object, tag core.Tag, vr core.VR, maxBytes int) ([]string, error) {
	e, ok := dataset.Get(tag)
	if !ok || e.VR() != vr {
		return nil, ErrNonconformant
	}
	switch v := e.Value.(type) {
	case core.RawValue:
		if len(v) > maxBytes {
			return nil, ErrNonconformant
		}
	case core.StringValue:
		if len(v) > maxBytes {
			return nil, ErrNonconformant
		}
		n := 0
		for _, s := range v {
			if len(s) > maxBytes-n {
				return nil, ErrNonconformant
			}
			n += len(s)
		}
	default:
		return nil, ErrNonconformant
	}
	v, ok := dataset.GetStrings(tag)
	if !ok {
		return nil, ErrNonconformant
	}
	return v, nil
}

func uihFloats(dataset *object.Object, tag core.Tag, count int) ([]float64, error) {
	v, err := uihText(dataset, tag, core.VRDS, count*17)
	if err != nil || len(v) != count {
		return nil, ErrNonconformant
	}
	for _, text := range v {
		text = strings.Trim(text, " ")
		if len(text) == 0 || len(text) > 16 {
			return nil, ErrNonconformant
		}
		for _, char := range text {
			if (char < '0' || char > '9') && char != '+' && char != '-' && char != '.' && char != 'E' && char != 'e' {
				return nil, ErrNonconformant
			}
		}
	}
	floats, err := dataset.GetFloats(tag)
	if err != nil || len(floats) != count {
		return nil, ErrNonconformant
	}
	for _, v := range floats {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, ErrNonconformant
		}
	}
	return floats, nil
}

// An implicit-VR private DS can arrive as UN. The validated creator and logical
// offset establish its type; reinterpret only that bounded raw value locally.
func uihPrivateFloats(dataset *object.Object, tag core.Tag, count int) ([]float64, error) {
	e, ok := dataset.Get(tag)
	if !ok {
		return nil, ErrNonconformant
	}
	if e.VR() == core.VRUN {
		if _, ok := e.Value.(core.RawValue); !ok {
			return nil, ErrNonconformant
		}
		e.Header.VR = core.VRDS
		return uihFloats(object.FromElements([]core.Element{e}, std.Dictionary), tag, count)
	}
	return uihFloats(dataset, tag, count)
}

func uihRequiredUID(dataset *object.Object, tag core.Tag) (string, error) {
	v, err := uihText(dataset, tag, core.VRUI, 64)
	if err != nil || len(v) != 1 || !core.IsValidUID(strings.TrimSpace(v[0])) {
		return "", ErrNonconformant
	}
	return strings.TrimSpace(v[0]), nil
}

// SplitUIHGrid converts the known UIH MR grid layout into detached classic MR
// files with native Explicit VR Little Endian pixels. Source buffers are borrowed
// only for the duration of the call. On error it returns nil outputs/zero report.
// DecoderRegistry is required for compressed sources, as in TranscodeFile.
func SplitUIHGrid(ctx context.Context, src *object.File, opts UIHGridOptions) ([]*object.File, UIHGridReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limits, err := normalizeLimits(opts.Limits)
	if err != nil {
		return nil, UIHGridReport{}, wrap("options", -1, err)
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, UIHGridReport{}, err
	}
	if src == nil || src.Dataset == nil {
		return nil, UIHGridReport{}, wrap("source", -1, ErrInvalidSource)
	}
	g, err := uihGridGeometry(ctx, src.Dataset, limits)
	if err != nil {
		return nil, UIHGridReport{}, wrap("geometry", -1, err)
	}
	sourceSOPUID, err := uihRequiredUID(src.Dataset, tagSOPInstanceUID)
	if err != nil {
		return nil, UIHGridReport{}, wrap("source", -1, err)
	}
	sourceSeriesUID, err := uihRequiredUID(src.Dataset, tagSeriesInstanceUID)
	if err != nil {
		return nil, UIHGridReport{}, wrap("source", -1, err)
	}
	seriesUID := strings.TrimSpace(opts.SeriesInstanceUID)
	if !core.IsValidUID(seriesUID) || seriesUID == sourceSeriesUID || seriesUID == sourceSOPUID || opts.SOPInstanceUIDForSlice == nil {
		return nil, UIHGridReport{}, wrap("options", -1, ErrInvalidOptions)
	}
	transcoded, _, err := pixeldata.TranscodeFile(ctx, src, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{DecoderRegistry: opts.DecoderRegistry, Limits: limits})
	if err != nil {
		if errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
			err = errors.Join(ErrResourceLimit, err)
		}
		return nil, UIHGridReport{}, wrap("transcode", -1, err)
	}
	defer transcoded.Close()
	frames, err := pixeldata.ExtractNativeFramesView(transcoded.Dataset)
	if err != nil {
		return nil, UIHGridReport{}, wrap("pixel_data", -1, err)
	}
	m := frames.Metadata
	if len(frames.Data) != 1 || m.SamplesPerPixel != 1 || (m.BitsAllocated != 8 && m.BitsAllocated != 16) || (m.PhotometricInterpretation != "MONOCHROME1" && m.PhotometricInterpretation != "MONOCHROME2") {
		return nil, UIHGridReport{}, wrap("pixel_data", -1, ErrNonconformant)
	}
	sampleBytes := int(m.BitsAllocated / 8)
	tileBytes := int64(g.Rows) * int64(g.Columns) * int64(sampleBytes)
	// Count structural copies (including repeated/empty SQ items) and clone
	// payload bytes before making per-output arrays or copying any metadata.
	metadataNodes, metadataBytes, err := uihMetadataCost(ctx, transcoded.Dataset, limits)
	if err != nil {
		return nil, UIHGridReport{}, wrap("output", -1, err)
	}
	const derivedNodes = 32 // derived attributes, source reference and File Meta
	paddedTileBytes := tileBytes + tileBytes%2
	if metadataNodes > limits.MaxElements/g.Images-derivedNodes || metadataBytes > limits.MaxOutputBytes/int64(g.Images)-paddedTileBytes {
		return nil, UIHGridReport{}, wrap("output", -1, ErrResourceLimit)
	}
	uids := make(map[string]struct{}, g.Images)
	sliceUIDs := make([]string, g.Images)
	for k := range sliceUIDs {
		if err := ctx.Err(); err != nil {
			return nil, UIHGridReport{}, err
		}
		uid, err := opts.SOPInstanceUIDForSlice(k)
		uid = strings.TrimSpace(uid)
		if err != nil {
			return nil, UIHGridReport{}, wrap("uid", k, err)
		}
		if _, duplicate := uids[uid]; !core.IsValidUID(uid) || uid == sourceSOPUID || uid == sourceSeriesUID || uid == seriesUID || duplicate {
			return nil, UIHGridReport{}, wrap("uid", k, ErrInvalidOptions)
		}
		uids[uid], sliceUIDs[k] = struct{}{}, uid
	}
	budget := conversionBudget{ctx: ctx, maxElements: limits.MaxElements, maxDepth: limits.MaxDepth}
	outputs := make([]*object.File, 0, g.Images)
	success := false
	defer func() {
		if !success {
			for _, file := range outputs {
				_ = file.Close()
			}
		}
	}()
	var outputBytes int64
	for k := range g.Positions {
		if err := ctx.Err(); err != nil {
			return nil, UIHGridReport{}, err
		}
		tileGeometry := MosaicGeometry{Grid: g.GridColumns, Rows: g.Rows, Columns: g.Columns, sourceColumns: g.sourceColumns}
		pixels := tilePixels(frames.Data[0], tileGeometry, k, sampleBytes)
		dataset, err := buildUIHGridSlice(&budget, transcoded.Dataset, g, k, pixels, m.BitsAllocated, seriesUID, sliceUIDs[k], sourceSOPUID)
		if err != nil {
			return nil, UIHGridReport{}, wrap("slice", k, err)
		}
		file := &object.File{Dataset: dataset, Meta: object.New(std.Dictionary), TransferSyntax: transfer.ExplicitVRLittleEndian}
		outputs = append(outputs, file)
		if err := file.RebuildFileMeta(); err != nil {
			return nil, UIHGridReport{}, wrap("file_meta", k, err)
		}
		counter := &limitWriter{ctx: ctx, remaining: limits.MaxOutputBytes - outputBytes}
		if err := object.WriteFile(counter, file); err != nil {
			return nil, UIHGridReport{}, wrap("output", k, err)
		}
		outputBytes += counter.written
	}
	success = true
	return outputs, UIHGridReport{Geometry: g, OutputBytes: outputBytes}, nil
}

func uihMetadataCost(ctx context.Context, source *object.Object, limits Limits) (int, int64, error) {
	nodes, payloadBytes := 0, int64(0)
	var visit func(core.Element, int) error
	visit = func(e core.Element, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > limits.MaxDepth || nodes >= limits.MaxElements {
			return ErrResourceLimit
		}
		nodes++
		if sequence, ok := e.Value.(core.SequenceValue); ok {
			if len(sequence.Items) > limits.MaxElements-nodes {
				return ErrResourceLimit
			}
			nodes += len(sequence.Items)
			for _, item := range sequence.Items {
				for _, child := range item.Elements {
					if err := visit(child, depth+1); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if e.Value == nil {
			return ErrInvalidSource
		}
		length, known := e.Value.EncodedLength()
		if !known || uint64(length) > uint64(limits.MaxOutputBytes-payloadBytes) {
			return ErrResourceLimit
		}
		payloadBytes += int64(length)
		return nil
	}
	for _, e := range source.Elements() {
		if _, drop := mosaicDroppedTags[e.Tag()]; drop || e.Tag().IsPrivate() {
			continue
		}
		if err := visit(e, 1); err != nil {
			return 0, 0, err
		}
	}
	return nodes, payloadBytes, nil
}

func buildUIHGridSlice(budget *conversionBudget, source *object.Object, g UIHGridGeometry, k int, pixels []byte, bits uint16, seriesUID, sopUID, sourceSOPUID string) (*object.Object, error) {
	elements := make([]core.Element, 0, source.Len())
	for _, e := range source.Elements() {
		if _, drop := mosaicDroppedTags[e.Tag()]; drop || e.Tag().IsPrivate() {
			continue
		}
		clone, err := budget.cloneElement(e, 1)
		if err != nil {
			return nil, err
		}
		elements = append(elements, clone)
	}
	dataset := object.FromElements(elements, std.Dictionary)
	imageType, _ := source.GetStrings(tagImageType)
	kept := []string{"DERIVED", "SECONDARY"}
	for _, s := range imageType[2:] {
		if strings.TrimSpace(s) != "VFRAME" {
			kept = append(kept, s)
		}
	}
	dataset.Put(stringsElement(tagImageType, core.VRCS, kept))
	dataset.Put(ui(tagSOPInstanceUID, sopUID))
	dataset.Put(ui(tagSeriesInstanceUID, seriesUID))
	dataset.Put(str(tagInstanceNumber, core.VRIS, strconv.Itoa(k+1)))
	dataset.Put(derivedio.US(tagRows, uint16(g.Rows)))
	dataset.Put(derivedio.US(tagColumns, uint16(g.Columns)))
	p, o := g.Positions[k], g.Orientations[k]
	dataset.Put(derivedio.DS(tagImagePositionPatient, p[:]...))
	dataset.Put(derivedio.DS(tagImageOrientationPatient, o[:]...))
	dataset.Put(derivedio.DS(tagSliceLocation, dot(p, g.Normal)))
	vr := core.VROB
	if bits == 16 {
		vr = core.VROW
	}
	if len(pixels)%2 != 0 {
		pixels = append(pixels, 0)
	}
	dataset.Put(core.NewRawElement(tagPixelData, vr, pixels))
	if err := appendSourceReference(dataset, derivation.Source{SOPClassUID: MRImageStorageUID, SOPInstanceUID: sourceSOPUID}); err != nil {
		return nil, err
	}
	dataset.Put(str(tagDerivationDescription, core.VRST, fmt.Sprintf("Slice %d of %d split from a UIH grid image", k+1, g.Images)))
	return dataset, nil
}
