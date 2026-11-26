# Quadric decimation scratch storage

`mesh.DecimateQuadric` uses two request-owned uint32 mark arrays and two reusable
neighbor slices to collect live adjacency. A generation stamp deduplicates each
collection without constructing a map for every collapse and heap refresh.
The two slots keep the first neighborhood intact while collecting the second.
Generation wrap clears both mark arrays; it does not invalidate the retained
slice. There is no global pool or cache shared between requests.

The arrays cost eight bytes per input vertex, plus the retained neighbor slices.
Other decimator storage, heap entries and output arrays still allocate. This is
a time optimization with a small allocation-count improvement, not a large
memory reduction. The topology/link condition, flipped-face checks, boundary
policy, quadric costs, reduction target and error controls retain their existing
meaning. Neighbor iteration follows first occurrence in the live face lists.
Equal-cost collapse ordering is not an API guarantee: the previous map traversal
and initial edge heap were already unordered. General bit-identical output or
determinism is not promised.

## Local before/after measurement

Base: `11fad2836c43e6e1fefe69336e31d652915304ed`. Compiler: Go 1.27.1,
Darwin arm64, Apple M4 Pro. Both precompiled binaries used the same synthetic
fixtures, GOMAXPROCS=1 and GOGC=100. Five process pairs alternated AB/BA; each
process ran 20 operations for each case. Builds, tests and GUI campaigns from
the other task owners were held during the timed campaign. Mesh construction
was outside the timer; each timed call includes API validation, initialization,
collapses, heap maintenance and output construction.

| Case | Input → output triangles | Median before → after | Time change | Median bytes/op before → after | Allocs/op before → after |
| --- | --- | --- | --- | --- | --- |
| Closed32 | 1920 → 576 | 2.164 → 1.929 ms | −10.83% | 1,444,008 → 1,446,072 | 13,964 → 13,893 |
| Closed70 | 9520 → 2856 | 12.164 → 11.033 ms | −9.30% | 9,238,563 → 9,229,202 | 71,723 → 71,068 |
| Boundary70 | 4690 → 1408 | 5.810 → 5.245 ms | −9.74% | 4,300,784 → 4,288,867 | 35,276 → 34,953 |

The asymmetric closed and open surfaces target 70% reduction; Boundary70 keeps
the boundary. The observed time ranges were 2.146–2.243 → 1.883–2.491 ms,
12.122–12.226 → 10.937–11.775 ms, and 5.770–5.858 → 5.196–5.370 ms respectively.
Closed32 has overlapping ranges and one slower candidate process. These are
local synthetic medians, not a universal speedup, VTK performance comparison,
clinical-data result or RSS measurement.

Five independent outputs per case and version were compared against thresholds
fixed before the campaign: area/volume relative change ≤1%, surface-distance
relative change ≤3% with a 1e-4 mm absolute floor, triangle-count difference ≤2,
closed manifold consistency and preserved open boundary. All passed. In this
corpus, the medians and ranges of area, volume, maximum/mean surface distance
and reported quadric error were equal between versions. This scoped geometric
equivalence does not establish bit-identical meshes for arbitrary inputs.

The committed benchmark is reproducible with:

```sh
GOMAXPROCS=1 GOGC=100 go test ./mesh -run '^$' -bench '^BenchmarkDecimateQuadric$' -benchtime=20x -benchmem -count=5
```

That command measures the current version; a before/after comparison also needs
the same benchmark harness over the pinned base. Tests compare stamped
membership with an independent scan of live triangles before and after forty
real collapses, exercise duplicates/dead faces/isolated vertices and generation
wrap between slots, and run independent concurrent requests under the race
detector. Existing geometric and topology tests remain in the package.

## VTK option assessment

The public `vtkQuadricDecimation` interface at VTK revision
`6ff0d2ec0d2ce59f7df41574c3278c61ca80b341` exposes optional volume preservation
and an attribute error metric, both disabled by default. Volume preservation
changes the optimization objective through normal-direction error constraints;
it is not equivalent to the existing boundary/topology guards or a strict
global volume bound. A new option needs a separately qualified algorithm and
error contract, so it is not added by this storage optimization.

The current `mesh.Mesh` stores positions and triangle indices, with optional
normals that decimation recomputes; it has no general per-vertex scalar/vector
attribute model. An attribute error option would first require an ownership,
interpolation and export contract for those attributes. No no-op option or
unqualified preservation claim is exposed. This is an assessment of the public
interface, not a copy of the VTK implementation.

