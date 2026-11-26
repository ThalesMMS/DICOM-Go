// Package tractographygeom selects, measures and tessellates stored DICOM
// tracks in patient LPS millimetres. It does not generate tracks or render images.
// Inputs must remain stable during calls; returned buffers are independently owned.
package tractographygeom

import (
	"errors"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/mesh"
	"github.com/ThalesMMS/dicom-go/tractography"
)

var (
	ErrInvalidOptions = errors.New("tractographygeom: invalid options")
	ErrInvalidData    = errors.New("tractographygeom: invalid geometry or scalar data")
	ErrLimitExceeded  = errors.New("tractographygeom: display limit exceeded")
)

// TrackID identifies the original stored track; TrackIndex is zero-based.
type TrackID struct {
	SetNumber  uint32
	TrackIndex int
}

type RegionKind uint8

const (
	Sphere RegionKind = iota
	Box
)

// Region is closed: touching a boundary counts as intersection. Sphere uses
// Center/RadiusMM; Box uses Min/Max. Unused fields must be zero.
type Region struct {
	Kind     RegionKind
	Center   [3]float64
	RadiusMM float64
	Min, Max [3]float64
}

// SelectionOptions selects any include (or all with RequireAllIncludes), then
// removes any exclude. Empty Include selects all. Tests use complete original
// segments, including crossings whose endpoints are both outside a region.
type SelectionOptions struct {
	Include, Exclude   []Region
	RequireAllIncludes bool
}

type TrackAnalysis struct {
	ID           TrackID
	LengthMM     float64
	Measurements []tractography.Measurement
	Statistics   []tractography.TrackStatistic
}
type Analysis struct {
	Tracks                                                                   []TrackAnalysis
	TotalLengthMM                                                            float64
	References                                                               []tractography.ImageReference
	SOPInstanceUID, StudyInstanceUID, SeriesInstanceUID, FrameOfReferenceUID string
	Sets                                                                     []SetAnalysis
}
type SetAnalysis struct {
	SetNumber  uint32
	Statistics []tractography.TrackSetStatistic
}

type Style uint8

const (
	Line Style = iota
	Tube
	Ribbon
)

type ColorMode uint8

const (
	Direction ColorMode = iota
	Scalar
)

// ScalarMapping explicitly identifies one per-point measurement, its coded
// quantity and UCUM units, and a finite increasing display domain. Sparse
// measurements are refused for display; nonfinite values are never discarded.
// Values outside [Min,Max] saturate to the blue-to-red display ramp.
type ScalarMapping struct {
	MeasurementIndex int
	Quantity, Units  codes.Code
	Min, Max         float64
}

// DisplayOptions bounds output, not selection or measurements. Zero limits use
// finite defaults; positive values may only reduce them. PointStride zero/one
// keeps original distinct points when budgets permit. LOD retains endpoints,
// deterministically samples tracks and original point indices, and never alters
// source tracks. TubeSides defaults to 6 (range 3..32). Tubes have open ends;
// ribbons use a transported local frame with width 2*RadiusMM.
type DisplayOptions struct {
	Style                                        Style
	RadiusMM                                     float64
	TubeSides, PointStride                       int
	MaxTracks, MaxPoints, MaxTriangles, MaxBytes int
	Color                                        ColorMode
	Scalar                                       *ScalarMapping
}

type Polyline struct {
	Owner  TrackID
	Points [][3]float32
	// Colors is linear RGB in [0,1], one value per point.
	Colors             [][3]float32
	SourcePointIndices []int
}
type Surface struct {
	Owner TrackID
	Mesh  mesh.Mesh
	// Colors is linear RGB in [0,1], one value per mesh vertex.
	Colors             [][3]float32
	SourcePointIndices []int
}
type DisplayStats struct {
	InputTracks, DisplayedTracks, InputPoints, DisplayedPoints int
	Vertices, Triangles, Bytes                                 int
	DegenerateTracks                                           []TrackID
	LOD                                                        bool
}
type Display struct {
	Lines    []Polyline
	Surfaces []Surface
	Stats    DisplayStats
}
