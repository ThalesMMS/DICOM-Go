package surfaceseg

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// Edge is an ordered pair of zero-based indices into Surface.Mesh.Vertices.
type Edge [2]uint32

// Polyline is a directed path of at least two zero-based point indices.
// A closed path may repeat its first point at the end.
type Polyline []uint32

// Primitives preserves non-triangular geometry in its encoded order. The
// points live in Surface.Mesh.Vertices, in patient LPS millimetres. These
// primitives do not imply an area, an enclosed volume or a triangle mesh.
type Primitives struct {
	Vertices []uint32
	Edges    []Edge
	Lines    []Polyline
}

// Count returns the number of explicit vertex, edge and line primitives.
func (p Primitives) Count() int { return len(p.Vertices) + len(p.Edges) + len(p.Lines) }

func (p Primitives) validate(points uint32) error {
	check := func(v uint32) error {
		if v >= points {
			return invalid("primitive point index %d outside 0..%d", v, points-1)
		}
		return nil
	}
	for _, v := range p.Vertices {
		if err := check(v); err != nil {
			return err
		}
	}
	for _, e := range p.Edges {
		if err := check(e[0]); err != nil {
			return err
		}
		if err := check(e[1]); err != nil {
			return err
		}
	}
	for _, line := range p.Lines {
		if len(line) < 2 {
			return invalid("line has %d points; at least two required", len(line))
		}
		for _, v := range line {
			if err := check(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func primitiveSequence(s Surface) core.Element {
	indices := func(long, retired core.Tag, v []uint32) core.Element {
		if s.RetiredIndexLists {
			data := make([]byte, 2*len(v))
			for i, value := range v {
				binary.LittleEndian.PutUint16(data[2*i:], uint16(value+1))
			}
			return derivedio.Raw(retired, core.VROW, data)
		}
		data := make([]byte, 4*len(v))
		for i, value := range v {
			binary.LittleEndian.PutUint32(data[4*i:], value+1)
		}
		return derivedio.Raw(long, core.VROL, data)
	}
	edges := make([]uint32, 0, 2*len(s.Primitives.Edges))
	for _, e := range s.Primitives.Edges {
		edges = append(edges, e[0], e[1])
	}
	triangles := make([]uint32, 0, 3*len(s.Mesh.Triangles))
	for _, t := range s.Mesh.Triangles {
		triangles = append(triangles, t[0], t[1], t[2])
	}
	lines := make([]core.DataSet, len(s.Primitives.Lines))
	for i, line := range s.Primitives.Lines {
		lines[i] = derivedio.DataSet(indices(tagLongPrimitivePointIndex, tagPrimitivePointIndexList, line))
	}
	return derivedio.Seq(tagMeshPrimitivesSequence, derivedio.DataSet(
		indices(tagLongVertexPointIndex, tagVertexPointIndexList, s.Primitives.Vertices),
		indices(tagLongEdgePointIndex, tagEdgePointIndexList, edges),
		indices(tagLongTrianglePointIndex, tagTrianglePointIndexList, triangles),
		derivedio.Seq(tagTriangleStripSequence), derivedio.Seq(tagTriangleFanSequence),
		derivedio.Seq(tagLineSequence, lines...), derivedio.Seq(tagFacetSequence),
	))
}

// ReadLimits bounds aggregate geometry before Read copies numeric arrays.
// Zero selects the default; negative limits are invalid. MaxBytes charges
// geometry payloads, reconstructed arrays and line slice headers. It excludes
// the caller-owned Object and unrelated patient/segment metadata.
type ReadLimits struct {
	MaxPoints, MaxTriangles, MaxPrimitives, MaxIndices, MaxBytes int64
}

func validateWriteGeometryResources(surfaces []Surface) error {
	var points, triangles, primitives, indices, bytes int64
	for _, s := range surfaces {
		if s.Mesh == nil {
			return invalid("surface has no point data")
		}
		points += int64(len(s.Mesh.Vertices))
		triangles += int64(len(s.Mesh.Triangles))
		primitives += int64(s.Primitives.Count()) + int64(len(s.Mesh.Triangles))
		indices += int64(len(s.Primitives.Vertices)) + 2*int64(len(s.Primitives.Edges)) + 3*int64(len(s.Mesh.Triangles))
		bytes += 24 * int64(len(s.Mesh.Vertices)+len(s.Mesh.Normals))
		bytes += 8 * int64(len(s.VectorAccuracy))
		bytes += 36 * int64(len(s.Mesh.Triangles))
		bytes += 8*int64(len(s.Primitives.Vertices)) + 24*int64(len(s.Primitives.Edges)) + 24*int64(len(s.Primitives.Lines))
		for _, line := range s.Primitives.Lines {
			indices += int64(len(line))
			bytes += 8 * int64(len(line))
		}
		if points > MaxReadPoints || triangles > MaxReadTriangles || primitives > MaxReadPrimitives || indices > MaxReadIndices || bytes > MaxReadGeometryBytes {
			return fmt.Errorf("%w: document geometry exceeds default read/write limits", ErrResourceLimitExceeded)
		}
	}
	return nil
}

func (l ReadLimits) defaults() (ReadLimits, error) {
	defaults := []int64{MaxReadPoints, MaxReadTriangles, MaxReadPrimitives, MaxReadIndices, MaxReadGeometryBytes}
	fields := []*int64{&l.MaxPoints, &l.MaxTriangles, &l.MaxPrimitives, &l.MaxIndices, &l.MaxBytes}
	for i, field := range fields {
		if *field < 0 {
			return l, invalid("negative geometry resource limit")
		}
		if *field == 0 {
			*field = defaults[i]
		}
		if *field > defaults[i] {
			*field = defaults[i]
		}
	}
	return l, nil
}

// preflightGeometry admits geometry before allocating decoded point/index
// lists. It counts the triangle expansion of strips/fans conservatively.
func preflightGeometry(obj *object.Object, limits ReadLimits) error {
	l, err := limits.defaults()
	if err != nil {
		return err
	}
	var points, triangles, primitives, indices, bytes int64
	check := func() error {
		if points > l.MaxPoints || triangles > l.MaxTriangles || primitives > l.MaxPrimitives || indices > l.MaxIndices || bytes > l.MaxBytes {
			return fmt.Errorf("%w: geometry points=%d triangles=%d primitives=%d indices=%d bytes=%d", ErrResourceLimitExceeded, points, triangles, primitives, indices, bytes)
		}
		return nil
	}
	indexCount := func(item *object.Object, long, retired core.Tag) (int64, error) {
		tag, err := selectIndexTag(item, long, retired)
		if err != nil {
			return 0, err
		}
		width := int64(4)
		if tag == retired {
			width = 2
		}
		n, err := geometryValueBytes(item, tag)
		if err != nil {
			return 0, err
		}
		if n%width != 0 {
			return 0, invalid("%s has incomplete indices", tag)
		}
		indices += n / width
		bytes += n + 4*(n/width)
		if err := check(); err != nil {
			return 0, err
		}
		return n / width, nil
	}
	for _, surface := range derivedio.Sequence(obj, tagSurfaceSequence) {
		for _, item := range derivedio.Sequence(surface, tagSurfacePointsSequence) {
			n, err := geometryValueBytes(item, tagPointCoordinatesData)
			if err != nil {
				return err
			}
			declared, err := item.GetInt(tagNumberOfSurfacePoints)
			if err != nil || declared < 0 {
				return invalid("invalid Number of Surface Points")
			}
			if declared > l.MaxPoints-points {
				return fmt.Errorf("%w: more than %d points", ErrResourceLimitExceeded, l.MaxPoints)
			}
			points += declared
			bytes += n + 12*declared
			if err := check(); err != nil {
				return err
			}
		}
		if element, ok := surface.Get(tagSurfaceNormalsSequence); ok {
			sequence, isSequence := element.Value.(core.SequenceValue)
			if !isSequence || element.VR() != core.VRSQ || len(sequence.Items) > 1 {
				return invalid("Surface Points Normals Sequence must be SQ with zero or one item")
			}
		}
		for _, item := range derivedio.Sequence(surface, tagSurfaceNormalsSequence) {
			n, err := geometryValueBytes(item, tagVectorCoordinateData)
			if err != nil {
				return err
			}
			accuracyBytes, err := vectorAccuracyBytes(item)
			if err != nil {
				return err
			}
			bytes += 2 * (n + accuracyBytes)
			if err := check(); err != nil {
				return err
			}
		}
		for _, item := range derivedio.Sequence(surface, tagMeshPrimitivesSequence) {
			v, err := indexCount(item, tagLongVertexPointIndex, tagVertexPointIndexList)
			if err != nil {
				return err
			}
			e, err := indexCount(item, tagLongEdgePointIndex, tagEdgePointIndexList)
			if err != nil {
				return err
			}
			t, err := indexCount(item, tagLongTrianglePointIndex, tagTrianglePointIndexList)
			if err != nil {
				return err
			}
			primitives += v + e/2 + t/3
			triangles += t / 3
			bytes += 4*e + 4*t
			if err := check(); err != nil {
				return err
			}
			for _, tag := range []core.Tag{tagLineSequence, tagTriangleStripSequence, tagTriangleFanSequence, tagFacetSequence} {
				for _, primitive := range derivedio.Sequence(item, tag) {
					n, err := indexCount(primitive, tagLongPrimitivePointIndex, tagPrimitivePointIndexList)
					if err != nil {
						return err
					}
					if tag == tagLineSequence {
						primitives++
						bytes += 24
					} else {
						triangles += max(0, n-2)
						primitives += max(0, n-2)
						bytes += 12 * max(0, n-2)
					}
					if err := check(); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func vectorAccuracyBytes(item *object.Object) (int64, error) {
	element, ok := item.Get(tagVectorAccuracy)
	if !ok {
		return 0, nil
	}
	if element.VR() != core.VRFL {
		return 0, invalid("Vector Accuracy requires FL")
	}
	bytes, err := geometryValueBytes(item, tagVectorAccuracy)
	if err != nil {
		return 0, fmt.Errorf("Vector Accuracy: %w", err)
	}
	if bytes != 0 && bytes != 4 && bytes != 12 {
		return 0, invalid("Vector Accuracy has %d bytes; expected one shared value or three XYZ values", bytes)
	}
	return bytes, nil
}

func geometryValueBytes(item *object.Object, tag core.Tag) (int64, error) {
	e, ok := item.Get(tag)
	if !ok {
		return 0, nil
	}
	if e.Value == nil {
		return 0, invalid("%s has no value", tag)
	}
	n, inline := e.Value.EncodedLength()
	if !inline || uint64(n) > math.MaxInt64 {
		return 0, invalid("%s has no bounded inline value", tag)
	}
	return int64(n), nil
}

// Never prefer one populated representation and silently drop another.
// Empty Long attributes can coexist with a populated retired attribute.
func selectIndexTag(item *object.Object, long, retired core.Tag) (core.Tag, error) {
	longBytes, err := geometryValueBytes(item, long)
	if err != nil {
		return long, err
	}
	retiredBytes, err := geometryValueBytes(item, retired)
	if err != nil {
		return retired, err
	}
	if longBytes != 0 && retiredBytes != 0 {
		return long, invalid("both %s and %s contain point indices", long, retired)
	}
	if retiredBytes != 0 || !item.Has(long) {
		return retired, nil
	}
	return long, nil
}
