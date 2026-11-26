package surfaceseg

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/mesh"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/seg"
)

// Read parses a Surface Segmentation data set. It returns
// ErrUnsupportedSOPClass for another SOP Class, ErrResourceLimitExceeded
// beyond the default geometry limits, and ErrInvalidObject when the
// surfaces or their references are inconsistent: a Number of Surfaces or
// Surface Count that disagrees with its sequence, a point count that
// disagrees with the coordinates, an index outside the points, normals
// that do not match the points, invalid Vector Accuracy, or a segment
// referencing a missing surface.
func Read(obj *object.Object) (*Document, error) {
	return ReadWithLimits(obj, ReadLimits{})
}

// ReadWithLimits reads the same geometry as Read, with lower caller-owned
// aggregate limits checked before copying coordinate or index arrays.
func ReadWithLimits(obj *object.Object, limits ReadLimits) (*Document, error) {
	if obj == nil {
		return nil, invalid("data set is nil")
	}
	if sopClass := derivedio.CleanUID(obj, derivedio.TagSOPClassUID); sopClass != SurfaceSegmentationStorage {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedSOPClass, sopClass)
	}
	if err := preflightGeometry(obj, limits); err != nil {
		return nil, err
	}
	doc := &Document{
		SOPInstanceUID:      derivedio.CleanUID(obj, derivedio.TagSOPInstanceUID),
		StudyInstanceUID:    derivedio.CleanUID(obj, derivedio.TagStudyInstanceUID),
		SeriesInstanceUID:   derivedio.CleanUID(obj, derivedio.TagSeriesInstanceUID),
		FrameOfReferenceUID: derivedio.CleanUID(obj, derivedio.TagFrameOfReferenceUID),
		Identification:      derivedio.ReadIdentification(obj),
		ContentDate:         derivedio.CleanString(obj, derivedio.TagContentDate),
		ContentTime:         derivedio.CleanString(obj, derivedio.TagContentTime),
	}
	items := derivedio.Sequence(obj, tagSurfaceSequence)
	if len(items) == 0 {
		return nil, invalid("no Surface Sequence")
	}
	if n, err := obj.GetInt(tagNumberOfSurfaces); err != nil || int(n) != len(items) {
		return nil, invalid("Number of Surfaces does not match the %d Surface Sequence items", len(items))
	}
	numbers := map[int]bool{}
	var points, triangles int64
	for i, item := range items {
		surface, err := readSurface(item, &points, &triangles)
		if err != nil {
			return nil, fmt.Errorf("surface item %d: %w", i+1, err)
		}
		if surface.Number < 1 || numbers[surface.Number] {
			return nil, invalid("Surface Number %d is not unique and positive", surface.Number)
		}
		numbers[surface.Number] = true
		doc.Surfaces = append(doc.Surfaces, surface)
	}
	for _, item := range derivedio.Sequence(obj, tagSegmentSequence) {
		segment, err := readSegment(item)
		if err != nil {
			return nil, err
		}
		for _, ref := range segment.Surfaces {
			if !numbers[ref.Number] {
				return nil, invalid("segment %d references missing surface %d", segment.Number, ref.Number)
			}
		}
		doc.Segments = append(doc.Segments, segment)
	}
	if len(doc.Segments) == 0 {
		return nil, invalid("no Segment Sequence")
	}
	doc.ReferencedImages = readReferencedSeries(obj)
	return doc, nil
}

func readCode(obj *object.Object, tag core.Tag) seg.CodedEntry {
	items := derivedio.Sequence(obj, tag)
	if len(items) == 0 {
		return seg.CodedEntry{}
	}
	return seg.CodedEntry{
		CodeValue:              derivedio.CleanString(items[0], tagCodeValue),
		CodingSchemeDesignator: derivedio.CleanString(items[0], tagCodingSchemeDesignator),
		CodeMeaning:            derivedio.CleanString(items[0], tagCodeMeaning),
	}
}

func readAlgorithm(obj *object.Object, tag core.Tag) Algorithm {
	items := derivedio.Sequence(obj, tag)
	if len(items) == 0 {
		return Algorithm{}
	}
	item := items[0]
	return Algorithm{
		Family:     readCode(item, tagAlgorithmFamilyCodeSeq),
		NameCode:   readCode(item, tagAlgorithmNameCodeSeq),
		Name:       derivedio.CleanString(item, tagAlgorithmName),
		Version:    derivedio.CleanString(item, tagAlgorithmVersion),
		Parameters: derivedio.CleanString(item, tagAlgorithmParameters),
	}
}

func readInstanceReference(item *object.Object) seg.ReferencedImage {
	ref := seg.ReferencedImage{
		SOPClassUID:    derivedio.CleanUID(item, derivedio.TagRefSOPClassUID),
		SOPInstanceUID: derivedio.CleanUID(item, derivedio.TagRefSOPInstanceUID),
	}
	for _, frame := range derivedio.Ints(item, derivedio.TagRefFrameNumber) {
		ref.Frames = append(ref.Frames, int(frame))
	}
	return ref
}

func readSegment(item *object.Object) (Segment, error) {
	segment := Segment{Segment: seg.Segment{
		Number:                    derivedio.Int(item, tagSegmentNumber),
		Label:                     derivedio.CleanString(item, tagSegmentLabel),
		AlgorithmType:             derivedio.CleanString(item, tagSegmentAlgorithmType),
		AlgorithmName:             derivedio.CleanString(item, tagSegmentAlgorithmName),
		SegmentedPropertyCategory: readCode(item, tagSegmentedPropertyCategory),
		SegmentedPropertyType:     readCode(item, tagSegmentedPropertyType),
	}}
	if lab := derivedio.Ints(item, tagRecommendedCIELab); len(lab) >= 3 {
		for k := 0; k < 3; k++ {
			if lab[k] < 0 || lab[k] > math.MaxUint16 {
				return Segment{}, invalid("segment %d CIELab value %d", segment.Number, lab[k])
			}
			segment.RecommendedDisplayCIELab[k] = uint16(lab[k])
		}
	}
	if segment.Number < 1 {
		return Segment{}, invalid("Segment Number %d", segment.Number)
	}
	refs := derivedio.Sequence(item, tagReferencedSurfaceSequence)
	if count, err := item.GetInt(tagSurfaceCount); err != nil || int(count) != len(refs) {
		return Segment{}, invalid("segment %d Surface Count does not match its %d referenced surfaces", segment.Number, len(refs))
	}
	for _, refItem := range refs {
		ref := SurfaceReference{
			Number:    derivedio.Int(refItem, tagReferencedSurfaceNumber),
			Algorithm: readAlgorithm(refItem, tagGenerationAlgorithmSeq),
		}
		for _, source := range derivedio.Sequence(refItem, tagSourceInstanceSequence) {
			ref.SourceInstances = append(ref.SourceInstances, readInstanceReference(source))
		}
		segment.Surfaces = append(segment.Surfaces, ref)
	}
	return segment, nil
}

func readReferencedSeries(obj *object.Object) []seg.ReferencedImage {
	var out []seg.ReferencedImage
	for _, series := range derivedio.Sequence(obj, tagReferencedSeriesSequence) {
		uid := derivedio.CleanUID(series, derivedio.TagSeriesInstanceUID)
		for _, item := range derivedio.Sequence(series, tagReferencedInstanceSequence) {
			ref := readInstanceReference(item)
			ref.SeriesInstanceUID = uid
			out = append(out, ref)
		}
	}
	return out
}

func readSurface(item *object.Object, points, triangles *int64) (Surface, error) {
	s := Surface{
		Number:           derivedio.Int(item, tagSurfaceNumber),
		Comments:         derivedio.CleanString(item, tagSurfaceComments),
		PresentationType: derivedio.CleanString(item, tagPresentationType),
		FiniteVolume:     derivedio.CleanString(item, tagFiniteVolume),
		Manifold:         derivedio.CleanString(item, tagManifold),
		OpacitySet:       true,
	}
	if gray := derivedio.Int(item, tagRecommendedGrayscale); gray >= 0 && gray <= math.MaxUint16 {
		s.RecommendedDisplayGrayscale = uint16(gray)
	}
	if lab := derivedio.Ints(item, tagRecommendedCIELab); len(lab) >= 3 {
		for k := 0; k < 3; k++ {
			if lab[k] < 0 || lab[k] > math.MaxUint16 {
				return s, invalid("CIELab value %d", lab[k])
			}
			s.RecommendedDisplayCIELab[k] = uint16(lab[k])
		}
	}
	if opacity, err := item.GetFloat(tagPresentationOpacity); err == nil {
		s.Opacity = opacity
	} else {
		s.Opacity, s.OpacitySet = 1, false
	}
	if derivedio.CleanString(item, tagSurfaceProcessing) == Yes {
		p := &Processing{Description: derivedio.CleanString(item, tagSurfaceProcessingDescr), Algorithm: readAlgorithm(item, tagProcessingAlgorithmSeq)}
		if ratio, err := item.GetFloat(tagSurfaceProcessingRatio); err == nil {
			p.Ratio = ratio
		}
		s.Processing = p
	}
	pointItems := derivedio.Sequence(item, tagSurfacePointsSequence)
	if len(pointItems) != 1 {
		return s, invalid("surface %d has %d Surface Points Sequence items", s.Number, len(pointItems))
	}
	coordinates, err := float32s(pointItems[0], tagPointCoordinatesData)
	if err != nil {
		return s, invalid("surface %d points: %v", s.Number, err)
	}
	count, err := pointItems[0].GetInt(tagNumberOfSurfacePoints)
	if err != nil || count < 0 || 3*count != int64(len(coordinates)) {
		return s, invalid("surface %d Number of Surface Points does not match %d coordinates", s.Number, len(coordinates))
	}
	if *points += count; *points > MaxReadPoints {
		return s, fmt.Errorf("%w: more than %d points", ErrResourceLimitExceeded, MaxReadPoints)
	}
	m := &mesh.Mesh{Vertices: make([][3]float32, count)}
	for i := range m.Vertices {
		m.Vertices[i] = [3]float32{coordinates[3*i], coordinates[3*i+1], coordinates[3*i+2]}
	}
	if normalItems := derivedio.Sequence(item, tagSurfaceNormalsSequence); len(normalItems) > 0 {
		normalItem := normalItems[0]
		vectors, err := float32s(normalItem, tagVectorCoordinateData)
		if err != nil {
			return s, invalid("surface %d normals: %v", s.Number, err)
		}
		n := derivedio.Int(normalItem, tagNumberOfVectors)
		dimensions := derivedio.Int(normalItem, tagVectorDimensionality)
		if dimensions != 3 || int64(n) != count || len(vectors) != 3*n {
			return s, invalid("surface %d has %d normals of dimension %d for %d points", s.Number, n, dimensions, count)
		}
		if normalItem.Has(tagVectorAccuracy) {
			accuracy, err := float32s(normalItem, tagVectorAccuracy)
			if err != nil {
				return s, invalid("surface %d Vector Accuracy: %v", s.Number, err)
			}
			if err := validateVectorAccuracy(accuracy); err != nil {
				return s, fmt.Errorf("surface %d: %w", s.Number, err)
			}
			if len(accuracy) > 0 {
				s.VectorAccuracy = accuracy
			}
		}
		m.Normals = make([][3]float32, n)
		for i := range m.Normals {
			m.Normals[i] = [3]float32{vectors[3*i], vectors[3*i+1], vectors[3*i+2]}
		}
	}
	primitives := derivedio.Sequence(item, tagMeshPrimitivesSequence)
	if len(primitives) != 1 {
		return s, invalid("surface %d has %d Surface Mesh Primitives Sequence items", s.Number, len(primitives))
	}
	if m.Triangles, s.Primitives, err = readPrimitives(primitives[0], uint32(count), triangles); err != nil {
		return s, fmt.Errorf("surface %d: %w", s.Number, err)
	}
	s.RetiredIndexLists = onlyRetiredIndexLists(primitives[0])
	if err := m.Validate(); err != nil {
		return s, invalid("surface %d: %v", s.Number, err)
	}
	s.Mesh = m
	if len(m.Vertices) == 0 || len(m.Triangles) == 0 && s.Primitives.Count() == 0 {
		return s, invalid("surface %d has no points or mesh primitives", s.Number)
	}
	return s, nil
}

// readPrimitives returns the triangles of the triangle list, strips, fans
// and facets (0-based), and preserved vertex, edge and line primitives.
func readPrimitives(item *object.Object, points uint32, total *int64) ([][3]uint32, Primitives, error) {
	var out [][3]uint32
	add := func(a, b, c uint32) error {
		if a == b || b == c || a == c {
			return nil // a degenerate strip or fan joint
		}
		if *total++; *total > MaxReadTriangles {
			return fmt.Errorf("%w: more than %d triangles", ErrResourceLimitExceeded, MaxReadTriangles)
		}
		out = append(out, [3]uint32{a, b, c})
		return nil
	}
	list, err := indexList(item, tagLongTrianglePointIndex, tagTrianglePointIndexList, points)
	if err != nil {
		return nil, Primitives{}, err
	}
	if len(list)%3 != 0 {
		return nil, Primitives{}, invalid("triangle index list of %d values", len(list))
	}
	if int64(len(list)/3) > MaxReadTriangles-*total {
		return nil, Primitives{}, fmt.Errorf("%w: more than %d triangles", ErrResourceLimitExceeded, MaxReadTriangles)
	}
	out = make([][3]uint32, 0, len(list)/3)
	for i := 0; i+2 < len(list); i += 3 {
		if err := add(list[i], list[i+1], list[i+2]); err != nil {
			return nil, Primitives{}, err
		}
	}
	primitive := func(tag core.Tag, emit func([]uint32) error) error {
		for _, p := range derivedio.Sequence(item, tag) {
			indices, err := indexList(p, tagLongPrimitivePointIndex, tagPrimitivePointIndexList, points)
			if err != nil {
				return err
			}
			if len(indices) < 3 {
				return invalid("primitive of %d points", len(indices))
			}
			if err := emit(indices); err != nil {
				return err
			}
		}
		return nil
	}
	// Strips alternate their winding so every triangle keeps the first's.
	if err := primitive(tagTriangleStripSequence, func(v []uint32) error {
		for i := 2; i < len(v); i++ {
			a, b := v[i-2], v[i-1]
			if i%2 == 1 {
				a, b = b, a
			}
			if err := add(a, b, v[i]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, Primitives{}, err
	}
	fan := func(v []uint32) error {
		for i := 2; i < len(v); i++ {
			if err := add(v[0], v[i-1], v[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := primitive(tagTriangleFanSequence, fan); err != nil {
		return nil, Primitives{}, err
	}
	if err := primitive(tagFacetSequence, fan); err != nil {
		return nil, Primitives{}, err
	}
	var preserved Primitives
	preserved.Vertices, err = indexList(item, tagLongVertexPointIndex, tagVertexPointIndexList, points)
	if err != nil {
		return nil, Primitives{}, err
	}
	edges, err := indexList(item, tagLongEdgePointIndex, tagEdgePointIndexList, points)
	if err != nil {
		return nil, Primitives{}, err
	}
	if len(edges)%2 != 0 {
		return nil, Primitives{}, invalid("edge index list has incomplete pair (%d indices)", len(edges))
	}
	for i := 0; i < len(edges); i += 2 {
		preserved.Edges = append(preserved.Edges, Edge{edges[i], edges[i+1]})
	}
	for _, line := range derivedio.Sequence(item, tagLineSequence) {
		v, err := indexList(line, tagLongPrimitivePointIndex, tagPrimitivePointIndexList, points)
		if err != nil {
			return nil, Primitives{}, err
		}
		preserved.Lines = append(preserved.Lines, Polyline(v))
	}
	if err := preserved.validate(points); err != nil {
		return nil, Primitives{}, err
	}
	return out, preserved, nil
}

func onlyRetiredIndexLists(item *object.Object) bool {
	retired := false
	for _, tags := range [][2]core.Tag{{tagLongVertexPointIndex, tagVertexPointIndexList}, {tagLongEdgePointIndex, tagEdgePointIndexList}, {tagLongTrianglePointIndex, tagTrianglePointIndexList}} {
		if item.Has(tags[0]) {
			return false
		}
		retired = retired || item.Has(tags[1])
	}
	for _, tag := range []core.Tag{tagLineSequence, tagTriangleStripSequence, tagTriangleFanSequence, tagFacetSequence} {
		for _, primitive := range derivedio.Sequence(item, tag) {
			if primitive.Has(tagLongPrimitivePointIndex) {
				return false
			}
			retired = retired || primitive.Has(tagPrimitivePointIndexList)
		}
	}
	return retired
}

// indexList reads a Long (OL) or retired (OW) point index list as 0-based
// indices, each checked against the point count.
func indexList(item *object.Object, long, retired core.Tag, points uint32) ([]uint32, error) {
	tag, err := selectIndexTag(item, long, retired)
	if err != nil {
		return nil, err
	}
	order := item.ValueByteOrder()
	var out []uint32
	if element, ok := item.Get(tag); ok && tag == long {
		switch value := element.Value.(type) {
		case core.Uint32Value:
			out = append(out, value...)
		default:
			raw, ok := element.RawBytes()
			if !ok || len(raw)%4 != 0 {
				return nil, invalid("%s is not a list of 32-bit indices", long)
			}
			out = make([]uint32, len(raw)/4)
			for i := range out {
				out[i] = order.Uint32(raw[4*i:])
			}
		}
	} else if element, ok := item.Get(tag); ok {
		switch value := element.Value.(type) {
		case core.Uint16Value:
			for _, v := range value {
				out = append(out, uint32(v))
			}
		default:
			raw, ok := element.RawBytes()
			if !ok || len(raw)%2 != 0 {
				return nil, invalid("%s is not a list of 16-bit indices", retired)
			}
			out = make([]uint32, len(raw)/2)
			for i := range out {
				out[i] = uint32(order.Uint16(raw[2*i:]))
			}
		}
	}
	for i, v := range out {
		if v < 1 || v > points {
			return nil, invalid("point index %d outside 1..%d", v, points)
		}
		out[i] = v - 1
	}
	return out, nil
}

// float32s reads an OF (or FL) element.
func float32s(item *object.Object, tag core.Tag) ([]float32, error) {
	element, ok := item.Get(tag)
	if !ok {
		return nil, fmt.Errorf("%s is missing", tag)
	}
	var out []float32
	switch value := element.Value.(type) {
	case core.Float32Value:
		out = append(out, value...)
	default:
		raw, ok := element.RawBytes()
		if !ok || len(raw)%4 != 0 {
			return nil, fmt.Errorf("%s is not a list of 32-bit floats", tag)
		}
		order := item.ValueByteOrder()
		out = make([]float32, len(raw)/4)
		for i := range out {
			out[i] = math.Float32frombits(order.Uint32(raw[4*i:]))
		}
	}
	for _, v := range out {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("%s holds a value that is not finite", tag)
		}
	}
	return out, nil
}
