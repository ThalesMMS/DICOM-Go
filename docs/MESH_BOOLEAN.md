# Solid operations on triangle meshes

`mesh.Boolean` computes union, intersection or A minus B of two closed,
oriented boundaries in patient LPS millimetres. It returns owned indexed
geometry and face/corner provenance. `mesh.Merge` continues to concatenate
vertices and triangles; it does not remove interior boundaries.

```go
result, err := mesh.Boolean(ctx,
    mesh.BooleanInput{Mesh: a, Reference: "source-A"},
    mesh.BooleanInput{Mesh: b, Reference: "source-B"},
    mesh.BooleanDifference,
    mesh.BooleanOptions{ToleranceMM: 1e-6},
)
if err != nil {
    return err // result is nil, including quota and cancellation failures
}
derived := result.Mesh
_ = derived
```

Inputs and their attribute arrays are borrowed until return and must remain
stable. Calls may share immutable inputs concurrently. No source is cleaned,
repaired, reoriented or otherwise mutated. An empty `&mesh.Mesh{}` represents
the empty set; a nil mesh or vertices without any faces are invalid.

## Supported boundary profile

Each input must have finite coordinates, valid indices, nondegenerate faces,
closed consistently directed edges, a single circular face fan at each used
vertex, and no self-intersections or overlapping component boundaries.
Disconnected components, concave solids, and nested cavity/material shells
are supported. Outer/material shells must point outward; cavity shells must
point inward, alternating with nesting depth. Existing quality, closure and
component helpers are reused, with additional adjacent-face checks.

Triangle intersection segments partition source faces in float64. Oriented
solid angles classify retained pieces. Internal partition edges belonging
to the same original face are canceled before triangulation; holes in those
faces remain holes. Surviving boundaries are conformed across faces and
triangulated with the shared plane contour helpers. Final float32 positions,
triangle orientation, closure, vertex links, intersection quality and shell
nesting are validated again. This is continuous surface geometry, without
voxelization or a rendering backend.

Coplanar overlapping boundaries (including identical solids), purely tangent
component pairs, inconclusive predicates, unresolved intersection features,
and invalid input are explicitly refused. Coplanar triangles within a valid
individual shell remain permitted; disjoint solids and strict containment
need no intersections. A crossing pair can contain normal shared seam
endpoints. This profile is not a general CAD kernel or an automatic mesh
repair facility.

`ToleranceMM` defaults to 1e-6 mm. It is a physical ambiguity and output
representation bound, not a welding distance. Source face altitudes within
that bound are refused. A separate scale-dependent double-precision roundoff
band joins duplicate construction positions and removes collinear temporary
nodes. Small real cavities are retained; a coarser tolerance can reject them
but never authorizes filling them. Float32 conversion that merges distinct
positions, flips/collapses a triangle, or exceeds tolerance is refused.
Large absolute coordinates may therefore require a different caller-selected
tolerance or a more suitable coordinate representation. No tolerance is
increased internally to force success.

## Attributes and provenance

`Operation`, `References` (A, B), and `ToleranceMM` identify the derivation.
Every output triangle has a `Faces` entry with source input (0 or 1), original
triangle index, reversal flag and three barycentric coordinate triples.
`MaxProjectionErrorMM` bounds the observed distance from rounded output
corners to their attributed source triangles, not a Hausdorff certificate.

Supported attributes are source vertex normals, face RGB colors and named
finite `VertexScalarAttribute` fields. Scalars are interpolated with source
barycentric weights. Both nonempty meshes must supply the same ordered
scalar names and units; mismatches are refused. Missing normals use geometric
face normals and `SourceNormals=false`. Supplied normals are interpolated and
normalized; cancelling normals are refused. B's retained cavity faces in
A minus B reverse winding and normals. `ColorsPresent` distinguishes an
absent color from supplied black.

Normals and scalar values are stored per face corner (`CornerNormals`,
`CornerScalars`), preserving discontinuities between the two sources.
`Mesh.Normals` is nil because one shared geometric vertex may have different
shading values on each side. Existing mesh writers can export the geometry;
passing only `result.Mesh` does **not** export these corner attributes or
provenance. A consumer must explicitly retain them or adapt its format,
without averaging seams silently. Materials, textures, arbitrary attributes
and vertex identity are not part of `BooleanInput`'s contract.


## Finite work and storage admission

Zero quotas select finite defaults; negative quotas are invalid. Index and
native addressability capacities are also checked.

| Option | Default | Scope |
| --- | ---: | --- |
| `MaxVertices`, `MaxTriangles` | 200000, 400000 | Per input |
| `MaxPolygons` | 200000 | Cumulative constructed polygons, including discarded pieces |
| `MaxPolygonVertices` | 128 | Each partition/conformed boundary |
| `MaxPlanesPerFace` | 2048 | Per-source-face intersection cuts; hard maximum 4096 |
| `MaxWorkingVertices` | 400000 | Double-precision construction arena |
| `MaxResultVertices`, `MaxResultTriangles` | 200000, 400000 | Output |
| `MaxScratchBytes`, `MaxOutputBytes` | 256 MiB each | Logical reservations described below |
| `MaxWork`, `MaxCandidates` | 20000000, 1000000 | Whole request, including validation |

Slice growth is admitted before allocation with explicit capacity growth and
old/new overlap charges. Shared topology/triangulation helpers receive
conservative linear live-scratch envelopes. `ScratchBytes` sums these
reservations across phases and tracked growth, including discarded work;
it is not an exact cumulative allocation count or process RSS. Output growth
is charged cumulatively too. Caller-owned inputs, runtime/allocator overhead
and process-wide memory are outside these logical byte quotas.

`Work` counts logical inspections in intersection, winding, construction and
validation; it is not an instruction count or time measurement. Candidate
accounting includes quality/adjacency validation as well as cross-input
triangle pairs. Cross-input broad-phase scanning and several conformity
checks are quadratic and consume the finite work quota even for distant
meshes. Large valid meshes can exhaust defaults without producing a result.
The API does not promise large-mesh performance.

Context checks occur through charged work and at phase/publication boundaries.
Some shared linear helpers are admitted before execution and checked after
return; there is no hard cancellation-latency deadline. A terminal
cancellation check also covers an empty result. Admission failure and
cancellation always return nil.

## Local numerical qualification

The fixtures and reference equations were authored independently. No external
algorithm implementation, VTK runtime output or mesh repair is used as an
oracle. Windows amd64, Go 1.26.4, CPU geometry tests cover:

| Fixture | Independent expected volume (union / intersection / A minus B) | Expected area |
| --- | --- | --- |
| A=[0,2]^3; B=[1,3] x [0.5,2.5] x [0.25,2.25] mm | 13.375 / 2.625 / 5.375 mm³ | 36.25 / 11.75 / 24 mm² |
| Concave L prism and crossing box | 11 / 1.25 / 8.75 mm³ | 36 / 8.5 / 38.5 mm² |
| Box with a pocket inside one original triangular face | 1001 / 0.5 / 999.5 mm³ | 604 / 4 / 602 mm² |

The box oracle uses independent interval lengths and exposed rectangles;
absolute volume/area bounds are 2e-5 or tighter. Rigidly rotated/translated
boxes retain the analytic intersection with 4e-5 absolute bounds.

Two unit spheres use the existing 12-polar-band/24-azimuth mesh fixture, with
B translated by float32 (0.631, 0.217, 0.119) mm. For center distance d, the
analytic intersection volume is pi*(4+d)*(2-d)^2/12, and exposed sphere area
gives union/intersection areas 4*pi +/- 2*pi*d and difference area 4*pi.
Fixed bounds are 5% volume and 3% area because the inputs themselves are
polygonal approximations of spheres. All three outputs are closed:

| Operation | Faces | Volume / analytic mm³ | Area / analytic mm² |
| --- | ---: | --- | --- |
| Union | 1004 | 6.0813726924 / 6.2366361628 | 16.6279563526 / 16.8251049724 |
| Intersection | 540 | 2.0597320002 / 2.1409442468 | 8.1475260330 / 8.3076362563 |
| A minus B | 772 | 2.0108203584 / 2.0478459580 | 12.3877412649 / 12.5663706144 |

Other cases verify disjoint sets, containment, empty results, multiple shells,
an inward small cavity (side about 0.0001 mm), circular vertex links,
self-intersection/invalid/nonfinite/coplanar/tangent refusal, float32 precision
refusal, attributes and barycentric reconstruction, immutable inputs and
detached output, all quota categories, mid-phase/terminal cancellation and
deterministic concurrent requests. Shared plane tests exercise the original
nil representation-predicate behavior. Reproduce the focused race checks and
the separate library baseline from the module directory:

```sh
go test -race ./mesh -run '^Test(Boolean|Plane)' -count=1 -v
make check
```

