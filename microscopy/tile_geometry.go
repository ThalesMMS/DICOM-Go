package microscopy

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

const maxSlideFocalPlanes = 4096

func placeSlideTiles(root *object.Object, level *Level) error {
	full := derivedio.CleanString(root, tagDimensionOrganizationType) == "TILED_FULL"
	perFrame := derivedio.Sequence(root, tagPerFrameFunctionalGroups)
	if _, present := root.Get(tagPerFrameFunctionalGroups); present && len(perFrame) != level.FrameCount {
		return fmt.Errorf("%w: %d per-frame groups for %d frames", ErrInvalidSlide, len(perFrame), level.FrameCount)
	}
	if !full && len(perFrame) == 0 {
		return fmt.Errorf("%w: frame positions are required outside TILED_FULL", ErrInvalidSlide)
	}
	shared, err := slideMacro(root, tagSharedFunctionalGroups)
	if err != nil {
		return err
	}
	sharedPath, err := slidePath(shared, level)
	if err != nil {
		return err
	}
	ancillary := isSingleFrameSlideFlavor(root)
	if ancillary && (level.FrameCount != 1 || level.concatenation.uid != "") {
		return fmt.Errorf("%w: ancillary slide image requires one standalone frame", ErrInvalidSlide)
	}
	if !full {
		return readSparseTiles(level, perFrame, sharedPath)
	}
	if ancillary {
		return placeTiledFullAncillary(level, shared, perFrame, sharedPath)
	}
	return placeTiledFull(root, level, shared, perFrame, sharedPath)
}

func placeTiledFull(root *object.Object, level *Level, shared *object.Object, perFrame []*object.Object, sharedPath string) error {
	if _, present := root.Get(tagNumberOfOpticalPaths); present {
		declared := derivedio.Ints(root, tagNumberOfOpticalPaths)
		if len(declared) != 1 || declared[0] != int64(len(level.OpticalPathIDs)) {
			return fmt.Errorf("%w: optical path count disagrees with the path sequence", ErrInvalidSlide)
		}
	}
	planeCount, err := slideCount(root, tagTotalPixelMatrixFocalPlanes, maxSlideFocalPlanes)
	if err != nil {
		return err
	}
	expected, err := implicitTileCount(level.MatrixWidth, level.MatrixHeight, level.TileWidth, level.TileHeight, len(level.OpticalPathIDs), planeCount)
	if err != nil {
		return err
	}
	end := level.FrameOffset + level.FrameCount
	if level.concatenation.uid == "" {
		if end != expected {
			return fmt.Errorf("%w: %d frames for %d TILED_FULL tile positions", ErrInvalidSlide, end, expected)
		}
	} else {
		if end > expected {
			return fmt.Errorf("%w: concatenation frames exceed the TILED_FULL layout", ErrInvalidSlide)
		}
		member := slideTileMember{source: level.Source, number: level.concatenation.number, offset: level.FrameOffset, frames: level.FrameCount}
		if err := validateSlideSpan(member, level.concatenation.total, expected); err != nil {
			return err
		}
	}
	if sharedPath != "" {
		framesPerPath := expected / len(level.OpticalPathIDs)
		if level.OpticalPathIDs[level.FrameOffset/framesPerPath] != sharedPath ||
			level.OpticalPathIDs[(end-1)/framesPerPath] != sharedPath {
			return fmt.Errorf("%w: shared optical path disagrees with the TILED_FULL frame span", ErrInvalidSlide)
		}
	}
	spacing := 0.0
	if planeCount > 1 {
		var err error
		spacing, err = slideFocalSpacing(shared, perFrame)
		if err != nil {
			return err
		}
	}
	level.FocalPlaneOffsets = make([]float64, planeCount)
	for plane := range level.FocalPlaneOffsets {
		z := level.Origin.Z + float64(plane)*spacing
		if math.IsInf(z, 0) || plane > 0 && z <= level.FocalPlaneOffsets[plane-1] {
			return fmt.Errorf("%w: unrepresentable focal plane position", ErrInvalidSlide)
		}
		level.FocalPlaneOffsets[plane] = z
	}
	if err := validateTiledFullGroups(level, shared, perFrame); err != nil {
		return err
	}
	level.ImplicitTiling = true
	return nil
}

// Ancillary TILED_FULL images have a single frame whose extent need not
// cover the full Total Pixel Matrix (PS3.3 C.7.6.17.3).
func placeTiledFullAncillary(level *Level, shared *object.Object, frames []*object.Object, path string) error {
	sharedPosition, err := slideMacro(shared, tagPlanePositionSlideSequence)
	if err != nil {
		return err
	}
	if len(frames) == 1 {
		position, err := slideMacro(frames[0], tagPlanePositionSlideSequence)
		if err != nil {
			return err
		}
		if position != nil {
			sharedPosition = position
		}
		explicitPath, err := slidePath(frames[0], level)
		if err != nil {
			return err
		}
		if explicitPath != "" {
			path = explicitPath
		}
	}
	if path == "" {
		path = level.OpticalPathIDs[0]
	}
	column, row, z := 0, 0, level.Origin.Z
	if sharedPosition != nil {
		column, row, z, err = slideTilePosition(sharedPosition, z, level)
		if err != nil {
			return err
		}
	}
	level.Tiles = []Tile{{
		Source: level.Source, FrameNumber: 1, Column: column, Row: row,
		Width: min(level.TileWidth, level.MatrixWidth-column), Height: min(level.TileHeight, level.MatrixHeight-row),
		OpticalPath: path, ZOffsetMillimeter: z,
	}}
	level.FocalPlaneOffsets = []float64{z}
	return nil
}

func validateTiledFullGroups(level *Level, shared *object.Object, perFrame []*object.Object) error {
	sharedPosition, err := slideMacro(shared, tagPlanePositionSlideSequence)
	if err != nil {
		return err
	}
	if sharedPosition == nil && len(perFrame) == 0 {
		return nil
	}
	across := ceilDiv(level.MatrixWidth, level.TileWidth)
	tilesPerPlane := int64(across) * int64(ceilDiv(level.MatrixHeight, level.TileHeight))
	planes := int64(len(level.FocalPlaneOffsets))
	for index := range level.FrameCount {
		ordinal := int64(level.FrameOffset + index)
		plane := ordinal / tilesPerPlane % planes
		column := int(ordinal%tilesPerPlane%int64(across)) * level.TileWidth
		row := int(ordinal%tilesPerPlane/int64(across)) * level.TileHeight
		path := level.OpticalPathIDs[ordinal/(tilesPerPlane*planes)]
		position := sharedPosition
		if len(perFrame) != 0 {
			frame := perFrame[index]
			explicit, err := slideMacro(frame, tagPlanePositionSlideSequence)
			if err != nil {
				return err
			}
			if explicit != nil {
				position = explicit
			}
			explicitPath, err := slidePath(frame, level)
			if err != nil {
				return err
			}
			if explicitPath != "" && explicitPath != path {
				return fmt.Errorf("%w: frame %d optical path disagrees with TILED_FULL order", ErrInvalidSlide, index+1)
			}
		}
		if position == nil {
			continue
		}
		z := level.FocalPlaneOffsets[plane]
		gotColumn, gotRow, gotZ, err := slideTilePosition(position, z, level)
		if err != nil {
			return fmt.Errorf("frame %d: %w", index+1, err)
		}
		if gotColumn != column || gotRow != row || math.Abs(gotZ-z) > 1e-9 {
			return fmt.Errorf("%w: frame %d position disagrees with TILED_FULL order", ErrInvalidSlide, index+1)
		}
	}
	return nil
}

func readSparseTiles(level *Level, perFrame []*object.Object, sharedPath string) error {
	if sharedPath == "" && len(level.OpticalPathIDs) == 1 {
		sharedPath = level.OpticalPathIDs[0]
	}
	planes := make(map[float64]struct{})
	tiles := make([]Tile, len(perFrame))
	for index, frame := range perFrame {
		position, err := slideMacro(frame, tagPlanePositionSlideSequence)
		if err != nil {
			return fmt.Errorf("frame %d: %w", index+1, err)
		}
		if position == nil {
			return fmt.Errorf("%w: frame %d has no explicit tile position", ErrInvalidSlide, index+1)
		}
		column, row, z, err := slideTilePosition(position, level.Origin.Z, level)
		if err != nil {
			return fmt.Errorf("frame %d: %w", index+1, err)
		}
		path, err := slidePath(frame, level)
		if err != nil {
			return err
		}
		if path == "" {
			path = sharedPath
		}
		if path == "" {
			return ColorDiagnostic{Source: level.Source, State: ColorMissingPath, Cause: ErrMissingOpticalPath}
		}
		planes[z] = struct{}{}
		if len(planes) > maxSlideFocalPlanes {
			return ErrColorResourceLimit
		}
		tiles[index] = Tile{
			Source: level.Source, FrameNumber: index + 1,
			Row: row, Column: column,
			Width: min(level.TileWidth, level.MatrixWidth-column), Height: min(level.TileHeight, level.MatrixHeight-row),
			OpticalPath: path, ZOffsetMillimeter: z,
		}
	}
	offsets := make([]float64, 0, len(planes))
	for z := range planes {
		offsets = append(offsets, z)
	}
	sort.Float64s(offsets)
	for index := range tiles {
		tiles[index].FocalPlane = sort.SearchFloat64s(offsets, tiles[index].ZOffsetMillimeter)
	}
	level.Tiles = tiles
	level.FocalPlaneOffsets = offsets
	return nil
}

func slideTilePosition(position *object.Object, fallbackZ float64, level *Level) (column, row int, z float64, err error) {
	columns := derivedio.Ints(position, tagColumnPositionTotalMatrix)
	rows := derivedio.Ints(position, tagRowPositionTotalMatrix)
	if len(columns) != 1 || len(rows) != 1 ||
		columns[0] <= 0 || columns[0] > int64(level.MatrixWidth) ||
		rows[0] <= 0 || rows[0] > int64(level.MatrixHeight) {
		return 0, 0, 0, fmt.Errorf("%w: invalid one-based tile position", ErrInvalidSlide)
	}
	z, err = slideZ(position, fallbackZ)
	return int(columns[0] - 1), int(rows[0] - 1), z, err
}

func slideZ(root *object.Object, fallback float64) (float64, error) {
	if _, present := root.Get(tagZOffsetSlide); !present {
		return fallback, nil
	}
	values := derivedio.Floats(root, tagZOffsetSlide)
	if len(values) != 1 || math.IsNaN(values[0]) || math.IsInf(values[0], 0) {
		return 0, fmt.Errorf("%w: invalid slide Z offset", ErrInvalidSlide)
	}
	// PS3.3 C.8.12.6 and C.8.12.14 encode slide Z offsets in micrometers.
	return values[0] / 1000, nil
}

func slideFocalSpacing(shared *object.Object, perFrame []*object.Object) (float64, error) {
	sharedMeasures, err := slideMacro(shared, tagPixelMeasuresSequence)
	if err != nil {
		return 0, err
	}
	if sharedMeasures != nil {
		for _, frame := range perFrame {
			if _, present := frame.Get(tagPixelMeasuresSequence); present {
				return 0, fmt.Errorf("%w: Pixel Measures occur in both shared and per-frame groups", ErrInvalidSlide)
			}
		}
		return spacingBetweenSlidePlanes(sharedMeasures)
	}
	spacing := 0.0
	for _, frame := range perFrame {
		measures, err := slideMacro(frame, tagPixelMeasuresSequence)
		if err != nil {
			return 0, err
		}
		current, err := spacingBetweenSlidePlanes(measures)
		if err != nil {
			return 0, err
		}
		if spacing > 0 && spacing != current {
			return 0, fmt.Errorf("%w: inconsistent focal plane spacing", ErrInvalidSlide)
		}
		spacing = current
	}
	if spacing <= 0 {
		return 0, fmt.Errorf("%w: missing Pixel Measures for TILED_FULL focal planes", ErrInvalidSlide)
	}
	return spacing, nil
}

func spacingBetweenSlidePlanes(measures *object.Object) (float64, error) {
	// Spacing Between Slices is in millimeters, independent of the
	// acquisition focus-stack count and distance (0048,0013/0014).
	values := derivedio.Floats(measures, tagSpacingBetweenSlices)
	if len(values) != 1 || !finitePositive(values[0]) {
		return 0, fmt.Errorf("%w: invalid Spacing Between Slices for TILED_FULL focal planes", ErrInvalidSlide)
	}
	return values[0], nil
}

func slidePath(group *object.Object, level *Level) (string, error) {
	path, err := slideMacro(group, tagOpticalPathIdentificationSequence)
	if err != nil || path == nil {
		return "", err
	}
	id := derivedio.CleanString(path, tagOpticalPathIdentifier)
	if id == "" {
		return "", ColorDiagnostic{Source: level.Source, State: ColorMissingPath, Cause: ErrMissingOpticalPath}
	}
	if !slices.Contains(level.OpticalPathIDs, id) {
		return "", ColorDiagnostic{Source: level.Source, OpticalPath: id, State: ColorUnknownPath, Cause: ErrUnknownOpticalPath}
	}
	return id, nil
}

func slideMacro(group *object.Object, tag core.Tag) (*object.Object, error) {
	if group == nil {
		return nil, nil
	}
	if _, present := group.Get(tag); !present {
		return nil, nil
	}
	items := derivedio.Sequence(group, tag)
	if len(items) != 1 {
		return nil, fmt.Errorf("%w: expected one item for %s", ErrInvalidSlide, tag)
	}
	return items[0], nil
}

func slideCount(root *object.Object, tag core.Tag, maximum int) (int, error) {
	if _, present := root.Get(tag); !present {
		// Retain the one-frame or one-plane default for legacy metadata.
		return 1, nil
	}
	values := derivedio.Ints(root, tag)
	if len(values) != 1 || values[0] <= 0 {
		return 0, fmt.Errorf("%w: expected positive count for %s", ErrInvalidSlide, tag)
	}
	if values[0] > int64(maximum) {
		if tag == tagTotalPixelMatrixFocalPlanes {
			return 0, ErrColorResourceLimit
		}
		return 0, fmt.Errorf("%w: frame count exceeds limits", ErrInvalidSlide)
	}
	return int(values[0]), nil
}

func isSingleFrameSlideFlavor(root *object.Object) bool {
	values, _ := root.GetStrings(tagImageType)
	if len(values) < 3 {
		return false
	}
	switch strings.TrimSpace(values[2]) {
	case "THUMBNAIL", "LABEL", "OVERVIEW", "LOCALIZER":
		return true
	default:
		return false
	}
}
