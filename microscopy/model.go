// Package microscopy models tiled DICOM whole-slide microscopy images without
// materializing their Total Pixel Matrix.
package microscopy

import (
	"errors"
	"fmt"
	"image"
	"math"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	VLWholeSlideMicroscopyImageStorage      = "1.2.840.10008.5.1.4.1.1.77.1.6"
	ConfocalMicroscopyTiledPyramidalStorage = "1.2.840.10008.5.1.4.1.1.77.1.9"
	MicroscopyBulkSimpleAnnotationsStorage  = "1.2.840.10008.5.1.4.1.1.91.1"
	maxMicroscopyFrames                     = 100_000_000
	defaultOpticalPathIdentifier            = "1"
)

var (
	ErrNotTiledMicroscopy = errors.New("dicom/microscopy: not a tiled microscopy image")
	ErrInvalidSlide       = errors.New("dicom/microscopy: invalid slide metadata")
	ErrUncalibratedSlide  = errors.New("dicom/microscopy: slide coordinates are not calibrated")
)

var (
	tagICCProfile                        = core.NewTag(0x0028, 0x2000)
	tagConcatenationFrameOffset          = core.NewTag(0x0020, 0x9228)
	tagDimensionOrganizationType         = core.NewTag(0x0020, 0x9311)
	tagImageType                         = core.NewTag(0x0008, 0x0008)
	tagSOPClassUID                       = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUID                    = core.NewTag(0x0008, 0x0018)
	tagStudyInstanceUID                  = core.NewTag(0x0020, 0x000D)
	tagSeriesInstanceUID                 = core.NewTag(0x0020, 0x000E)
	tagRows                              = core.NewTag(0x0028, 0x0010)
	tagColumns                           = core.NewTag(0x0028, 0x0011)
	tagNumberOfFrames                    = core.NewTag(0x0028, 0x0008)
	tagPixelSpacing                      = core.NewTag(0x0028, 0x0030)
	tagSharedFunctionalGroups            = core.NewTag(0x5200, 0x9229)
	tagPerFrameFunctionalGroups          = core.NewTag(0x5200, 0x9230)
	tagPixelMeasuresSequence             = core.NewTag(0x0028, 0x9110)
	tagSpacingBetweenSlices              = core.NewTag(0x0018, 0x0088)
	tagTotalPixelMatrixColumns           = core.NewTag(0x0048, 0x0006)
	tagTotalPixelMatrixRows              = core.NewTag(0x0048, 0x0007)
	tagTotalPixelMatrixOriginSequence    = core.NewTag(0x0048, 0x0008)
	tagImagedVolumeWidth                 = core.NewTag(0x0048, 0x0001)
	tagImagedVolumeHeight                = core.NewTag(0x0048, 0x0002)
	tagImagedVolumeDepth                 = core.NewTag(0x0048, 0x0003)
	tagImageOrientationSlide             = core.NewTag(0x0048, 0x0102)
	tagOpticalPathSequence               = core.NewTag(0x0048, 0x0105)
	tagOpticalPathIdentifier             = core.NewTag(0x0048, 0x0106)
	tagOpticalPathDescription            = core.NewTag(0x0048, 0x0107)
	tagOpticalPathIdentificationSequence = core.NewTag(0x0048, 0x0207)
	tagPlanePositionSlideSequence        = core.NewTag(0x0048, 0x021A)
	tagColumnPositionTotalMatrix         = core.NewTag(0x0048, 0x021E)
	tagRowPositionTotalMatrix            = core.NewTag(0x0048, 0x021F)
	tagNumberOfOpticalPaths              = core.NewTag(0x0048, 0x0302)
	tagTotalPixelMatrixFocalPlanes       = core.NewTag(0x0048, 0x0303)
	tagXOffsetSlide                      = core.NewTag(0x0040, 0x072A)
	tagYOffsetSlide                      = core.NewTag(0x0040, 0x073A)
	tagZOffsetSlide                      = core.NewTag(0x0040, 0x074A)
	tagContainerIdentifier               = core.NewTag(0x0040, 0x0512)
	tagSpecimenIdentifier                = core.NewTag(0x0040, 0x0551)
	tagSpecimenDescriptionSequence       = core.NewTag(0x0040, 0x0560)
)

// InstanceRef identifies a WSI instance used as a tile source.
type InstanceRef struct {
	StudyInstanceUID  string
	SeriesInstanceUID string
	SOPInstanceUID    string
}

// SlidePoint is a point in the DICOM Slide Coordinate System, in millimeters.
type SlidePoint struct {
	X float64
	Y float64
	Z float64
}

type OpticalPath struct {
	// ICCProfile belongs to this optical path, never the root pixel macro.
	// Read owns these bytes and bounds their aggregate allocation.
	ICCProfile  []byte
	Identifier  string
	Description string
}

type Specimen struct {
	Identifier          string
	ContainerIdentifier string
	Description         string
}

// Tile is a one-based DICOM frame positioned in the Total Pixel Matrix. Row
// and Column are zero-based pixel coordinates in that matrix.
type Tile struct {
	Source            InstanceRef
	FrameNumber       int
	Row               int
	Column            int
	Width             int
	Height            int
	OpticalPath       string
	FocalPlane        int
	ZOffsetMillimeter float64
}

func (t Tile) Bounds() image.Rectangle {
	return image.Rect(t.Column, t.Row, t.Column+t.Width, t.Row+t.Height)
}

func (t Tile) Key() TileKey {
	return TileKey{
		Source: t.Source, FrameNumber: t.FrameNumber,
		OpticalPath: t.OpticalPath, FocalPlane: t.FocalPlane,
	}
}

type TileKey struct {
	Source      InstanceRef
	FrameNumber int
	OpticalPath string
	FocalPlane  int
}

// Level describes one WSI pyramid resolution. Pixel spacing is expressed in
// millimeters per Total Pixel Matrix pixel.
type Level struct {
	// FrameOffset is the number of preceding frames for the first retained
	// source in a TILED_FULL concatenation.
	FrameOffset       int
	ID                string
	MatrixWidth       int
	MatrixHeight      int
	TileWidth         int
	TileHeight        int
	PixelSpacingX     float64
	PixelSpacingY     float64
	Origin            SlidePoint
	Orientation       [6]float64
	ImagedVolumeDepth float64
	Tiles             []Tile
	Source            InstanceRef
	FrameCount        int
	ImplicitTiling    bool
	OpticalPathIDs    []string
	FocalPlaneOffsets []float64
	concatenation     slideConcatenation
	members           []slideTileMember
}

func (l Level) MatrixBounds() image.Rectangle {
	return image.Rect(0, 0, l.MatrixWidth, l.MatrixHeight)
}

func (l Level) Calibrated() bool {
	return finitePositive(l.PixelSpacingX) && finitePositive(l.PixelSpacingY)
}

// SlideCoordinate maps a Total Pixel Matrix pixel to the DICOM slide
// coordinate system. Image Orientation (Slide) is honored when present.
func (l Level) SlideCoordinate(pixel image.Point, zMillimeter float64) (SlidePoint, error) {
	if !l.Calibrated() {
		return SlidePoint{}, ErrUncalibratedSlide
	}
	row := l.Orientation[0:3]
	column := l.Orientation[3:6]
	if zeroVector(row) || zeroVector(column) {
		row = []float64{1, 0, 0}
		column = []float64{0, 1, 0}
	}
	xDistance := float64(pixel.X) * l.PixelSpacingX
	yDistance := float64(pixel.Y) * l.PixelSpacingY
	return SlidePoint{
		X: l.Origin.X + row[0]*xDistance + column[0]*yDistance,
		Y: l.Origin.Y + row[1]*xDistance + column[1]*yDistance,
		Z: l.Origin.Z + row[2]*xDistance + column[2]*yDistance + zMillimeter,
	}, nil
}

// PixelCoordinate maps a calibrated DICOM Slide Coordinate System point back
// into floating-point Total Pixel Matrix coordinates.
func (l Level) PixelCoordinate(point SlidePoint) (x, y float64, err error) {
	if !l.Calibrated() {
		return 0, 0, ErrUncalibratedSlide
	}
	row := l.Orientation[0:3]
	column := l.Orientation[3:6]
	if zeroVector(row) || zeroVector(column) {
		row = []float64{1, 0, 0}
		column = []float64{0, 1, 0}
	}
	delta := [3]float64{
		point.X - l.Origin.X,
		point.Y - l.Origin.Y,
		point.Z - l.Origin.Z,
	}
	x = (delta[0]*row[0] + delta[1]*row[1] + delta[2]*row[2]) / l.PixelSpacingX
	y = (delta[0]*column[0] + delta[1]*column[1] + delta[2]*column[2]) / l.PixelSpacingY
	return x, y, nil
}

func (l Level) Distance(a, b image.Point) (float64, error) {
	first, err := l.SlideCoordinate(a, 0)
	if err != nil {
		return 0, err
	}
	second, err := l.SlideCoordinate(b, 0)
	if err != nil {
		return 0, err
	}
	return math.Sqrt(
		(second.X-first.X)*(second.X-first.X) +
			(second.Y-first.Y)*(second.Y-first.Y) +
			(second.Z-first.Z)*(second.Z-first.Z),
	), nil
}

// TilesForViewport returns only tiles intersecting the requested Total Pixel
// Matrix rectangle and selected optical path/focal plane. Results are sorted
// center-first to prioritize the visible viewport under network contention.
func (l Level) TilesForViewport(viewport image.Rectangle, opticalPath string, focalPlane int) []Tile {
	return l.TilesForViewportLimit(viewport, opticalPath, focalPlane, 0)
}

// TilesForViewportLimit is TilesForViewport with a center-priority result cap.
// A non-positive limit returns every matching tile.
func (l Level) TilesForViewportLimit(viewport image.Rectangle, opticalPath string, focalPlane, limit int) []Tile {
	return l.tilesForViewportLimit(viewport, focalPlane, limit, func(path string) bool {
		return opticalPath == "" || path == opticalPath
	}, false)
}

func (l Level) tilesForViewportLimit(viewport image.Rectangle, focalPlane, limit int, matchesPath func(string) bool, deduplicate bool) []Tile {
	viewport = viewport.Intersect(l.MatrixBounds())
	if viewport.Empty() {
		return nil
	}
	if l.ImplicitTiling {
		return l.implicitTilesForViewport(viewport, focalPlane, limit, matchesPath)
	}
	out := make([]Tile, 0)
	for _, tile := range l.Tiles {
		if !matchesPath(tile.OpticalPath) {
			continue
		}
		if focalPlane >= 0 && tile.FocalPlane != focalPlane {
			continue
		}
		if tile.Bounds().Overlaps(viewport) {
			out = append(out, tile)
		}
	}
	center := image.Pt((viewport.Min.X+viewport.Max.X)/2, (viewport.Min.Y+viewport.Max.Y)/2)
	sort.SliceStable(out, func(i, j int) bool {
		return squaredDistance(out[i].Bounds(), center) < squaredDistance(out[j].Bounds(), center)
	})
	if deduplicate {
		return deduplicateTiles(out, limit)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (l Level) implicitTilesForViewport(viewport image.Rectangle, focalPlane, limit int, matchesPath func(string) bool) []Tile {
	if l.FrameCount <= 0 || l.TileWidth <= 0 || l.TileHeight <= 0 {
		return nil
	}
	tilesAcross := ceilDiv(l.MatrixWidth, l.TileWidth)
	tilesDown := ceilDiv(l.MatrixHeight, l.TileHeight)
	tilesPerPlanePath := tilesAcross * tilesDown
	pathIDs := l.OpticalPathIDs
	if len(pathIDs) == 0 {
		pathIDs = []string{defaultOpticalPathIdentifier}
	}
	var pathIndices []int
	for index, identifier := range pathIDs {
		if matchesPath(identifier) {
			pathIndices = append(pathIndices, index)
		}
	}
	if len(pathIndices) == 0 {
		return nil
	}
	planeCount := max(1, len(l.FocalPlaneOffsets))
	firstPlane, lastPlane := 0, planeCount-1
	if focalPlane >= 0 {
		if focalPlane >= planeCount {
			return nil
		}
		firstPlane, lastPlane = focalPlane, focalPlane
	}
	firstColumn := max(0, viewport.Min.X/l.TileWidth)
	lastColumn := min(tilesAcross-1, (viewport.Max.X-1)/l.TileWidth)
	firstRow := max(0, viewport.Min.Y/l.TileHeight)
	lastRow := min(tilesDown-1, (viewport.Max.Y-1)/l.TileHeight)
	capacity := min(l.FrameCount, (lastColumn-firstColumn+1)*(lastRow-firstRow+1)*len(pathIndices)*(lastPlane-firstPlane+1))
	if limit > 0 {
		capacity = min(capacity, limit)
	}
	out := make([]Tile, 0, capacity)
	appendCoordinate := func(rowIndex, columnIndex int) bool {
		for plane := firstPlane; plane <= lastPlane; plane++ {
			for _, pathIndex := range pathIndices {
				ordinal := pathIndex*tilesPerPlanePath*planeCount + plane*tilesPerPlanePath + rowIndex*tilesAcross + columnIndex
				source, frameNumber, found := l.locateTileFrame(ordinal)
				if !found {
					continue
				}
				column := columnIndex * l.TileWidth
				row := rowIndex * l.TileHeight
				z := 0.0
				if plane < len(l.FocalPlaneOffsets) {
					z = l.FocalPlaneOffsets[plane]
				}
				out = append(out, Tile{
					Source: source, FrameNumber: frameNumber,
					Row: row, Column: column,
					Width: min(l.TileWidth, l.MatrixWidth-column), Height: min(l.TileHeight, l.MatrixHeight-row),
					OpticalPath: pathIDs[pathIndex], FocalPlane: plane, ZOffsetMillimeter: z,
				})
				if limit > 0 && len(out) >= limit {
					return true
				}
			}
		}
		return false
	}
	if limit > 0 {
		centerColumn := min(lastColumn, max(firstColumn, ((viewport.Min.X+viewport.Max.X)/2)/l.TileWidth))
		centerRow := min(lastRow, max(firstRow, ((viewport.Min.Y+viewport.Max.Y)/2)/l.TileHeight))
		maxRadius := max(
			max(centerColumn-firstColumn, lastColumn-centerColumn),
			max(centerRow-firstRow, lastRow-centerRow),
		)
		for radius := 0; radius <= maxRadius; radius++ {
			for rowIndex := centerRow - radius; rowIndex <= centerRow+radius; rowIndex++ {
				if rowIndex < firstRow || rowIndex > lastRow {
					continue
				}
				for columnIndex := centerColumn - radius; columnIndex <= centerColumn+radius; columnIndex++ {
					if columnIndex < firstColumn || columnIndex > lastColumn ||
						(radius > 0 && rowIndex != centerRow-radius && rowIndex != centerRow+radius &&
							columnIndex != centerColumn-radius && columnIndex != centerColumn+radius) {
						continue
					}
					if appendCoordinate(rowIndex, columnIndex) {
						return out
					}
				}
			}
		}
		return out
	}
	for rowIndex := firstRow; rowIndex <= lastRow; rowIndex++ {
		for columnIndex := firstColumn; columnIndex <= lastColumn; columnIndex++ {
			appendCoordinate(rowIndex, columnIndex)
		}
	}
	center := image.Pt((viewport.Min.X+viewport.Max.X)/2, (viewport.Min.Y+viewport.Max.Y)/2)
	sort.SliceStable(out, func(i, j int) bool {
		return squaredDistance(out[i].Bounds(), center) < squaredDistance(out[j].Bounds(), center)
	})
	return out
}

type Instance struct {
	// ColorPixels identifies native RGB/YBR/palette colour rather than a
	// monochrome channel intended for an explicit fluorescence tint.
	ColorPixels    bool
	Level          Level
	OpticalPaths   []OpticalPath
	FocalPlanes    []float64
	Specimen       Specimen
	DimensionOrder []string
}

// Pyramid combines sparse resolution levels without inventing missing levels.
type Pyramid struct {
	Levels       []Level
	OpticalPaths []OpticalPath
	FocalPlanes  []float64
	Specimens    []Specimen
}

func (p *Pyramid) Add(instance Instance) error {
	if p == nil {
		return fmt.Errorf("%w: nil pyramid", ErrInvalidSlide)
	}
	level := instance.Level
	if level.MatrixWidth <= 0 || level.MatrixHeight <= 0 || level.TileWidth <= 0 || level.TileHeight <= 0 {
		return fmt.Errorf("%w: invalid pyramid level", ErrInvalidSlide)
	}
	if err := p.addLevel(level); err != nil {
		return err
	}
	p.OpticalPaths = appendUniqueOpticalPaths(p.OpticalPaths, instance.OpticalPaths...)
	p.FocalPlanes = appendUniqueFloats(p.FocalPlanes, instance.FocalPlanes...)
	if instance.Specimen != (Specimen{}) {
		p.Specimens = appendUniqueSpecimens(p.Specimens, instance.Specimen)
	}
	sort.SliceStable(p.Levels, func(i, j int) bool {
		left, right := p.Levels[i], p.Levels[j]
		if left.Calibrated() && right.Calibrated() && left.PixelSpacingX != right.PixelSpacingX {
			return left.PixelSpacingX < right.PixelSpacingX
		}
		return int64(left.MatrixWidth)*int64(left.MatrixHeight) > int64(right.MatrixWidth)*int64(right.MatrixHeight)
	})
	sort.Float64s(p.FocalPlanes)
	return nil
}

// LevelForScale chooses the level whose source pixels most closely match one
// screen pixel, preferring the next finer level over an undersampled one.
func (p *Pyramid) LevelForScale(totalMatrixPixelsPerScreenPixel float64) (Level, bool) {
	if p == nil || len(p.Levels) == 0 {
		return Level{}, false
	}
	if !finitePositive(totalMatrixPixelsPerScreenPixel) {
		return p.Levels[0], true
	}
	base := p.Levels[0]
	best := base
	bestError := math.Inf(1)
	for _, level := range p.Levels {
		ratio := float64(base.MatrixWidth) / float64(level.MatrixWidth)
		if !finitePositive(ratio) {
			continue
		}
		err := math.Abs(math.Log2(ratio / totalMatrixPixelsPerScreenPixel))
		if err < bestError || (err == bestError && ratio < totalMatrixPixelsPerScreenPixel) {
			best, bestError = level, err
		}
	}
	return best, true
}

// Read parses one tiled microscopy image instance. Pixel Data is intentionally
// not decoded or copied.
func Read(file *object.File) (Instance, error) {
	if file == nil || file.Dataset == nil {
		return Instance{}, fmt.Errorf("%w: nil dataset", ErrInvalidSlide)
	}
	root := file.Dataset
	sopClass := derivedio.CleanUID(root, tagSOPClassUID)
	if sopClass != VLWholeSlideMicroscopyImageStorage && sopClass != ConfocalMicroscopyTiledPyramidalStorage {
		return Instance{}, ErrNotTiledMicroscopy
	}
	matrixWidth := derivedio.Int(root, tagTotalPixelMatrixColumns)
	matrixHeight := derivedio.Int(root, tagTotalPixelMatrixRows)
	tileWidth := derivedio.Int(root, tagColumns)
	tileHeight := derivedio.Int(root, tagRows)
	frameCount, err := slideCount(root, tagNumberOfFrames, maxMicroscopyFrames)
	if err != nil {
		return Instance{}, err
	}
	if matrixWidth <= 0 || matrixHeight <= 0 || tileWidth <= 0 || tileHeight <= 0 {
		return Instance{}, fmt.Errorf(
			"%w: matrix=%dx%d tile=%dx%d frames=%d",
			ErrInvalidSlide, matrixWidth, matrixHeight, tileWidth, tileHeight, frameCount,
		)
	}
	ref := InstanceRef{
		StudyInstanceUID:  derivedio.CleanUID(root, tagStudyInstanceUID),
		SeriesInstanceUID: derivedio.CleanUID(root, tagSeriesInstanceUID),
		SOPInstanceUID:    derivedio.CleanUID(root, tagSOPInstanceUID),
	}
	spacingX, spacingY := slidePixelSpacing(root, matrixWidth, matrixHeight)
	origin, err := slideOrigin(root)
	if err != nil {
		return Instance{}, err
	}
	orientation := slideOrientation(root)
	opticalPaths, err := readOpticalPaths(root)
	if err != nil {
		return Instance{}, err
	}
	if len(opticalPaths) == 0 {
		opticalPaths = []OpticalPath{{Identifier: defaultOpticalPathIdentifier}}
	}
	concatenation, err := readSlideConcatenation(root)
	if err != nil {
		return Instance{}, err
	}
	if concatenation.offset > maxMicroscopyFrames-frameCount {
		return Instance{}, fmt.Errorf("%w: concatenation frame offset", ErrInvalidSlide)
	}
	level := Level{
		ID:          fmt.Sprintf("%s:%dx%d", ref.SOPInstanceUID, matrixWidth, matrixHeight),
		MatrixWidth: matrixWidth, MatrixHeight: matrixHeight,
		TileWidth: tileWidth, TileHeight: tileHeight,
		PixelSpacingX: spacingX, PixelSpacingY: spacingY,
		Origin: origin, Orientation: orientation,
		ImagedVolumeDepth: firstFloat(root, tagImagedVolumeDepth),
		Source:            ref,
		FrameOffset:       concatenation.offset, FrameCount: frameCount, concatenation: concatenation,
		OpticalPathIDs: make([]string, len(opticalPaths)),
	}
	for index, path := range opticalPaths {
		level.OpticalPathIDs[index] = path.Identifier
	}
	if err := placeSlideTiles(root, &level); err != nil {
		return Instance{}, err
	}
	return Instance{
		ColorPixels:  derivedio.Int(root, core.NewTag(0x0028, 0x0002)) >= 3 || strings.EqualFold(derivedio.CleanString(root, core.NewTag(0x0028, 0x0004)), "PALETTE COLOR"),
		Level:        level,
		OpticalPaths: opticalPaths,
		FocalPlanes:  append([]float64(nil), level.FocalPlaneOffsets...),
		Specimen:     readSpecimen(root),
	}, nil
}

func slidePixelSpacing(root *object.Object, matrixWidth, matrixHeight int) (float64, float64) {
	var measures *object.Object
	if shared := derivedio.Sequence(root, tagSharedFunctionalGroups); len(shared) > 0 {
		measures = firstNestedSequenceItem(shared[0], tagPixelMeasuresSequence)
	}
	for _, source := range []*object.Object{measures, root} {
		if row, column, values, err := source.SpacingPair(tagPixelSpacing); err == nil && values != object.SpacingValuesAbsent {
			return column, row
		}
	}
	width := firstFloat(root, tagImagedVolumeWidth)
	height := firstFloat(root, tagImagedVolumeHeight)
	if finitePositive(width) && finitePositive(height) {
		return width / float64(matrixWidth), height / float64(matrixHeight)
	}
	return 0, 0
}

func slideOrigin(root *object.Object) (SlidePoint, error) {
	items := derivedio.Sequence(root, tagTotalPixelMatrixOriginSequence)
	if len(items) == 0 {
		return SlidePoint{}, nil
	}
	z, err := slideZ(items[0], 0)
	if err != nil {
		return SlidePoint{}, err
	}
	return SlidePoint{
		X: firstFloat(items[0], tagXOffsetSlide),
		Y: firstFloat(items[0], tagYOffsetSlide),
		Z: z,
	}, nil
}

func slideOrientation(root *object.Object) (out [6]float64) {
	values := derivedio.Floats(root, tagImageOrientationSlide)
	if len(values) == 6 {
		copy(out[:], values)
	}
	return out
}

func readOpticalPaths(root *object.Object) ([]OpticalPath, error) {
	items := derivedio.Sequence(root, tagOpticalPathSequence)
	if len(items) > MaxOpticalPaths {
		return nil, ErrColorResourceLimit
	}
	out := make([]OpticalPath, 0, len(items))
	seen := make(map[string]bool, len(items))
	bytes := 0
	for _, item := range items {
		identifier := derivedio.CleanString(item, tagOpticalPathIdentifier)
		if len(identifier) > 64 {
			return nil, ErrColorResourceLimit
		}
		if identifier == "" || seen[identifier] {
			return nil, fmt.Errorf("%w: missing or duplicate optical path identifier", ErrInvalidSlide)
		}
		seen[identifier] = true
		var data []byte
		if element, ok := item.Get(tagICCProfile); ok {
			data, _ = element.RawBytes()
		}
		if len(data) > MaxOpticalPathProfileBytes || len(data) > MaxInstanceProfileBytes-bytes {
			return nil, ErrColorResourceLimit
		}
		bytes += len(data)
		out = append(out, OpticalPath{
			ICCProfile:  append([]byte(nil), data...),
			Identifier:  identifier,
			Description: derivedio.CleanString(item, tagOpticalPathDescription),
		})
	}
	return out, nil
}

func readSpecimen(root *object.Object) Specimen {
	specimen := Specimen{
		Identifier:          derivedio.CleanString(root, tagSpecimenIdentifier),
		ContainerIdentifier: derivedio.CleanString(root, tagContainerIdentifier),
	}
	if items := derivedio.Sequence(root, tagSpecimenDescriptionSequence); len(items) > 0 {
		if specimen.Identifier == "" {
			specimen.Identifier = derivedio.CleanString(items[0], tagSpecimenIdentifier)
		}
	}
	return specimen
}

func firstNestedSequenceItem(root *object.Object, target core.Tag) *object.Object {
	return findNestedSequenceItem(root, target, 0)
}

func findNestedSequenceItem(root *object.Object, target core.Tag, depth int) *object.Object {
	if root == nil || depth > 5 {
		return nil
	}
	if items := derivedio.Sequence(root, target); len(items) > 0 {
		return items[0]
	}
	for _, element := range root.Elements() {
		if element.VR() != core.VRSQ {
			continue
		}
		items, ok := root.GetSequence(element.Tag())
		if !ok {
			continue
		}
		for _, item := range items {
			if found := findNestedSequenceItem(item, target, depth+1); found != nil {
				return found
			}
		}
	}
	return nil
}

func firstFloat(root *object.Object, tag core.Tag) float64 {
	values := derivedio.Floats(root, tag)
	if len(values) == 0 || math.IsNaN(values[0]) || math.IsInf(values[0], 0) {
		return 0
	}
	return values[0]
}

func appendUniqueOpticalPaths(existing []OpticalPath, values ...OpticalPath) []OpticalPath {
	seen := make(map[string]bool, len(existing))
	for _, value := range existing {
		seen[value.Identifier] = true
	}
	for _, value := range values {
		value.Identifier = strings.TrimSpace(value.Identifier)
		if value.Identifier == "" || seen[value.Identifier] {
			continue
		}
		seen[value.Identifier] = true
		existing = append(existing, value)
	}
	return existing
}

func appendUniqueFloats(existing []float64, values ...float64) []float64 {
	for _, value := range values {
		found := false
		for _, current := range existing {
			if math.Abs(current-value) <= 1e-9 {
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, value)
		}
	}
	return existing
}

func appendUniqueSpecimens(existing []Specimen, value Specimen) []Specimen {
	for _, current := range existing {
		if current.Identifier == value.Identifier && current.ContainerIdentifier == value.ContainerIdentifier {
			return existing
		}
	}
	return append(existing, value)
}

func squaredDistance(bounds image.Rectangle, point image.Point) int64 {
	centerX := int64(bounds.Min.X+bounds.Max.X) / 2
	centerY := int64(bounds.Min.Y+bounds.Max.Y) / 2
	dx := centerX - int64(point.X)
	dy := centerY - int64(point.Y)
	return dx*dx + dy*dy
}

func ceilDiv(value, divisor int) int {
	quotient := value / divisor
	if value%divisor > 0 {
		quotient++
	}
	return quotient
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func zeroVector(vector []float64) bool {
	return len(vector) != 3 || (vector[0] == 0 && vector[1] == 0 && vector[2] == 0)
}
