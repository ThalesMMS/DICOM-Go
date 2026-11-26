package mesh

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
)

// Binary glTF 2.0 export (#1183), after the Khronos glTF 2.0 specification
// (GLB container, section 4.4) and VTK's vtkGLTFExporter for the scope: one
// glTF mesh per part with positions, normals and triangle indices, one
// metallic-roughness material per part in its colour, and a root node that
// carries the millimetre-to-metre scale. Written from the specification; no
// VTK code is copied.
//
// The vertex data stay in patient LPS millimetres, exactly the float32
// values of the mesh (as in the STL, PLY and OBJ writers). glTF's unit is
// the metre and its up axis +Y, so the root node "patient" scales by 0.001
// and, unless GLBOptions.PatientAxes is set, turns the patient frame so
// superior is +Y and anterior +Z (the front of a glTF asset): a proper
// rotation of -90° about the left axis, which keeps the outward winding.

// Part is one named, coloured mesh of a multi-part model file, such as one
// segment of a segmentation.
type Part struct {
	Name string
	Mesh *Mesh
	// Color is the part's sRGB colour (0..255).
	Color [3]uint8
}

// GLBOptions configures WriteGLB.
type GLBOptions struct {
	// Generator names the writing application (asset.generator); empty uses
	// "dicom-go mesh".
	Generator string
	// Comment is stored as asset.extras.comment.
	Comment string
	// PatientAxes keeps the patient LPS axes in the root node, which then
	// only scales millimetres to metres; by default it also turns superior
	// to glTF's +Y up.
	PatientAxes bool
	// VertexScalars adds custom FLOAT/SCALAR attributes to each Part, named
	// "_"+Name. Nil adds none; otherwise the outer length must equal parts.
	// Optional units are stored in accessor extras and are not transformed
	// by the root node. Validation, including float32 conversion, precedes
	// all output. Limits are MaxVertexScalarAttributes per part and
	// MaxGLBScalarAttributes across this call.
	VertexScalars [][]VertexScalarAttribute
}

// glTF constants (specification section 5).
const (
	glbMagic          = 0x46546C67 // "glTF"
	glbVersion        = 2
	glbChunkJSON      = 0x4E4F534A // "JSON"
	glbChunkBIN       = 0x004E4942 // "BIN\0"
	gltfFloat         = 5126
	gltfUnsignedShort = 5123
	gltfUnsignedInt   = 5125
	gltfArrayBuffer   = 34962
	gltfElementBuffer = 34963
	gltfTriangles     = 4
	// gltfShortIndexLimit is the largest vertex count indexed with
	// UNSIGNED_SHORT: 65535 is the primitive-restart value, which glTF
	// forbids as an index.
	gltfShortIndexLimit = 65535
)

type gltfDocument struct {
	Asset       gltfAsset        `json:"asset"`
	Scene       int              `json:"scene"`
	Scenes      []gltfScene      `json:"scenes"`
	Nodes       []gltfNode       `json:"nodes"`
	Meshes      []gltfMesh       `json:"meshes"`
	Materials   []gltfMaterial   `json:"materials"`
	Accessors   []gltfAccessor   `json:"accessors"`
	BufferViews []gltfBufferView `json:"bufferViews"`
	Buffers     []gltfBuffer     `json:"buffers"`
}

type gltfAsset struct {
	Version   string     `json:"version"`
	Generator string     `json:"generator,omitempty"`
	Extras    gltfExtras `json:"extras"`
}

type gltfExtras struct {
	Units   string `json:"units"`
	Comment string `json:"comment,omitempty"`
}

type gltfScene struct {
	Nodes []int `json:"nodes"`
}

type gltfNode struct {
	Name     string    `json:"name,omitempty"`
	Mesh     *int      `json:"mesh,omitempty"`
	Children []int     `json:"children,omitempty"`
	Rotation []float64 `json:"rotation,omitempty"`
	Scale    []float64 `json:"scale,omitempty"`
}

type gltfMesh struct {
	Name       string          `json:"name,omitempty"`
	Primitives []gltfPrimitive `json:"primitives"`
}

type gltfPrimitive struct {
	Attributes map[string]int `json:"attributes"`
	Indices    int            `json:"indices"`
	Material   int            `json:"material"`
	Mode       int            `json:"mode"`
}

type gltfMaterial struct {
	Name                 string                `json:"name,omitempty"`
	PBRMetallicRoughness gltfMetallicRoughness `json:"pbrMetallicRoughness"`
}

type gltfMetallicRoughness struct {
	BaseColorFactor [4]float64 `json:"baseColorFactor"`
	MetallicFactor  float64    `json:"metallicFactor"`
	RoughnessFactor float64    `json:"roughnessFactor"`
}

type gltfAccessor struct {
	BufferView    int               `json:"bufferView"`
	ComponentType int               `json:"componentType"`
	Count         int               `json:"count"`
	Type          string            `json:"type"`
	Min           []float64         `json:"min,omitempty"`
	Max           []float64         `json:"max,omitempty"`
	Extras        *gltfScalarExtras `json:"extras,omitempty"`
}

type gltfScalarExtras struct {
	Units string `json:"units"`
}

type gltfBufferView struct {
	Buffer     int   `json:"buffer"`
	ByteOffset int64 `json:"byteOffset"`
	ByteLength int64 `json:"byteLength"`
	Target     int   `json:"target"`
}

type gltfBuffer struct {
	ByteLength int64 `json:"byteLength"`
}

// glbPart is a part's validated data and its place in the binary chunk.
type glbPart struct {
	mesh    *Mesh
	normals [][3]float32 // unit normals, nil when the part has none usable
	short   bool         // UNSIGNED_SHORT indices
	scalars []VertexScalarAttribute
}

// WriteGLB writes parts as one binary glTF 2.0 (.glb) asset: per part a
// node, a mesh with one indexed triangle primitive (POSITION, and NORMAL
// when every vertex normal is usable) and an opaque material in the part's
// colour. Every part must be a valid mesh with at least one triangle. The
// output is deterministic, and the binary chunk is streamed after the JSON.
func WriteGLB(w io.Writer, parts []Part, opts GLBOptions) error {
	if len(parts) == 0 {
		return fmt.Errorf("%w: glTF export without parts", ErrInvalidField)
	}
	if opts.VertexScalars != nil && len(opts.VertexScalars) != len(parts) {
		return fmt.Errorf("%w: %d scalar attribute lists for %d glTF parts", ErrInvalidField, len(opts.VertexScalars), len(parts))
	}
	totalScalars := 0
	for _, attributes := range opts.VertexScalars {
		if len(attributes) > MaxVertexScalarAttributes || totalScalars > MaxGLBScalarAttributes-len(attributes) {
			return fmt.Errorf("%w: glTF scalar attribute count", ErrInvalidField)
		}
		totalScalars += len(attributes)
	}
	for i, attributes := range opts.VertexScalars {
		if parts[i].Mesh == nil {
			return fmt.Errorf("%w: nil glTF part %d", ErrInvalidField, i)
		}
		if err := validateVertexScalars(attributes, len(parts[i].Mesh.Vertices), true); err != nil {
			return fmt.Errorf("glTF part %d: %w", i, err)
		}
	}
	units := "Vertex positions are DICOM patient LPS millimetres; the root node scales them to metres and turns superior to +Y."
	if opts.PatientAxes {
		units = "Vertex positions are DICOM patient LPS millimetres; the root node scales them to metres."
	}
	doc := gltfDocument{
		Asset: gltfAsset{Version: "2.0", Generator: opts.Generator, Extras: gltfExtras{
			Units: units, Comment: sanitizeHeaderLine(opts.Comment),
		}},
		Scenes:  []gltfScene{{Nodes: []int{0}}},
		Buffers: []gltfBuffer{{}},
	}
	if doc.Asset.Generator == "" {
		doc.Asset.Generator = "dicom-go mesh"
	}
	root := gltfNode{Name: "patient", Scale: []float64{0.001, 0.001, 0.001}}
	if !opts.PatientAxes {
		// -90° about +X (L): +Y (P) goes to -Z, +Z (S) to +Y.
		root.Rotation = []float64{-math.Sqrt2 / 2, 0, 0, math.Sqrt2 / 2}
	}
	doc.Nodes = append(doc.Nodes, root)

	var offset int64
	view := func(length int64, target int) int {
		doc.BufferViews = append(doc.BufferViews, gltfBufferView{ByteOffset: offset, ByteLength: length, Target: target})
		offset += (length + 3) &^ 3
		return len(doc.BufferViews) - 1
	}
	prepared := make([]glbPart, len(parts))
	for i, part := range parts {
		m := part.Mesh
		if err := m.Validate(); err != nil {
			return fmt.Errorf("glTF part %d: %w", i, err)
		}
		if len(m.Triangles) == 0 {
			return fmt.Errorf("%w: glTF part %d has no triangles", ErrInvalidField, i)
		}
		p := glbPart{mesh: m, normals: unitNormals(m.Normals), short: len(m.Vertices) <= gltfShortIndexLimit}
		if opts.VertexScalars != nil {
			p.scalars = opts.VertexScalars[i]
		}
		prepared[i] = p

		lower, upper := vertexBounds(m.Vertices)
		attributes := map[string]int{"POSITION": len(doc.Accessors)}
		doc.Accessors = append(doc.Accessors, gltfAccessor{
			BufferView: view(12*int64(len(m.Vertices)), gltfArrayBuffer), ComponentType: gltfFloat,
			Count: len(m.Vertices), Type: "VEC3", Min: lower, Max: upper,
		})
		if p.normals != nil {
			attributes["NORMAL"] = len(doc.Accessors)
			doc.Accessors = append(doc.Accessors, gltfAccessor{
				BufferView: view(12*int64(len(m.Vertices)), gltfArrayBuffer), ComponentType: gltfFloat,
				Count: len(m.Vertices), Type: "VEC3",
			})
		}
		for _, scalar := range p.scalars {
			attributes["_"+scalar.Name] = len(doc.Accessors)
			accessor := gltfAccessor{
				BufferView: view(4*int64(len(m.Vertices)), gltfArrayBuffer), ComponentType: gltfFloat,
				Count: len(m.Vertices), Type: "SCALAR",
			}
			if scalar.Units != "" {
				accessor.Extras = &gltfScalarExtras{Units: scalar.Units}
			}
			doc.Accessors = append(doc.Accessors, accessor)
		}
		indexSize, componentType := int64(4), gltfUnsignedInt
		if p.short {
			indexSize, componentType = 2, gltfUnsignedShort
		}
		indices := len(doc.Accessors)
		doc.Accessors = append(doc.Accessors, gltfAccessor{
			BufferView: view(3*indexSize*int64(len(m.Triangles)), gltfElementBuffer), ComponentType: componentType,
			Count: 3 * len(m.Triangles), Type: "SCALAR",
		})
		name := part.Name
		doc.Materials = append(doc.Materials, gltfMaterial{Name: name, PBRMetallicRoughness: gltfMetallicRoughness{
			BaseColorFactor: [4]float64{srgbToLinear(part.Color[0]), srgbToLinear(part.Color[1]), srgbToLinear(part.Color[2]), 1},
			MetallicFactor:  0, RoughnessFactor: 0.6,
		}})
		doc.Meshes = append(doc.Meshes, gltfMesh{Name: name, Primitives: []gltfPrimitive{{
			Attributes: attributes, Indices: indices, Material: i, Mode: gltfTriangles,
		}}})
		meshIndex := i
		doc.Nodes = append(doc.Nodes, gltfNode{Name: name, Mesh: &meshIndex})
		doc.Nodes[0].Children = append(doc.Nodes[0].Children, len(doc.Nodes)-1)
	}
	doc.Buffers[0].ByteLength = offset

	text, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	for len(text)%4 != 0 {
		text = append(text, ' ')
	}
	total := int64(12+8+len(text)+8) + offset
	if total > math.MaxUint32 {
		return fmt.Errorf("%w: glTF binary of %d bytes exceeds 4 GiB", ErrInvalidField, total)
	}
	out := bufio.NewWriterSize(w, 1<<16)
	var word [4]byte
	put := func(v uint32) {
		binary.LittleEndian.PutUint32(word[:], v)
		out.Write(word[:])
	}
	put(glbMagic)
	put(glbVersion)
	put(uint32(total))
	put(uint32(len(text)))
	put(glbChunkJSON)
	out.Write(text)
	put(uint32(offset))
	put(glbChunkBIN)
	for _, p := range prepared {
		for _, v := range p.mesh.Vertices {
			put(math.Float32bits(v[0]))
			put(math.Float32bits(v[1]))
			put(math.Float32bits(v[2]))
		}
		for _, n := range p.normals {
			put(math.Float32bits(n[0]))
			put(math.Float32bits(n[1]))
			put(math.Float32bits(n[2]))
		}
		for _, scalar := range p.scalars {
			for _, value := range scalar.Values {
				put(math.Float32bits(float32(value)))
			}
		}
		if p.short {
			var half [2]byte
			for _, t := range p.mesh.Triangles {
				for _, index := range t {
					binary.LittleEndian.PutUint16(half[:], uint16(index))
					out.Write(half[:])
				}
			}
			if len(p.mesh.Triangles)%2 != 0 {
				// Pad the 6-byte triangles to the 4-byte view alignment.
				out.Write([]byte{0, 0})
			}
		} else {
			for _, t := range p.mesh.Triangles {
				put(t[0])
				put(t[1])
				put(t[2])
			}
		}
	}
	return out.Flush()
}

// vertexBounds are the per-axis minimum and maximum of the float32
// coordinates, as float64 values equal to them (glTF requires the POSITION
// bounds and validators compare them exactly).
func vertexBounds(vertices [][3]float32) (lower, upper []float64) {
	lo := vertices[0]
	hi := vertices[0]
	for _, v := range vertices[1:] {
		for k := 0; k < 3; k++ {
			lo[k] = min(lo[k], v[k])
			hi[k] = max(hi[k], v[k])
		}
	}
	return []float64{float64(lo[0]), float64(lo[1]), float64(lo[2])}, []float64{float64(hi[0]), float64(hi[1]), float64(hi[2])}
}

// unitNormals renormalises the normals for glTF, which requires unit
// NORMAL vectors; nil when there are none or one is degenerate.
func unitNormals(normals [][3]float32) [][3]float32 {
	if normals == nil {
		return nil
	}
	out := make([][3]float32, len(normals))
	for i, n := range normals {
		u := unit32([3]float64{float64(n[0]), float64(n[1]), float64(n[2])})
		if u == ([3]float32{}) {
			return nil
		}
		out[i] = u
	}
	return out
}

// srgbToLinear converts an sRGB byte to the linear-light factor glTF's
// baseColorFactor holds.
func srgbToLinear(c uint8) float64 {
	v := float64(c) / 255
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}
