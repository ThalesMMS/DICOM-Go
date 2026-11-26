# Stored tract geometry

`tractographygeom` consumes the independently owned results produced by
[`tractography.Read`](../tractography). It selects stored tracks, computes
lengths and builds patient-LPS geometry. It never estimates diffusion tensors,
generates tracks, samples a volume, rasterizes or composites an image.

Every operation first applies the existing finite default IOD resource limits
and validates the complete stored model. Geometric operations additionally
reject nonfinite coordinates anywhere in that model. A nil context means
Background; cancellation and errors return no partial result. Inputs must stay
stable during a call. Output buffers, coded entries and source-reference lists
are independent of source buffers.

`TrackID` consists of the original set number and zero-based track index.
An empty ID list means no tracks. `Select` with empty include/exclude regions
returns all IDs, in stored order. Spheres and axis-aligned boxes use physical
patient-LPS millimetres and include their boundary. Intersection uses complete
original segments; a segment crossing a region is selected even when both
endpoints are outside. Include regions are ORed, or ANDed with
`RequireAllIncludes`; any exclude removes a track afterward. There are at most
1024 regions per operation.

`Analyze` sums original segment lengths in float64 millimetres. Repeated points
contribute zero; a closed path includes its stored closing segment. It retains
each selected track's measurement values and their original explicit point index
order, track statistics, set statistics and source references. It does not
recompute supplied measurements/statistics or invent area or volume. Stored
nonfinite measurement values remain available for inspection; they are never
replaced with zero. IDs and measurements do not depend on display LOD.

`BuildDisplay` provides polylines, open-ended tubes or two-sided-width ribbons
for a GPU scene consumer. Radius is physical and defaults to 0.5 mm; ribbon
width is twice the radius. Tube sides default to six and must be 3 through 32.
A projected transported frame keeps curved and oblique paths finite; closed
paths reuse the initial frame at the final original point. Surfaces may
self-intersect on sharp turns or self-crossing paths. This tessellation is a
display representation, not a watertight segmentation or a clinical surface
measurement. Direction color is linear RGB `abs(normalized(L,P,S tangent))`,
computed on original distinct points and invariant under reversing the track.

Scalar color requires an explicit zero-based measurement index in every
selected set, exact coded quantity/unit identity (`codes.Code`, including
meaning), UCUM units and a finite increasing domain. Every selected track must
have a finite value at every original point. Explicit point indices may be in
arbitrary order, but must cover all points exactly once. Sparse values, NaN,
infinity, missing measurements or mismatched codes return an error even when
display LOD would omit that track or point. The linear blue-to-red ramp
saturates outside the supplied domain. It does not infer units or a domain.

Zero display limits use these finite ceilings; positive overrides only lower
them: 100,000 tracks, 1,000,000 centerline points, 2,000,000 triangles and
256 MiB of returned array payload. Bytes count vertices, normals, colors,
original-point indices and triangle indices; they do not include Go headers,
temporary validation/sampling buffers, original IOD data or a downstream GPU
scene copy. Callers must separately budget those retained allocations.

LOD first samples tracks evenly in requested ID order if the track ceiling
requires it. It then samples original point indices using the requested stride,
retains endpoints and lowers point counts until every output ceiling fits.
Closed or folded tracks retain an original excursion and closing endpoint.
Sampling removes consecutive repeated positions; entirely repeated tracks have
no drawable primitive and are explicitly listed in `DegenerateTracks`, while
remaining selectable and measurable at zero length. A minimum display that
cannot fit the budgets returns `ErrLimitExceeded`, not an incomplete display.
LOD is deterministic and has no geometric approximation-error guarantee.
Each primitive retains its original owner and each vertex/point its original
point index. `DisplayStats` reports input/display counts, payload bytes,
degenerate IDs and whether sampling occurred.

The authored analytic tests cover straight/curved/oblique/reversed tracks,
closed/folded/repeated/degenerate paths, crossing/touching regions, original
measurements and ownership, scalar alignment/refusal, deterministic bounded LOD
and cancellation. They do not claim external tractography interoperability,
GPU frame correctness, native UI behavior or performance qualification.

```sh
go test ./tractographygeom -count=1
```
