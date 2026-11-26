# Nondestructive mesh geometry quality

`mesh.AnalyzeQuality(ctx, mesh, options)` inspects an indexed surface in patient
LPS millimetres. It does not repair or change vertices, normals, winding or face
indices. External attribute arrays remain caller owned and are not accessed.
This geometric report is not clinical or manufacturing qualification. It does
not require a closed surface: open surfaces and nested disjoint shells are not
reported as intersections merely because of their topology or containment.

## Metrics and categories

The report has triangle area (mm²), minimum internal angle (degrees), and a
normalized aspect ratio, with sample count, minimum, maximum and mean. The
aspect ratio is `sqrt(3) * longestEdge² / (4 * area)`; an equilateral triangle
has ratio one. It follows the public longest-edge/shortest-altitude definition
normalized for a triangle ([Altair definition](https://help.altair.com/simlab/help/en_us/topics/meshing/elementqualitydefinitions_tri.htm)).
Zero-area triangles, including repeated vertex indices, have no defined
angle/aspect sample and make intersection conclusions inconclusive. Positive
areas below `MinAreaMM2`, angles below `MinAngleDegrees` and ratios above
`MaxAspectRatio` have separate counts and bounded face indices. These thresholds
are optional: zero disables that metric's threshold. Area statistics include
zero-area triangles. Minimum angle must be at most 60 degrees; a positive
maximum aspect ratio must be at least one.

Duplicate indexed faces are detected by sorting all three indices, ignoring
winding. A group of k duplicates contributes k−1 duplicate findings and k−1
representative links, not all k(k−1)/2 pairs. Faces with coincident coordinates
but different indices are instead assessed geometrically. Intersection pairs
are classified separately as interior crossing, coplanar positive-area overlap,
boundary contact, or uncertain. Contacts are retained separately from crossings.
A tolerance-near parallel plane pair is uncertain, not silently clear. Coplanarity is certified
separately: float32 dyadics are scaled by 2^149 into integers of at most 277 bits;
a 3×3 orientation determinant needs at most 840 bits. The constant-domain exact
check uses math/big only for near-parallel candidates not on a common exact
axis-aligned slice. Tiny tilted planes within roundoff remain uncertain. Other
intersection predicates remain floating-point computations.

The intersection scope excludes pairs sharing any vertex index and excludes
zero-area triangles. `AdjacentPairsSkipped` records the former. This excludes
legitimate adjacency but also means that folded adjacent triangles require a
separate topology/adjacency inspection. Containment is not a crossing test.
The scalar computations use float64 over the original float32 coordinates;
plane/interval tests and a normalized 2D separating-axis test implement public
geometry mathematics. They are conservative floating-point predicates with a
coordinate-scale roundoff band, not arbitrary precision exact predicates.
([CGAL terminology about predicates and constructions](https://doc.cgal.org/Manual/3.1/doc_html/cgal_manual/Kernel_d/Chapter_predicates_constructions_d.html)).

`DistanceToleranceMM` governs pair proximity/contact classification. It does not
alter coordinates or merge vertices. Scaling coordinates by s requires scaling
this distance by s and `MinAreaMM2` by s²; angle/aspect thresholds do not scale.
Nonfinite coordinates/normals, mismatched normals and missing triangle indices
are diagnosed before geometry metrics or the spatial search run. The validation
itself is cancellable and work limited; a partial validation is not a verdict
that the input is valid.

## Bounded work and interpretation

Zero limits choose finite defaults:

| Limit | Default | Meaning |
|---|---:|---|
| MaxVertices | 10,000,000 | Input vertex limit |
| MaxTriangles | 10,000,000 | Input face limit |
| MaxBytes | 256 MiB | Logical analysis array payload reservation |
| MaxWork | 20,000,000 | Input steps, merge-sort copies, duplicate scan and sweep inspections |
| MaxCandidates | 1,000,000 | Nonadjacent overlapping AABB pairs entering geometry predicates |
| MaxResults | 1,024 | Each finding index list and the combined pair list |

The reservation uses the actual Go struct sizes for two AABB arrays and two
index-key arrays per triangle, plus seven fixed-capacity uint32 index lists and
one fixed-capacity pair list, plus a conservative 16 KiB logical scratch
reservation for the exact dyadic predicate (fixed <=840-bit limbs/headers). Overflow and the byte cap are checked before
allocation. This excludes caller-owned source arrays, the fixed report/stack
and Go allocator/runtime overhead; it is not a process-RSS promise. Bounded
slices never grow past their reserved capacities. Result truncation still allows
complete counts; `ResultsTruncated` announces that retained IDs are incomplete.

Stable merge sorts check cancellation/work on every copied item. The spatial
search sweeps sorted X bounds and tests Y/Z overlap, without an all-pairs array.
Every sweep inspection consumes work, including rejected/adjacent pairs. Dense
AABBs can still require quadratic comparisons in principle, but the explicit
work/candidate caps stop this request before unbounded work. A preexisting mesh
locator lacks context/quota construction, so this request-local bounded sweep
is used instead. No goroutines or background work outlive the call.

`Complete` describes traversal of the stated scope. `State` is `complete`,
`inconclusive`, `limited`, `canceled`, `invalid`, or `partial`. Degenerate faces
or uncertain pairs set `IntersectionsConclusive=false`; a finished scan can thus
have `Complete=true` and `State=inconclusive`. Every incomplete/error return
sets `IntersectionsConclusive=false`, including invalid input and early limits. A complete scan is not a claim of
suitability or absence of problems outside the analyzed scope. On work/candidate
or byte/input limits the function returns a partial report and
`*QualityLimitError` wrapping `ErrBudgetExceeded`. Cancellation returns the
context error and a partial report. Invalid input/options wrap `ErrInvalidField`.
Callers must inspect the error, completion, scope, categories and truncation.


The root ran this nine-top selector, with seven category subcases: all passed,
with no skips or failures. Source guards before/after matched the six owned
files, 4,707 private library files and 8,675 primary files. The actual command
also used `-json -p=1 -timeout=90s`, with a 300-second supervisor bound.

The focused selector is:

```sh
go test ./mesh -run '^TestQuality(AnalyticMetricsAndSourcePreservation|DegenerateAndOppositeWindingDuplicates|IntersectionCategories|AdjacentAndNestedShells|PhysicalScale|InvalidBeforeGeometry|LimitsAndBoundedResults|CancellationDuringSearch|NearParallelIsUncertain)$' -count=1
```

The tests use synthetic analytic geometry, including collinear/repeated-index
faces, opposite-winding duplicates, legitimate adjacency, coplanar overlap and
edge contact, crossing and point contact, nested tetrahedral shells, physical
scaling, malformed input, quotas and deterministic mid-search cancellation.
They check source vertices/normals/indices remain unchanged.

Local cost probes cover 4,096 spatially separated faces and 512 overlapping faces
with a 64-candidate cap:

```sh
go test ./mesh -run '^$' -bench '^BenchmarkQuality(Separated|DenseLimited)$' -benchtime=1x -count=3 -benchmem
```

The root compiled the same-source mesh test binary separately, then ran the
benchmark selector above through that binary with three one-iteration rows per
scenario. Both the focal/compile and benchmark processes exited zero, were
reaped, drained their output and left no owned process group. The benchmarks
validate their own complete/capped result conditions: the separated surface
finishes with zero narrowphase candidates; the dense surface returns the
expected budget error after exactly 64 candidates and remains incomplete.

Host reported by the raw benchmark: macOS (`darwin/arm64`), Apple M4 Pro. The
recorded environment used `GOMAXPROCS=2`, `CGO_ENABLED=1`, `GOMEMLIMIT=1GiB`,
`GOTOOLCHAIN=local`, `GOWORK=off`. These receipts do not record a Go version.
No GPU or external mesh implementation is involved in these local probes.

| Scenario | Observation | ns/op | B/op | allocs/op |
|---|---:|---:|---:|---:|
| 4,096 separated faces | 1 | 2,605,417 | 590,832 | 14 |
| 4,096 separated faces | 2 | 2,208,708 | 590,816 | 13 |
| 4,096 separated faces | 3 | 2,129,042 | 590,816 | 13 |
| 512 overlapping faces, capped at 64 candidates | 1 | 339,333 | 74,544 | 14 |
| 512 overlapping faces, capped at 64 candidates | 2 | 271,750 | 74,544 | 14 |
| 512 overlapping faces, capped at 64 candidates | 3 | 272,083 | 74,544 | 14 |

| Scenario | Median ns/op (range) | Median B/op (range) | Median allocs/op (range) |
|---|---:|---:|---:|
| Separated | 2,208,708 (2,129,042–2,605,417) | 590,816 (590,816–590,832) | 13 (13–14) |
| Dense, limited | 272,083 (271,750–339,333) | 74,544 (74,544–74,544) | 14 (14–14) |

These are bounded local cost observations for different workloads, not a
comparative optimization result or universal latency guarantee. Dense work
is limited rather than fully analyzed. The scenarios exercise an axis-aligned
plane fast path; they do not measure the fixed-domain exact dyadic predicate
on arbitrary oblique coplanar input. B/op is Go allocation accounting for the
call, not process RSS; source construction is outside the timer. The byte quota
is the conservative logical reservation described above, not a promise that
B/op or whole-process RSS equals that reservation. Three single-iteration
observations do not establish steady-state statistics or production throughput.

The integration library `make check` passed in 195.40 seconds on the same
five Go postimages, with all 8,681 repository source paths unchanged before and
after. The final guide is a documentation-only update; documentation validation
is required for that update. No race check, external peer runtime, native
Windows run, Twin UI or clinical/manufacturing qualification is claimed.
