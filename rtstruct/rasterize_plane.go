package rtstruct

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
	dicomroi "github.com/ThalesMMS/dicom-go/roi"
)

// SlicePreviewOptions identifies the target image and supplies its patient
// geometry. Rows and Columns describe the mask grid even when the image has
// no usable patient geometry. PlaneToleranceMM is the maximum distance of
// any vertex of an unreferenced contour from the slice.
type SlicePreviewOptions struct {
	SOPInstanceUID   string
	Geometry         render.SliceGeometry
	PlaneToleranceMM float64
}

// SlicePreview contains the detached sub-pixel outlines and filled mask for a
// single ROI on a slice. The mask is nil when the grid is unavailable or no
// pixel centres are covered.
type SlicePreview struct {
	Contours [][]dicomroi.Point2D
	Mask     *dicomroi.RasterMask
	// PartialGeometry reports unsupported or mixed contour types in the ROI.
	PartialGeometry bool
	// MissingGeometry reports use of direct X/Y coordinates for display.
	MissingGeometry bool
}

// PreviewSlice places one ROI's contours on the slice for display. Explicit
// image references take precedence over plane distance. An unreferenced
// contour requires valid patient geometry and all its vertices must be within
// the plane tolerance. A matching referenced contour can use its raw X/Y
// coordinates for display if patient geometry is unavailable.
//
// The first supported closed contour type sets the fill mode for the ROI.
// Unsupported types and contours of the other fill mode are skipped and
// reported as partial geometry. Use Rasterize for strict volume conversion.
func PreviewSlice(contours []Contour, opts SlicePreviewOptions) SlicePreview {
	fill, partial := previewContourFill(contours)
	result := SlicePreview{PartialGeometry: partial}
	if fill == contourUnsupported {
		return result
	}

	geometry := opts.Geometry
	hasGeometry := validProjectionGeometry(geometry)
	for _, contour := range contours {
		if contourFillFor(contour.GeometricType) != fill || len(contour.Points) < 3 {
			continue
		}
		tolerance := opts.PlaneToleranceMM
		if len(contour.ReferencedImages) > 0 {
			if !referencesSOPInstance(contour, opts.SOPInstanceUID) {
				continue
			}
			tolerance = math.Inf(1)
		} else if !hasGeometry || tolerance < 0 || !finiteCoordinate(tolerance) {
			continue
		}

		var points []dicomroi.Point2D
		if hasGeometry {
			projected, err := projectContour(contour.Points, geometry, tolerance)
			if err != nil {
				continue
			}
			points = projected
		} else {
			points = previewXY(contour.Points)
			if len(points) == 0 {
				continue
			}
			result.MissingGeometry = true
		}
		result.Contours = append(result.Contours, points)

		if geometry.Columns > 0 && geometry.Rows > 0 {
			if result.Mask == nil {
				result.Mask = dicomroi.NewRasterMask(geometry.Columns, geometry.Rows)
			}
			fill.combine(result.Mask, points)
		}
	}
	if result.Mask != nil && result.Mask.Empty() {
		result.Mask = nil
	}
	return result
}

func referencesSOPInstance(contour Contour, sopInstanceUID string) bool {
	if sopInstanceUID == "" {
		return false
	}
	for _, ref := range contour.ReferencedImages {
		if ref.SOPInstanceUID == sopInstanceUID {
			return true
		}
	}
	return false
}

func previewXY(points []Point3D) []dicomroi.Point2D {
	out := make([]dicomroi.Point2D, 0, len(points))
	for _, point := range points {
		if !finiteCoordinate(point.X) || !finiteCoordinate(point.Y) {
			return nil
		}
		out = append(out, dicomroi.Point2D{X: point.X, Y: point.Y})
	}
	return out
}

func validProjectionGeometry(geometry render.SliceGeometry) bool {
	return finitePositive(geometry.RowSpacing) && finitePositive(geometry.ColSpacing) &&
		finiteVecComponents(geometry.Origin) && finiteVector(geometry.RowDir) &&
		finiteVector(geometry.ColDir) && finiteVector(geometry.Normal)
}

func projectContour(points []Point3D, geometry render.SliceGeometry, planeToleranceMM float64) ([]dicomroi.Point2D, error) {
	rowDir := geometry.RowDir.Normalize()
	colDir := geometry.ColDir.Normalize()
	normal := geometry.Normal.Normalize()
	out := make([]dicomroi.Point2D, 0, len(points))
	for _, point := range points {
		delta := render.Vec3{X: point.X, Y: point.Y, Z: point.Z}.Sub(geometry.Origin)
		distance := math.Abs(delta.Dot(normal))
		if !finiteCoordinate(distance) || distance > planeToleranceMM {
			return nil, fmt.Errorf("%w: contour point is %.6g mm from plane (tolerance %.6g mm)", ErrGeometryMismatch, distance, planeToleranceMM)
		}
		x := delta.Dot(rowDir) / geometry.ColSpacing
		y := delta.Dot(colDir) / geometry.RowSpacing
		if !finiteCoordinate(x) || !finiteCoordinate(y) {
			return nil, fmt.Errorf("%w: projected contour point is not finite", ErrGeometryMismatch)
		}
		out = append(out, dicomroi.Point2D{X: x, Y: y})
	}
	return out, nil
}

type contourFill int

const (
	contourUnsupported contourFill = iota
	contourUnion
	contourXOR
)

func contourFillFor(geometricType string) contourFill {
	switch geometricType {
	case "", ContourClosedPlanar:
		return contourUnion
	case ContourClosedPlanarXOR:
		return contourXOR
	default:
		return contourUnsupported
	}
}

func strictContourFill(contours []Contour) (contourFill, error) {
	fill := contourUnion
	for _, contour := range contours {
		switch contourFillFor(contour.GeometricType) {
		case contourXOR:
			fill = contourXOR
		case contourUnsupported:
			return contourUnsupported, fmt.Errorf("%w: %s", ErrUnsupportedContourType, contour.GeometricType)
		}
	}
	if fill == contourXOR {
		for _, contour := range contours {
			if contourFillFor(contour.GeometricType) != contourXOR {
				return contourUnsupported, fmt.Errorf("%w: CLOSEDPLANAR_XOR cannot be mixed with %q", ErrGeometryMismatch, contour.GeometricType)
			}
		}
	}
	return fill, nil
}

func previewContourFill(contours []Contour) (contourFill, bool) {
	fill := contourUnsupported
	partial := false
	for _, contour := range contours {
		mode := contourFillFor(contour.GeometricType)
		switch {
		case mode == contourUnsupported:
			partial = true
		case fill == contourUnsupported:
			fill = mode
		case fill != mode:
			partial = true
		}
	}
	return fill, partial
}

func (fill contourFill) combine(mask *dicomroi.RasterMask, points []dicomroi.Point2D) {
	contour := dicomroi.RasterizePolygon(points, mask.Columns, mask.Rows)
	if fill == contourXOR {
		mask.XOR(contour)
	} else {
		mask.Union(contour)
	}
}
