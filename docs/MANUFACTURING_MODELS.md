# Reading manufacturing models

`mesh.ReadSTL`, `mesh.ReadOBJ` and `mesh.ReadMTL` accept a context, reader
and `mesh.ReadOptions`. Geometry requires a positive explicit
`MillimetresPerUnit`; returned vertices are physical millimetres. No STL or
OBJ filename, header or missing DICOM unit implies a scale.

Defaults admit 64 MiB input, 128 MiB logical output, one million vertices,
two million triangles, 1024 materials, 64 KiB lines, 256-byte names and
256 corners per polygon. Zero limits select these defaults; negative or
excessive options fail. Logical output excludes temporary input, parser
scratch, Go slice capacity and map overhead, which have independent count
and input limits. Reads and parse loops check cancellation.

STL accepts strict ASCII facets and little-endian binary facets. Binary is
recognised by size: 84 header bytes plus 50 bytes per facet. A size that
matches the stored triangle count takes precedence over a misleading solid
header; when the count disagrees, a non-text file of that size is read with
the count its size gives (the read limits apply to it), while text (valid
UTF-8 without NUL bytes) is parsed as ASCII. Stored facet normals are not
used and may be non-finite; the vertex winding defines each face and no
normals are returned. Finite positions, complete facet records, valid
indices and nonzero triangle areas are mandatory. Each facet keeps its own
three vertices. Binary attribute words are opaque; color extensions are not
inferred.

OBJ supports vertices with optional homogeneous weight, normal and texture
coordinates, independently indexed face corners, preceding positive or
negative indices, objects/groups, smoothing declarations, material names
and bounded continuations. Simple planar concave polygons are triangulated;
nonplanar, intersecting, self-touching and degenerate faces are refused.
Normals and texture bindings remain separate from the position mesh. Points,
lines, freeform surfaces, vertex colors and unknown directives are explicit
unsupported features.

MTL retains classic RGB properties, optical scalar declarations and opaque
texture declarations. Duplicate names/properties and simultaneous d/Tr opacity
declarations are refused. Texture arguments and mtllib names never cause file
or network access. Rendering applications must state which material properties
they support.

`encapdoc.ReadModelMetadata` validates STL/OBJ/MTL envelopes. Geometry needs
a valid explicit Frame of Reference UID and one UCUM CID 7063 unit (m, cm,
mm or um). MTL has no geometry scale. ICC bytes and color-space declarations
are preserved, with a 2 MiB ICC limit and at most 1024 explicit instance
references. Deferred ICC bytes must be materialized by the caller; they are
not silently interpreted as sRGB. Relative URIs are bounded comparison keys
validated by `ModelRelativeURI`, not resources to open. Traversal, absolute
URLs/paths, whitespace, backslashes and executable names are refused.

The envelope contract follows [PS3.3 Manufacturing 3D Model](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.35.html),
[explicit relative references](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.24.2.4.html),
and [CID 7063](https://dicom.nema.org/medical/dicom/current/output/chtml/part16/sect_CID_7063.html).

Focused checks: `go test ./mesh -run '^Test(Read(STL|OBJ|MTL)|ModelReaders).*'`
and `go test ./encapdoc -run '^TestModel(Metadata|RelativeURI).*'`.
