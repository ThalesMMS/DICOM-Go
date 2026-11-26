# Physical and subpixel polyline simplification

`roi.SimplifyPhysicalPolyline` simplifies one independent piecewise-linear line
in a caller-supplied physical frame. `roi.Point3D` coordinates and the tolerance
are millimetres. `SimplifyPixelPolyline` first applies an explicitly supplied
pixel-to-physical affine plane; it returns the retained original subpixel
coordinates as well as their physical coordinates. Neither operation changes
`SimplifyClosedContour`, ROI measurements, CPR paths or Twin defaults.

```go
result, err := roi.SimplifyPhysicalPolyline(ctx, physicalPoints,
    roi.PolylineSimplifyOptions{
        ToleranceMM: 0.05,
        ProtectedIndices: []int{4, 12},
        Topology: roi.PolylineRequireSimple,
    })
```

The result owns `Points` and `OriginalIndices`, ordered by original index.
Coordinates are copied unchanged from the input. Open endpoints and all
protected indices survive. Duplicate protected indices are idempotent, and
separate protected samples at the same coordinate both survive. The caller
must not mutate input slices concurrently with a call.

## Physical error and closure

The iterative Ramer–Douglas–Peucker pass partitions at protected points. A
candidate chord is admitted only when **every** intervening original vertex is
within `ToleranceMM` of the finite segment, using exact rational squared
Euclidean distance on the input float64 values. An approximate distance chooses
a split; it cannot authorize an otherwise invalid chord. Distance to a segment
is convex, so this bounds the whole original piecewise-linear subpath to that
chord, including the points between samples. This is not a bound for a spline
fitted later, a raster mask, a resampled CPR path, an area or a measurement.
Zero tolerance removes exactly collinear intermediate points only.

`Closed` supplies a final edge to the first point. The first vertex and two
deterministically selected distinct original anchors survive, maintaining at
least three distinct physical vertices. A terminal point exactly equal to the
first is accepted and retained with its own original index; `ExplicitClosure`
reports this representation. An input with implicit closure stays implicit.
Closed inputs with fewer than three distinct physical vertices are refused.
The explicit unchecked profile can admit a degenerate collinear loop; it does
not claim positive area or winding preservation.

Empty and one-point open lines are valid owned results. Consecutive exact
duplicates can be removed unless protected or an endpoint. They are coalesced
only for intersection geometry; near duplicates are not merged by an epsilon.

## Intersection profiles

The zero/default `PolylineRequireSimple` profile checks both input and output.
Nonadjacent segment crossings, touches and collinear overlaps are refused.
Adjacent segments may meet at their common endpoint, but cannot overlap by
backtracking. Consecutive zero-length edges are ignored for these checks.
Axis-aligned bounding boxes cheaply reject separated segment pairs. Remaining
pairs use exact rational coplanarity and orientation predicates on the physical
float64 coordinates, including arbitrary 3D planes and subnormal separation.
An introduced intersection returns `ErrPolylineTopology` and a zero result;
there is no automatic fallback to an unverified line.

`PolylineIgnoreIntersections` is an explicit caller choice. It skips both
intersection checks and reports that profile in the result. Neither profile
promises preservation of knot type, ambient isotopy, minimum separation,
intersections with other lines, graph branches, shared vertices, enclosed area
or downstream clinical interpretation. Passing the simple-line check is a
narrow segment-intersection result, not a general topology certificate.

## Explicit subpixel mapping

`PolylinePixelPlane` maps `(x,y)` to
`OriginMM + x*ColumnStepMM + y*RowStepMM`. The two independent displacement
vectors already contain the physical millimetres per pixel; they need not be
orthogonal or unit length. No image spacing, direction or frame identity is
inferred. Anisotropic and oblique planes use the physical error before deciding
which original subpixel points to retain.

The affine is materialized in float64 and admitted after transformation. The
exact distance and intersection guarantees concern these materialized physical
coordinates, not an ideal arbitrary-precision affine. Extremely small steps
beside a large origin can round distinct pixel positions to coincident physical
positions; callers must choose representable geometry appropriate to their
frame and intended precision. No inverse transform or pixel rounding is used
when returning retained original subpixel points.

## Admission, cancellation and diagnostics

Coordinates, tolerance, pixel coordinates and affine fields must be finite,
with absolute magnitude at most `1e9` in their documented units. Tolerance is
nonnegative; affine displacement vectors must be independent. Transformed
coordinates outside the physical range are refused, never clamped.
Out-of-range protected indices, nil contexts, unknown profiles and invalid
limits return `ErrPolylineInvalid`.

| Limit | Zero/default | Hard maximum |
|---|---:|---:|
| Points / protected-index entries | 4,096 | 100,000 |
| Charged work | 32,000,000 | 1,000,000,000 |
| Estimated live bytes | 8 MiB | 256 MiB |

Negative limits or requests above a hard maximum are invalid. Admission checks
`256 * inputPoints + 256 KiB` before point-sized allocations. This conservative
structural estimate includes the affine adapter, fixed-capacity linear arrays,
owned output and bounded exact-predicate scratch. It is **not** an RSS,
allocator-rounding, garbage-collector, cumulative-allocation or process-memory
limit. Input buffers belong to the caller. Exact arithmetic has fixed-degree
expressions on the admitted finite float64 numbers; it never grows an
arbitrary symbolic expression.

`Work` charges point visits, distance evaluations, stack processing, segment-pair
checks (including bounding-box rejections), transformation and metrics loops.
The all-pairs intersection profile can exhaust the work limit before the point
limit, which is an explicit refusal rather than an incomplete check. There is
no quadratic stored matrix, recursion or background worker. Each charged step
checks cancellation; a single fixed-size exact predicate completes before the
next check. Context cancellation and deadline errors remain recognizable with
`errors.Is`; all errors return a zero result.

The result reports original/output float64 lengths, removed-point count,
reduction fraction, charged work and the structural byte estimate. Length and
reduction are observations, not guarantees of preserved length. Maximum
deviation is computed from exact rational squared distances, rounded after a
128-bit square root to a float64 diagnostic. The exact tolerance admission
continues to govern the accepted geometry independently of diagnostic rounding.

## Qualification and adoption limits

Local contract tests cover analytic curves, helixes, closed loops, original
index/anchor correspondence, explicit and implicit closure, protected duplicate
samples, anisotropic/oblique mappings, introduced-crossing refusal, endpoint
touches/backtracking, 3D skew versus coplanar crossings, finite range/subnormal
values, input/output ownership, work/byte limits and cancellation during work.
External-package controls independently compare exact rational squared
cross-product distance to the tolerance, express diagnostic distance by float64
cross products and sum original/output lengths. Boundary controls include
subnormal distances. They do not invoke private distance/predicate helpers. Native/race/minimum-toolchain command results and a separately sealed
synthetic measurement campaign are recorded in the delivery evidence.

Benchmarks expose explicit `Simple` and `Unchecked` cost profiles. Measurements
of time, allocations, length and reduction must be read with the recorded
synthetic inputs and commands; no existing 3D simplifier speedup is implied.
Consumer/UI/CPR impact is unqualified, and no consumer default is changed.
Integrating this primitive into Twin must be explicit and reversible, with
caller review of error and measurement/CPR impact. CPU geometric preprocessing
does not authorize CPU 3D rendering: Twin ray casting, volume sampling and image
composition remain GPU-only in both frontends on both operating systems.


A sealed public-API campaign on Apple M4 Pro, macOS 27.0.1 arm64,
Go 1.27.1 and `GOMAXPROCS=1` made four fresh process-group repeats. Each process
made one warmup and three measured calls for every case/profile: 64 warmups +
192 measurements = **256 actual API calls**, without calibration or hidden
benchmark calls. All 12 observations per cell, including outliers, were retained.
The four measurement processes and the prepare process were reaped with empty
private groups. Source, executable and seals matched before and after execution.
Other work could be active on the host; these are descriptive absolute costs,
without a quiet-host, significance or speedup claim.

`runtime.GC` and counter reads occurred outside the timed call. Allocation below
is the observed process `TotalAlloc` delta for one call, **cumulative allocated
bytes**, not live scratch, RSS, peak memory or the structural admission estimate.
No consumer, image/GUI frame or CPR spline was timed. Exact arithmetic has a
substantial time/allocation cost in these inputs; this API was not installed as
a Twin default.

| Case / profile | Outcome | Median time ms [min–max] | Median TotalAlloc bytes [min–max] | Median Mallocs [min–max] |
|---|---|---:|---:|---:|
| AnisotropicPixel / Simple | success | 21.375500 [20.880708–43.469625] | 16768424 [16768408–16768440] | 488167 [488167–488167] |
| AnisotropicPixel / Unchecked | success | 16.841563 [16.388166–17.290958] | 13442400 [13442384–13442416] | 381396 [381396–381396] |
| Circle / Simple | success | 30.614750 [30.132583–56.902666] | 18779016 [18779000–18779032] | 405641 [405641–405641] |
| Circle / Unchecked | success | 23.731521 [23.119625–35.795875] | 14096936 [14096936–14096952] | 292841 [292841–292841] |
| Helix / Simple | success | 40.184042 [38.901292–55.671834] | 28983568 [28983544–28983592] | 682047 [682047–682047] |
| Helix / Unchecked | success | 31.637355 [30.145792–42.536584] | 22754240 [22754208–22754256] | 530531 [530531–530531] |
| IntroducedCrossing / Simple | topology | 0.050458 [0.048250–0.055708] | 32784 [32784–32784] | 1515 [1515–1515] |
| IntroducedCrossing / Unchecked | success | 0.015730 [0.014917–0.019625] | 8384 [8384–8384] | 378 [378–378] |
| Line / Simple | success | 4.014542 [3.963334–4.089666] | 2357344 [2357344–2357344] | 108247 [108247–108247] |
| Line / Unchecked | success | 1.955062 [1.930792–2.050416] | 1238640 [1238640–1238640] | 55055 [55055–55055] |
| Protected / Simple | success | 10.799437 [10.528750–12.009500] | 7594176 [7594160–7594176] | 207974 [207974–207974] |
| Protected / Unchecked | success | 6.399250 [6.313916–7.337417] | 4567864 [4567864–4567880] | 117213 [117213–117213] |
| Sine / Simple | success | 24.336333 [23.854542–30.352125] | 15727504 [15727488–15727640] | 421203 [421203–421205] |
| Sine / Unchecked | success | 19.390500 [19.009250–20.376333] | 12708520 [12708504–12708520] | 330691 [330691–330691] |
| WorkCap / Simple | limit | 0.002313 [0.002125–0.003250] | 13904 [13904–13904] | 3 [3–3] |
| WorkCap / Unchecked | limit | 0.001875 [0.001666–0.002375] | 5200 [5200–5200] | 3 [3–3] |

For the six regular synthetic geometries, both profiles retained identical
samples. Tolerance was 0.05 mm; these observations characterize the resulting
piecewise-linear path, without asserting preserved length or downstream CPR
accuracy. The closed circle had 256 input vertices; other regular cases had 257.

| Case | Retained | Removed % | Input/output length mm | Length delta mm | Max deviation mm | Work Simple / Unchecked |
|---|---:|---:|---:|---:|---:|---:|
| Line | 2 | 99.221790 | 16.000000000 / 16.000000000 | 0.000000000 | 0.000000000 | 34435 / 1538 |
| Sine | 26 | 89.883268 | 19.510111919 / 19.455589526 | -0.054522393 | 0.045890074 | 36160 / 2939 |
| Helix | 33 | 87.159533 | 16.077209880 / 15.912084083 | -0.165125796 | 0.038802537 | 36469 / 3045 |
| Circle | 32 | 87.500000 | 62.830276023 / 62.730969811 | -0.099306212 | 0.048152733 | 36392 / 2970 |
| Protected | 27 | 89.494163 | 19.510111919 / 19.444008719 | -0.066103200 | 0.045890074 | 34972 / 1725 |
| AnisotropicPixel | 17 | 93.385214 | 45.768230294 / 45.744554287 | -0.023676007 | 0.046348290 | 36128 / 3096 |

The simple profile refused the introduced-crossing control; the explicitly
unchecked profile accepted four of its six samples, with 4 mm deviation at a
5 mm tolerance and lengths 19.605551275 → 11.605551275 mm. That unchecked result
is not intersection-qualified. Both profiles refused the 512-work-unit control.
Refusal returns zero results, so zero geometry/report fields in its raw record
are not measurements of accepted geometry.

The structured admission estimate was 327,936 bytes for 257 samples and 327,680
bytes for 256, distinct from the cumulative allocation counts above. For example,
Helix/Simple had 28,983,568 median cumulative allocated bytes and 682,047 Mallocs;
its 40.184 ms median spanned 38.901–55.672 ms. Circle/Simple ranged
30.133–56.903 ms, and AnisotropicPixel/Simple 20.881–43.470 ms. These outliers
remain in the table and evidence; no allocation, RSS or universal timing gain
is inferred.

Campaign seal SHA256:
`7df65cc2f1109992ad8ea993b941de0b8104eb126de7a60116ed44a85cb761ec`.
Executable SHA256:
`2f77c34e7e40c5ec3d0c92105e0c8f4afdd6bb10419575224d8f97167cc297fb`.
Complete observations SHA256:
`09730a9f912a3788abc0bce413b3c93a29642412f72f6af5fcf75fdd2c8a3f49`.
The successful G1 run qualifies its actual empty/reaped process groups; its
failure-cleanup branches were not exercised. A separate sealed G2 guard changed
only supervisor cleanup and passed two private timeout/child-survivor controls
with bounded kill/reap/drain, **zero product/benchmark calls**. Timing harness,
input corpus, executable and every G1 timing observation remained unchanged.
Neither qualification replaces the consumer-impact checks required before any
future default adoption.

## Provenance

The implementation independently derives Euclidean projection and determinant
algebra. The Douglas–Peucker method is described in Douglas and Peucker (1973),
[DOI bibliographic reference](https://doi.org/10.3138/FM57-6770-U75U-7727).
[Shewchuk's robust-predicate research](https://www.cs.cmu.edu/~quake/robust.html)
explains why a rounded orientation sign can misclassify geometry; this
implementation uses Go exact rationals and does not transcribe its algorithms.
The [VTK polyline decimation documentation](https://vtk.org/doc/nightly/html/classvtkDecimatePolylineFilter.html)
is a behavioral comparison: its priority strategy may collapse a closed loop
and does not enforce shared-graph constraints. No VTK source is imported or
copied. This primitive's contract comes from the stated mathematical checks
and local synthetic controls, without clinical validation claims.
