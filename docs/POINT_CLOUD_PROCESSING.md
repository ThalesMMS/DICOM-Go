# Bounded point-cloud cleanup and reduction

`mesh.ProcessPointCloud` derives an owned unconnected point subset in patient
LPS millimetres. It neither mutates the source nor constructs triangles. The
input explicitly declares `Units: "mm"` and uses the same float32 coordinates
as `mesh.Mesh`. Float64 arithmetic cannot recover detail already lost in those
coordinates. Keep the borrowed input and attributes stable during the call.

```go
cloud := &mesh.PointCloud{
    Units: "mm",
    Points: pointsLPS,
    Normals: suppliedNormals,
    Colors: colorsRGBA,
    Scalars: []mesh.VertexScalarAttribute{
        {Name: "Confidence", Values: confidence, Units: "1"},
    },
    Protected: preserveRareStructures,
}
result, err := mesh.ProcessPointCloud(ctx, cloud, mesh.PointCloudOptions{
    Neighbors: 8, SigmaFactor: 2,
    RemoveOutliers: false,
    MinDistanceMM: 0.4, DistanceToleranceMM: 1e-9, Seed: 73,
    MaxPoints: 100000, MaxNeighbors: 16,
    MaxBytes: 32 << 20, MaxWork: 20000000,
})
```

For each source point, the statistical score is the mean Euclidean distance
to `Neighbors` other source points (duplicates count as distinct neighbors).
The threshold is the mean of all scores plus `SigmaFactor` times their
**population** standard deviation. A strict score above the threshold marks
an `OutlierCandidate`. `Neighbors: 0` disables this analysis; a positive value
must be smaller than the source count and at most `MaxNeighbors`.

Candidates are diagnostic proposals. Only explicit `RemoveOutliers: true`
applies statistical removal. The synthetic unequal-density thin-line fixture
shows **5/5 valid sparse points flagged incorrectly**, with zero applied losses
in proposal mode. A caller-declared `Protected` mask preserves the same five
points even when removal is requested. Density does not identify anatomy:
an unprotected sparse structure can be lost when the caller applies proposals.

Distance reduction visits protected points first and the remaining points in
a seeded SplitMix64/Fisher-Yates permutation, accepting a point only if no
already accepted point lies closer than
`MinDistanceMM - DistanceToleranceMM`. The strict distance comparison uses the
stored positions promoted to float64, including tiny or very large positive
separation options. Zero minimum distance disables reduction. Positive tolerance
must be strictly smaller than the requested minimum. The output is in source
order. This is a greedy subset, with no maximal-density or optimality claim.
Repeated input/options produce exactly the same subset and maps. Two protected
points that violate the effective separation cause an error, preserving both
the preservation policy and the separation contract.

`OriginalIndices` maps each output point to its source. `InputToOutput` maps
each source point to its output index, or `-1` when removed. `Removed`
distinguishes applied statistical removal from distance suppression. Normals,
RGBA colors, finite named scalar values/units and protection flags are copied
exactly. Normal magnitude/orientation is preserved rather than inferred. Scalar
metadata follows `VertexScalarAttribute`: at most 32 attributes, unique ASCII
names, finite values, explicit optional units. Unsupported application metadata
can be remapped by `OriginalIndices`; it is never silently claimed as supported.

## Admission, index and cancellation

Zero limits select 1,000,000 points, 64 neighbors, 256 MiB logical payload and
100,000,000 work units. Negative limits are invalid. Point indices must fit
signed 32-bit index nodes. Finite input/options and attribute counts are checked.
An in-place deterministic selection builds a balanced KD tree. Its comparisons,
neighbor heap operations, node visits, point/scalar checks, sampling and copies
consume the work quota and check cancellation. Degenerate point distributions
and adverse selection cannot escape the finite work limit.

Oversized scalar names/units are rejected by constant-size diagnostics before
the shared attribute validator can quote caller data. The regression supplies
10 MB metadata strings with a 64-byte array quota and a one-unit work quota.

Before allocating arrays the operation reserves the worst-case detached output,
index, permutation, maps, candidate flags, scores, removal reasons and scratch.
On this 64-bit host the reservation is `59*N + 16*K` bytes, plus `12*N` for
normals, `4*N` for RGBA colors, `N` for protection, and `8*N*A + sizeof(attribute)*A`
for scalar attributes. `N` is the input count, `K` the selected neighbor count
and `A` the scalar count. This conservative payload includes scratch for stages
that may be disabled. It excludes caller input, Go allocator/runtime overhead
and the bounded recursion/call stack. It is **not a process RSS ceiling**.

`PointCloudLimitError` wraps `mesh.ErrBudgetExceeded`, giving the resource,
usage and limit. Invalid contracts wrap `mesh.ErrInvalidField`. Cancellation
returns the context error. Every error returns a noncomplete diagnostic report
and **nil cloud and all nil result arrays**, including interruption during
final copying. No partial subset can appear complete. Successful reports give
effective options, counts, threshold, payload reservation and charged work.

## Reconstruction experiment and decision

The repeatable experiment is confined to
[`pointcloud_reconstruction_test.go`](../mesh/pointcloud_reconstruction_test.go).
It samples a nearest-point tangent-plane signed field, then uses the existing
Marching Cubes extractor with **open grid boundaries**. The synthetic R=10 mm
sphere supplies analytic oriented normals; ordinary unordered points do not
supply this extra information. No normal estimation, surface reconstruction
API, clinical preset, viewer job or CPU 3D renderer is introduced.

Before evaluation the sphere gate is fixed at at most **0.5 mm sampled surface
error**, Euler characteristic 2 and zero defective/unpaired edges. Experiment
admission is at most 10,000 points, 150,000 grid samples, 64 MiB grid/index payload
and 50,000,000 charged index visits/comparisons. Extraction has a separate
8 MiB mesh-output and 200,000 triangle quota, with one worker. Topology analysis
uses maps bounded by those mesh counts; its allocator overhead is measured in
process RSS rather than charged to the numeric array reservation. The campaign
reports vertices and triangle-centroid distances to the analytic sphere;
these are **sampled errors, not an exact Hausdorff bound**. Source coverage is
the largest nearest-retained-point distance over the original 2,048 points.

| Input | Grid spacing mm | Retained points | Max sampled error mm | Source coverage mm | Euler | Defective edges |
|---|---:|---:|---:|---:|---:|---:|
| Original coherent normals | 1 | 2048 | 0.031710 | 0 | 2 | 0 |
| Reduced coherent normals | 1 | 756 | 0.056498 | 0.897912 | 2 | 0 |
| Original coherent normals | 0.5 | 2048 | 0.014631 | 0 | 2 | 0 |
| Reduced coherent normals | 0.5 | 756 | 0.059917 | 0.897912 | 2 | 0 |
| Half-sphere reversed normals | 1 | 2048 | 9.488518 | 0 | -23 | 162 |
| Missing upper cap | 1 | 1638 | 2.561590 | 8.685367 | 1 | 28 |

Defective edges count undirected edges without exactly two oppositely wound
faces. The last two cases also have 162 and 28 unpaired directed edges. Their
failure demonstrates ambiguous sign/orientation and invented geometry near
unsampled borders. Finer grids improve the ideal original sphere but do not
remove sampling bias: the reduced sphere's worst sampled error rises slightly
at 0.5 mm. Reconstruction is **not promoted** beyond this experiment.

Keeping the original sphere needs 49,152 coordinate/normal payload bytes and
preserves all input samples. The reduced cloud needs 18,144 bytes but loses up
to 0.897912 mm of source-point coverage. At 0.5 mm, reconstructing the original
requires a 470,596-byte grid and a 271,536-byte mesh; the reduced case requires
the same grid and a 272,832-byte mesh. A smaller cloud does not imply a smaller
reconstructed surface or stronger anatomical preservation.

## Measured local cost and replay


The pipeline fixture is a 10,000-point sphere plus one remote point. Original
means an owned copy with all processing disabled; clean explicitly removes
outliers with K=8 and factor=2; reduced also applies 0.4 mm separation/seed=73.

| Phase | Median ms/op | Median B/op | Output points | Process peak RSS bytes |
|---|---:|---:|---:|---:|
| Pipeline original | 0.118469 | 481744 | 10001 | 14598144 |
| Pipeline clean | 14.028938 | 686743 | 10000 | 13713408 |
| Pipeline reduced | 17.936814 | 555665 | 3766 | 13434880 |
| Reconstruction original, h=0.5 | 137.899625 | 3313600 | 2048 source | 15974400 |
| Reconstruction reduced, h=0.5 | 76.904250 | 3288029 | 756 source | 15335424 |

Both processing phases remove exactly one true remote outlier. Pipeline work
is 20,002/1,507,226/1,797,445 units, with 710,071/710,199/710,199 reserved bytes.
Reconstruction costs include field sampling, extraction, topology counting and
sampled error/coverage measurement. None is an image rendering benchmark.

Replay from the library module:

```sh
mkdir -p /tmp/pointcloud-replay
GOMAXPROCS=4 go test -p 2 ./mesh -run '^TestPointCloud' -count=1 -v
GOMAXPROCS=4 go test -p 2 -race ./mesh -run '^TestPointCloud' -count=1
GOMAXPROCS=4 go test -p 2 ./mesh -c -o /tmp/pointcloud-replay/mesh.test
# Repeat separately for original, clean and reduced; all three are retained.
GOMAXPROCS=4 /usr/bin/time -l /tmp/pointcloud-replay/mesh.test \
  -test.run='^$' -test.bench='^BenchmarkPointCloudPipeline$/^original$' \
  -test.benchtime=200ms -test.count=3 -test.benchmem
# Repeat separately for original and reduced.
GOMAXPROCS=4 /usr/bin/time -l /tmp/pointcloud-replay/mesh.test \
  -test.run='^$' -test.bench='^BenchmarkPointCloudReconstructionExperiment$/^original$' \
  -test.benchtime=200ms -test.count=3 -test.benchmem
make docs-check
```

Focused tests qualify exact nearest neighbors against brute force, duplicates,
unequal density, thin-structure bias/protection, attribute/map ownership,
repeatability and separation, exact byte/work boundaries and cancellation in
all stages. The CPU numeric preprocessing has no Twin rendering integration.
No GPU, external PACS, clinical data or independent VTK runtime was tested.

Public behavioral references are
[VTK statistical outlier removal](https://vtk.org/doc/nightly/html/classvtkStatisticalOutlierRemoval.html),
[VTK distance sampling](https://vtk.org/doc/nightly/html/classvtkPoissonDiskSampler.html)
and [VTK reconstruction](https://vtk.org/doc/nightly/html/classvtkSurfaceReconstructionFilter.html).
The implementation uses public mathematical definitions and synthetic fixtures;
no VTK source is copied and no numerical parity is claimed.
