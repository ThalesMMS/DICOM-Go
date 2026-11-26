package surfaceseg

import (
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/mesh"
	"github.com/ThalesMMS/dicom-go/seg"
)

// MeshSegment is one segment of FromMeshes: its description and its
// surface.
type MeshSegment struct {
	// Segment is the Segment Description; a zero Number is the position
	// (1-based), an empty AlgorithmType MANUAL, empty property codes the
	// generic ROI codes of package seg, and a zero CIELab colour comes
	// from ColorRGB.
	Segment seg.Segment
	// ColorRGB is the sRGB display colour when the segment has no CIELab
	// colour.
	ColorRGB [3]uint8
	// Mesh is the surface in patient LPS millimetres.
	Mesh *mesh.Mesh
	// Opacity is the recommended opacity in [0, 1]; zero means 1.
	Opacity float64
}

// FromMeshesOptions builds a document from segment meshes.
type FromMeshesOptions struct {
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	Segments            []MeshSegment
	// Algorithm generated every surface (Segment Surface Generation
	// Algorithm Identification); see SurfaceNetsAlgorithm.
	Algorithm Algorithm
	// SourceImages are the images the segments were drawn on: the Common
	// Instance Reference and every surface's Segment Surface Source
	// Instance Sequence.
	SourceImages []seg.ReferencedImage
}

// SurfaceNetsAlgorithm identifies surfaces built by package mesh's
// ExtractLabels (multi-label SurfaceNets): Algorithm Family
// (123101, DCM, "Neighborhood Analysis") of CID 7162.
func SurfaceNetsAlgorithm(version string) Algorithm {
	return Algorithm{
		Family:  seg.CodedEntry{CodeValue: "123101", CodingSchemeDesignator: "DCM", CodeMeaning: "Neighborhood Analysis"},
		Name:    "SurfaceNets",
		Version: version,
	}
}

// FromMeshes builds a document with one surface per segment, numbered in
// order. Finite Volume and Manifold come from each mesh's closure.
func FromMeshes(opts FromMeshesOptions) (*Document, error) {
	doc := &Document{
		SOPInstanceUID:      opts.SOPInstanceUID,
		StudyInstanceUID:    opts.StudyInstanceUID,
		SeriesInstanceUID:   opts.SeriesInstanceUID,
		FrameOfReferenceUID: opts.FrameOfReferenceUID,
		ReferencedImages:    append([]seg.ReferencedImage(nil), opts.SourceImages...),
	}
	for i, part := range opts.Segments {
		if part.Mesh == nil {
			return nil, invalid("segment %d has no mesh", i+1)
		}
		segment := part.Segment
		if segment.Number == 0 {
			segment.Number = i + 1
		}
		if strings.TrimSpace(segment.Label) == "" {
			segment.Label = fmt.Sprintf("Segment %d", segment.Number)
		}
		if segment.AlgorithmType == "" {
			segment.AlgorithmType = seg.AlgorithmManual
		}
		if segment.SegmentedPropertyCategory == (seg.CodedEntry{}) {
			segment.SegmentedPropertyCategory = seg.GenericROICategoryCode()
		}
		if segment.SegmentedPropertyType == (seg.CodedEntry{}) {
			segment.SegmentedPropertyType = seg.GenericROITypeCode()
		}
		if segment.RecommendedDisplayCIELab == ([3]uint16{}) {
			c := part.ColorRGB
			segment.RecommendedDisplayCIELab = seg.RGBToDICOMCIELab([3]int{int(c[0]), int(c[1]), int(c[2])})
		}
		sources := make([]seg.ReferencedImage, len(opts.SourceImages))
		for k, source := range opts.SourceImages {
			sources[k] = seg.ReferencedImage{SOPClassUID: source.SOPClassUID, SOPInstanceUID: source.SOPInstanceUID, Frames: source.Frames}
		}
		number := len(doc.Surfaces) + 1
		surface := Surface{Number: number, Mesh: part.Mesh, RecommendedDisplayCIELab: segment.RecommendedDisplayCIELab, Opacity: part.Opacity}
		doc.Surfaces = append(doc.Surfaces, surface)
		doc.Segments = append(doc.Segments, Segment{Segment: segment, Surfaces: []SurfaceReference{{Number: number, Algorithm: opts.Algorithm, SourceInstances: sources}}})
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

// SegmentMesh returns the surfaces of segment number merged into one mesh
// (mesh.Merge) and its sRGB display colour.
func (d *Document) SegmentMesh(number int) (*mesh.Mesh, [3]uint8, bool) {
	if d == nil {
		return nil, [3]uint8{}, false
	}
	for _, segment := range d.Segments {
		if segment.Number != number {
			continue
		}
		var meshes []*mesh.Mesh
		lab := segment.RecommendedDisplayCIELab
		for _, ref := range segment.Surfaces {
			if surface, ok := d.Surface(ref.Number); ok && surface.Mesh != nil {
				meshes = append(meshes, surface.Mesh)
				if lab == ([3]uint16{}) {
					lab = surface.RecommendedDisplayCIELab
				}
			}
		}
		if len(meshes) == 0 {
			return nil, [3]uint8{}, false
		}
		merged := meshes[0]
		if len(meshes) > 1 {
			merged, _ = mesh.Merge(meshes, nil)
		}
		return merged, labToRGB(lab), true
	}
	return nil, [3]uint8{}, false
}

// SegmentPrimitives preserves every referenced surface's non-triangle
// primitives in reference order, remapping indices to SegmentMesh's points.
// It does not generate triangles or assign solid-volume semantics.
func (d *Document) SegmentPrimitives(number int) Primitives {
	var result Primitives
	if d == nil {
		return result
	}
	for _, segment := range d.Segments {
		if segment.Number != number {
			continue
		}
		var offset uint32
		for _, ref := range segment.Surfaces {
			s, ok := d.Surface(ref.Number)
			if !ok || s.Mesh == nil {
				continue
			}
			for _, v := range s.Primitives.Vertices {
				result.Vertices = append(result.Vertices, v+offset)
			}
			for _, e := range s.Primitives.Edges {
				result.Edges = append(result.Edges, Edge{e[0] + offset, e[1] + offset})
			}
			for _, line := range s.Primitives.Lines {
				copy := make(Polyline, len(line))
				for i, v := range line {
					copy[i] = v + offset
				}
				result.Lines = append(result.Lines, copy)
			}
			offset += uint32(len(s.Mesh.Vertices))
		}
		break
	}
	return result
}

// Parts returns one coloured part per segment that has surfaces, named by
// its label, for mesh.WriteGLB, WriteOBJParts and WriteMTL. This historical
// mesh-only projection does not carry Primitives. Use TriangleParts to refuse
// a lossy projection of a mixed or non-triangular document before export.
func (d *Document) Parts() []mesh.Part {
	if d == nil {
		return nil
	}
	var parts []mesh.Part
	for _, segment := range d.Segments {
		if m, color, ok := d.SegmentMesh(segment.Number); ok {
			parts = append(parts, mesh.Part{Name: segment.Label, Mesh: m, Color: color})
		}
	}
	return parts
}

// TriangleParts returns mesh parts only when that export preserves every
// primitive. It refuses points, edges and lines rather than triangulating
// them or silently omitting them from a triangle-only file such as STL.
func (d *Document) TriangleParts() ([]mesh.Part, error) {
	if d == nil {
		return nil, invalid("document is nil")
	}
	for _, s := range d.Surfaces {
		if s.Primitives.Count() != 0 {
			return nil, fmt.Errorf("%w: surface %d", ErrNonTrianglePrimitives, s.Number)
		}
	}
	return d.Parts(), nil
}

func labToRGB(lab [3]uint16) [3]uint8 {
	rgb := seg.DICOMCIELabToRGB(lab)
	var out [3]uint8
	for k, v := range rgb {
		out[k] = uint8(max(0, min(255, v)))
	}
	return out
}
