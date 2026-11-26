package rtstruct

import (
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// ErrTransform reports contours that a registration cannot map.
var ErrTransform = errors.New("rtstruct: contour points cannot be mapped")

// Transform returns a copy of the structure set with every contour point
// mapped by mapper into frameOfReferenceUID (#1089), as a Spatial
// Registration maps points of the structure set's Frame of Reference into
// another one. The copy has no SOP identity, and its contours reference no
// images, since the source images belong to the other Frame of Reference.
// It fails closed: a point the mapper refuses, or maps to a non-finite
// point, fails the whole transform. A non-rigid mapping may take planar
// contours out of their planes; callers that need slice-planar contours
// rasterize the source structure set and resample the labels instead.
func (s *StructureSet) Transform(frameOfReferenceUID string, mapper func(render.Vec3) (render.Vec3, error)) (*StructureSet, error) {
	if s == nil || mapper == nil {
		return nil, fmt.Errorf("%w: missing structure set or mapper", ErrTransform)
	}
	out := *s
	out.SOPInstanceUID, out.SeriesInstanceUID = "", ""
	out.FrameOfReferenceUID = frameOfReferenceUID
	out.ROIs = make([]ROI, len(s.ROIs))
	for i, roi := range s.ROIs {
		roi.Contours = make([]Contour, len(s.ROIs[i].Contours))
		for j, contour := range s.ROIs[i].Contours {
			points := make([]Point3D, len(contour.Points))
			for k, p := range contour.Points {
				q, err := mapper(render.Vec3{X: p.X, Y: p.Y, Z: p.Z})
				if err != nil {
					return nil, fmt.Errorf("%w: ROI %d contour %d point %d: %v", ErrTransform, roi.Number, j+1, k+1, err)
				}
				if math.IsNaN(q.X+q.Y+q.Z) || math.IsInf(q.X+q.Y+q.Z, 0) {
					return nil, fmt.Errorf("%w: ROI %d contour %d point %d maps to a non-finite point", ErrTransform, roi.Number, j+1, k+1)
				}
				points[k] = Point3D{X: q.X, Y: q.Y, Z: q.Z}
			}
			roi.Contours[j] = Contour{GeometricType: contour.GeometricType, Points: points}
		}
		out.ROIs[i] = roi
	}
	return &out, nil
}
