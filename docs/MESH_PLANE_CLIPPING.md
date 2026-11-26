# Plane sections and closed mesh clipping

`mesh.SectionByPlane` and `mesh.ClipClosedByPlane` operate on indexed triangular
solids in patient LPS millimetres. The initial profile supports **one plane per
call**, including concave surfaces and nested shells/cavities. They do not change
volume-rendering cut planes or add a desktop workflow. Any future 3D presentation
in Twin must remain GPU-only in both frontends and operating systems.

## Geometry and diagnostics

A plane consists of an origin in millimetres and a finite nonzero normal. The
normal is normalized; `KeepPositive` retains its nonnegative signed-distance
half-space, otherwise the nonpositive side is retained. The finite default
`ToleranceMM=1e-6` is a physical classification band: vertices inside it are
projected onto the plane. It is not an epsilon silently used to weld arbitrary
mesh points. Excessively large tolerance can collapse faces; this fails instead
of claiming a closed result. Output coordinates follow Mesh's float32 contract,
while intersections and planar predicates use float64.

Input faces must form closed consistently oriented shells with shared indices.
The existing `AnalyzeQuality` detects indexed duplicates, degeneracies and
nonadjacent intersections/contact/uncertainty. Such inputs are rejected. Indexed
adjacent faces are excluded by that existing classifier; this is not a complete
CAD certification of all possible self-intersections or of arbitrary pathological
solid topology. Outward material boundaries and inward cavity boundaries are
required. At a cut, nesting and boundary orientation are checked; incompatible
nested shell winding is an error, not repaired automatically.

Sections report points, ordered polylines, loop identity, immediate parent and
nesting depth. Odd-depth closed loops are holes. Closed loops do not repeat their
first vertex. Winding of the ordered section itself is traversal-dependent;
`Depth` and `Hole` identify material/cavity semantics. A tangential face yields
its perimeter, a tangent edge may yield an open polyline, and an isolated tangent
vertex is counted. Branching, intersecting or tolerance-touching closed loops
are inconclusive and produce `ErrPlaneGeometry`.

Clipping intersects each original edge once and preserves shared endpoint IDs.
Original side triangles are clipped and triangulated. Remaining directed boundary
edges are reversed for the cap. Loops are projected onto a basis whose normal is
the outward cap normal. A visible bridge connects each immediate hole to its
outer ring; ears triangulate the resulting weakly simple ring, retaining the
original boundary IDs and collinear boundary vertices. Triangle centroids must
belong to the material domain, cap area must equal outer area minus holes, and
final edge closure/winding and geometric quality are verified again. Nested
material islands are capped separately. An unavailable bridge, degenerate ear,
ambiguous triangulation or precision collapse returns an error, with no partial
result. Cavities are not filled independently as disks.

Each indexed connected shell is classified independently: a tangential shell
with no strictly retained vertex is removed, even when a different disconnected
shell is retained. The section still reports its tangent contour.

When the plane retains no strictly interior vertex in any component, the clipped solid is empty,
including an outward tangential face/edge/vertex. Conversely, retaining the solid
side of a tangent plane preserves the solid (subject to the stated projection
band). The empty result has no faces and is distinguished from an error.

## Attributes, ownership and provenance

Inputs and option slices are borrowed only during the call and must remain
stable. Successful results own their geometry, loops, scalar arrays and colors;
concurrent independent calls share no mutable cache. Source-edge interpolation
uses the same parameter for geometry, normals and each `VertexScalarAttribute`.
Normals are normalized interpolated source shading normals, not sharp cap vertex
normals; cap face normals come from geometry. Compatible scalar names/units use
the existing PLY/GLB attribute contract. Original face colors are copied and cap
faces use `CapColor` when a source face-color array is supplied.

`SourceTriangles[i] >= 0` names the original face; `-1` identifies a generated
cap. `Plane`, `KeepPositive` and `SourceReference` record the derivation. A bare
Mesh has no DICOM source identity: anonymous mathematical use may leave the
reference empty, while an application exporting a clinical-derived asset must
supply/preserve its source identity outside this geometry API. Neither these
operations nor a successful export establishes clinical validity.

`PlaneClipResult.WritePLYFile(ctx, path, maxBytes)` writes existing PLY geometry,
scalar properties, optional colors and derivation comments to a private temporary
in the destination directory. It checks cancellation and the output-byte quota,
flushes/syncs/closes and checks cancellation before rename. Failure removes the
temporary and leaves the old destination intact. Rename is the commit point;
a cancellation after successful rename does not yield a false failed-publication
return. Zero export limit selects 512 MiB. Destination replacement follows the
platform's `os.Rename` contract and may fail explicitly. Parent-directory crash
durability is not promised. Full per-face source indices remain in the result;
ordinary PLY/OBJ/STL do not encode the entire application provenance graph.

## Bounds and cancellation

PlaneOptions has finite zero-value defaults: one million input vertices, two
million input triangles, two million result vertices, four million result
triangles, 512 MiB logical working bytes and twenty million work units. Negative
limits are invalid. Checked admission reserves

`32768 + (inputVertices + 4*inputTriangles) * (1536 + 64*scalarAttributeCount)`

bytes before topology/intersection/output allocation. This conservative logical
reservation covers simultaneous input accounting, topology and quality arrays,
clipping, loops, bridges, ear scratch and owned results; normal slots are always
included. It excludes Go allocator/runtime overhead and is **not measured peak
RSS**. Attribute validation additionally bounds the existing scalar count/name/
units contract. Input and result quality checks use their respective independent vertex/face
limits, so an admitted input may produce an empty result under tiny output quotas.
Every new vertex/face and predicate traversal is bounded; total
work includes existing quality work. All arithmetic is checked before admission.
The algorithm is deterministic and serial; there are no private workers or
persistent cache. Context errors return nil result and never imply a valid
partial solid. Export has a separate byte bound and temporary lifecycle.

Large or adversarial contours can exhaust the work quota; no near-linear cost,
robustness to all CAD meshes, universal speedup or memory/RSS gain is claimed.
The single-plane API does not authorize an unbounded implicit collection of
planes. Callers may request separately budgeted successive operations.

## Actual qualification and remaining integration checks

Root executed the following new tests successfully as part of a 15-test focal
selection (nine new top-level tests and six existing mesh regressions):

- `TestPlaneSectionClosedPhantoms`: independent shoelace section areas and signed
  half volumes for box, concave L prism and a sampled sphere; source immutability
  and outward cap normals.
- `TestPlaneClipCavityAndNestedIslands`: nested oriented boxes, cavity subtraction,
  parent/depth and material island; cap centroids cannot cover the void.
- `TestPlaneSectionTangentCoincidentAndSides`: face/edge/vertex coincidence, empty
  section, reversed retained sides and oblique symmetric half solids.
- `TestPlaneClipAttributesOwnershipAndDeterminism`: affine scalar interpolation,
  normal/color/source-face correspondence, independent output and concurrent calls.
- `TestPlaneClipAdmissionCancellationAndInvalidGeometry`: preallocation quotas,
  overflow, cancellation, open/inconsistent/intersecting solids and invalid planes.
- `TestPlaneClipSeparateInputAndOutputQuotas`,
  `TestPlaneClipTangentDisconnectedComponent` and
  `TestPlaneClipCanceledEmptyNeverPublishes`: causal regression cases for independent
  input/output admission, tangent shells with another retained component, and the
  empty-result publication cancellation fence.
- `TestPlaneClipExportRoundTripAndAtomicFailure`: independent PLY/OBJ readers,
  exact indexed geometry/scalar/color payload, EOF, area/volume/winding, disk and
  quota/cancel failure preserving the destination and removing temporaries.

The actual focal command was `go test -json -overlay=<sealed G5 overlay>
-count=1 -p=1 -timeout=60s ./mesh -run <exact 15-test selector>` on macOS
27.0.1 arm64 with Go 1.27.1, GOMAXPROCS=2 and GOMEMLIMIT=1GiB. All 15 top-level
tests and one package passed, with zero failures or skips, in 1.199269 seconds
of supervised command time. A separate `-race -timeout=90s` run selected
`TestPlaneClipAttributesOwnershipAndDeterminism` and
`TestPlaneClipExportRoundTripAndAtomicFailure`: both top-level tests and one
package passed with zero failures or skips in 4.251515 seconds. This is a focused
race check, not a full-module race result. Both commands exited normally, their
owned process groups were empty, children reaped and output drained; all seven
payload and 466 selected dependency guards matched before and after execution.

The earlier G3 focal remains recorded as FAILED: only the oblique cube cap
failed containment; the other 14 top-level tests passed. G4 remained a static
intermediate. G5 added conservative physical ear-area and boundary-point
admission plus positive/negative projection-ULP controls, while preserving
containment, cap-area, winding and manifold oracles. The corrected oblique
case now passes for both retained sides, with volume 4 and cap area 3*sqrt(3).
No failed receipt is relabelled as passing.

The six original acceptance criteria map to the actual cases as follows:

| Criterion | Evidence and limits |
| --- | --- |
| Patient-space loops, generated-face identity and attributes | Ordered loops with parent/depth/hole, original/cap face IDs, affine scalar interpolation, normals/colors/provenance and owned output checks. |
| Box, sphere, concave surface and cavity | Independent contour shoelace areas and signed volumes, oriented closure, cavity subtraction and nested material island; cap centroids cannot fill the void. |
| Tangency, coincidence, empty and retained-side inversion | Face/edge/vertex/empty cases, disconnected tangent shell removal, complementary oblique volume and cap-area checks; finite physical tolerance. |
| Invalid input and inconclusive triangulation | Quality/closure diagnostics, non-manifold vertex-link rejection, open/inconsistent/intersecting input, cap domain/area/winding and final manifold checks. General CAD robustness is not promised. |
| Context and finite input/output/scratch/export bounds | Checked admission/work/result quotas, independent input/output quotas, terminal empty cancellation returning nil, and bounded atomic file publication preserving an existing destination on failure. |
| Export round-trip and focused checks | Independent PLY/OBJ parsing of resulting indexed geometry and attributes, EOF and mass/closure checks, focal15 and focused race2. No UI was added; future Twin presentation must share its service and remain GPU-only. |


Admission also checks that every used vertex has one connected circular link.
Closed edge incidence alone does not admit shells welded only at a vertex.
This linear bounded check uses the same request reservation, work limit and
context; invalid input never produces a mesh eligible for export.

Oblique projection does not turn collinear boundary vertices into zero-width
cap ears: the oriented area must exceed the existing physical tolerance times
the proposed diagonal length. The vertex remains in the boundary; all cap area, containment,
winding and final manifold checks remain required. G3 focal failure is
preserved; the corrected G5 focal qualification passed.

The ear blocker includes boundary points within that same physical tolerance,
including negative projection roundoff on a diagonal, so clipping cannot skip
an existing contour vertex. This strengthens triangulation admission only;
cap-domain, area and final manifold validation are unchanged.
