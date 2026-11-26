# Discrete mesh curvature

`mesh.VertexCurvatures(ctx, m, mesh.CurvatureOptions{})` computes per-vertex
signed mean, Gaussian and principal curvature. It returns owned float64
arrays and flags with exactly one entry per input vertex. Positions are
promoted from `Mesh`'s float32 patient LPS millimetres before geometric
operations. The mesh is borrowed for the call, is never changed or retained,
and must remain stable while the call runs. Concurrent calls use separate
scratch and result arrays.

The estimator uses cotangent edge weights and mixed Voronoi areas, following
[Meyer et al., Discrete Differential-Geometry Operators for Triangulated
2-Manifolds](https://www.multires.caltech.edu/pubs/diffGeoOps.pdf), sections
3–5. Mean is half the projection of the discrete mean-curvature vector onto
the area-weighted oriented face normal. It does not use supplied vertex
normals. On an outward-wound convex sphere of radius R, mean and both
principal curvatures approach 1/R, and Gaussian approaches 1/R².

Mean and principal values are in **mm⁻¹**, Gaussian in **mm⁻²**, and
`MixedArea` in **mm²**. Reversing all triangle winding reverses mean and
principal signs, preserving Gaussian and area. `PrincipalMin` and
`PrincipalMax` are ordered numerically. Uniformly scaling coordinates by s
scales mean/principal by 1/s, Gaussian by 1/s² and area by s².

For non-obtuse triangles the mixed area is the corner's Voronoi area. An
obtuse triangle allocates half its area to the obtuse corner and a quarter
to each other corner. Gaussian curvature is the interior angle defect
divided by that mixed area. Principal values are H ± sqrt(max(0, H²−K)).
When H²−K is negative, `CurvaturePrincipalClamped` records the projection:
both principal values equal H, while the original H and K remain unchanged.
Their product consequently need not equal K at a flagged vertex.

## Accepted geometry and masked vertices

Each used vertex must have one connected manifold face fan: a cycle at an
interior vertex or a path at a boundary vertex. Disconnected surface
components are accepted. An edge must have one or two incident triangles;
two-face edges must be traversed in opposite directions. Duplicate faces
in either winding, three-face edges, inconsistent winding and bow-tie
vertices are errors. This checks vertex links as well as edge counts.

`CurvatureBoundary` masks all four curvature values to zero at boundary
vertices, while retaining their mixed area. A boundary angle defect also
contains boundary geodesic curvature and is not exposed as the requested
interior Gaussian estimate. `CurvatureUnused` identifies vertices not used
by any face; their numeric results, including area, are zero. The flags
distinguish these vertices from flat interior vertices. Empty meshes return
an empty result.

Missing/repeated indices, non-finite positions, mismatched normal-array
length, zero-length edges, degenerate triangles and singular interior
oriented normal sums are errors. A triangle is numerically singular when
its cross-product magnitude is at most `64 * epsilon_float64` times its
largest squared edge length. The normal-sum test uses the same relative
threshold against the sum of incident cross-product magnitudes. These
tests use no fixed millimetre cutoff. The function does not repair, weld,
orient components or detect global self-intersections. Coincident vertices
with separate indices are not automatically connected.

Invalid geometry/options wrap `mesh.ErrInvalidField`. Count/byte limits
wrap `mesh.ErrBudgetExceeded`, with `*mesh.CurvatureBudgetError` carrying
effective limits and the conservative array reservation where applicable.
Cancellation is checked during validation, topology sorting, geometry and
finalization; no partial result is returned. Nil context means Background.

## Resource and precision limits

Default limits are 2,000,000 vertices, 4,000,000 triangles and 1 GiB of
managed array reservation. Zero option values select those defaults;
negative limits are invalid and positive limits are explicit finite bounds.
Checked count arithmetic and uint32/host-int index limits always apply.
Oversized input fails before allocating result or topology arrays.

For nonempty input with N vertices and T triangles, the reservation is
`128*N + 128*T + 8192` bytes. Array capacities are at most `109*N + 111*T`:
five float64 result arrays and one byte of flags, two float64 vector arrays,
two float64 scalar arrays and a uint32 vertex bookkeeping array; two
at-most-16-byte edge-record buffers with three records per triangle, uint32 corner
parents and byte ranks. The extra reservation covers fixed bookkeeping.
Arrays are local to one call; sorting and vertex-link checks do not use
global caches or grow maps. This bound excludes input, allocator/runtime
overhead and process RSS; it is not a measured RSS ceiling.

Float64 computation cannot restore detail lost in float32 input positions.
Large offsets, fine spacing, poorly shaped triangles, geometric noise and
sharp creases can impair estimates. Refinement is not promised to improve
every arbitrary triangulation or every value monotonically. Principal
clamping is especially relevant at nearly umbilic vertices. No general
convergence theorem or exact correspondence with another library is claimed.

## Exporting scalar attributes

`PLYOptions.VertexScalars` and `GLBOptions.VertexScalars` accept
`VertexScalarAttribute{Name, Values, Units}`. The arrays are borrowed for
the writer call and must remain stable. Each must match the corresponding
vertex count. For GLB the outer slice must match the number of parts; nil
means no attributes, and a nil/empty inner slice adds none to that part.
`Mesh` and `Part` do not acquire extra owned arrays.

```go
c, err := mesh.VertexCurvatures(ctx, m, mesh.CurvatureOptions{})
if err != nil {
    return err
}
flags := make([]float64, len(c.Flags))
for i, f := range c.Flags {
    flags[i] = float64(f)
}
attributes := []mesh.VertexScalarAttribute{
    {Name: "CURVATURE_MEAN", Values: c.Mean, Units: "mm^-1"},
    {Name: "CURVATURE_GAUSSIAN", Values: c.Gaussian, Units: "mm^-2"},
    {Name: "CURVATURE_MIN", Values: c.PrincipalMin, Units: "mm^-1"},
    {Name: "CURVATURE_MAX", Values: c.PrincipalMax, Units: "mm^-1"},
    {Name: "CURVATURE_FLAGS", Values: flags},
}
// PLY:
err = mesh.WritePLY(w, m, mesh.PLYOptions{VertexScalars: attributes})
// Or GLB, for one part:
err = mesh.WriteGLB(w, []mesh.Part{{Name: "surface", Mesh: m}},
    mesh.GLBOptions{VertexScalars: [][]mesh.VertexScalarAttribute{attributes}})
```

PLY writes scalar `double` properties, preserving float64 scalar precision,
after existing position/normal properties, with optional units comments.
GLB writes custom `_Name` FLOAT/SCALAR accessors, rounds values to float32
and stores optional units in accessor extras. Those units describe the
stored values: the existing GLB root transform converts vertex positions
to metres but **does not rescale custom scalar values**. Generic viewers
are not promised to visualize custom attributes.

Names must be ASCII identifiers starting with a letter, at most 64 bytes;
x/y/z/nx/ny/nz are reserved regardless of case. Duplicate names are rejected.
Units may contain ASCII letters/digits and `^*/._-`, at most 64 bytes.
There are at most 32 scalar attributes per mesh/part and 4096 across a GLB
call. Names, units, counts and all finite values are checked before output;
GLB also rejects float32 overflow. PLY can represent a finite double larger
than float32. New views obey GLB's existing alignment and 4 GiB bound.
With no scalar attributes, existing PLY/GLB bytes remain unchanged.

[Khronos glTF 2.0](https://registry.khronos.org/glTF/specs/2.0/glTF-2.0.html)
sections 3.6–3.7 specify underscore-prefixed custom semantics, matching
accessor counts and finite IEEE754 float32 values. The PLY extension follows
the existing binary little-endian 1.0 element/property format, with scalar
double properties. Writers keep their existing io.Writer APIs and propagate
writer errors; the curvature context does not govern a later writer call.

## Analytic acceptance

Tests use independently constructed analytic fixtures. An outward R=20 mm
icosphere at levels 1–4 records every used vertex, including the original
twelve valence-five vertices. Maximum relative acceptance bounds at levels
3/4 are 5% for mean/Gaussian and 15% for principal values. Area-weighted RMS
errors decrease between asymmetric levels above the explicit relative
float32 noise floor `2^-23`; the symmetric first level's nearly exact mean
estimate is below that floor and is not used as a monotonicity baseline.

Observed relative RMS H at levels 1/2/3/4 is approximately
`3.17e-8 / 7.23e-5 / 1.58e-5 / 8.92e-6`, and RMS K is
`0.0773 / 0.0192 / 0.00480 / 0.00120`. At level 4, maximum relative errors
are `2.12e-5` for H/principal and `0.00142` for K. The prospective analytic
limits were retained. These figures qualify the synthetic fixture and
estimator; they are not guarantees on arbitrary surfaces.

Open R=10 mm cylinders use 32/64/128 angular segments. Interior expectations
are H=1/(2R), K=0, k_max=1/R, k_min=0; the two end rings are masked. Tests
retain 2% relative H/nonzero-principal limits, `1e-4/R²` absolute K and
`1e-4/R` near-zero principal limits. Observed maximum relative H errors are
`4.82e-7 / 6.65e-6 / 3.61e-5`; they increase as spacing shrinks with
float32 coordinates, so no strict cylinder error-decrease claim is made.

Additional checks cover a regular tetrahedron's closed-form area/angles,
Gauss–Bonnet on closed spheres, area tiling, an obtuse triangle, flat/saddle
patches, winding/scaling/rigid transforms, source/result ownership,
concurrent calls, cancellation and topology/budget errors. Independent
PLY/GLB readers verify every exported scalar, flag, unit, count and binary
range; twelve baseline hashes verify default bytes. These are local
synthetic and format checks, not clinical, live-system or generic-viewer
acceptance.
