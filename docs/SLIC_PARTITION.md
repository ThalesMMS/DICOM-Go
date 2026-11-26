# Scalar SLIC partitions in physical space

`roi.SLIC` is an opt-in scalar 2D/3D spatially constrained Lloyd partition.
It produces connected units for a caller to select or process. It does not
classify tissue, create DICOM Segment Numbers, train a model, change an existing
segmentation or activate a desktop tool. A 2D image has `Size[2]=1`.

## Parameters and distance

`SLICGrid` borrows dense x-fastest `Values` and an optional `Available` bitmap;
spacing is positive finite millimetres. The caller must keep both slices stable
until return. Scalar values must share a declared unit/domain. Multichannel data
and inferred weighting of incompatible units are not accepted by this scalar API.
`IntensityScale` is a required positive finite normalization factor in that same
scalar unit. `TargetStepMM` is required positive finite, and `Compactness` finite
nonnegative. The distance is

`((value-centerValue)/IntensityScale)^2 + Compactness^2 * physicalDistance^2 / TargetStepMM^2`.

The seed lattice rounds `TargetStepMM/spacing` to the nearest positive integer
voxel step, clamped to the dimension. `SeedStepVoxels` and `SeedStepMM` report the
resulting physical quantization; the requested size is not an exact size promise.
Each cell picks its physically nearest available voxel to the cell center;
ties use x-fastest traversal. Empty cells contribute no seed. Initialization has
no random choice or gradient perturbation. IDs are deterministic for stable input,
internal to this partition, and not stable across edited inputs.

Assignment examines each seed's index window extending two lattice steps about
its current center. If domain holes leave a valid voxel outside all windows,
it explicitly searches every initialized center for that voxel. This fallback
is charged to the work budget; unavailable voxels are not silently assigned.
Center means are updated serially. Equal distances prefer the earlier seed.
A zero compactness produces intensity-only assignment within those windows and
may collapse unused seeds; no anatomical meaning follows from any cluster.

## Domain, connectivity and convergence

A nonempty admitted grid with no available voxels returns a zero-label map and
zero clusters. Zero dimensions and mismatched slice lengths are invalid. NaN and
infinities are unavailable by default; `RejectNonFinite` rejects them in the
available domain. Masked-out values are never part of numeric normalization.
Finite magnitudes that cannot safely represent the squared distance are rejected,
instead of overflowing into an arbitrary label.

Label zero means unavailable; positive IDs designate connected units. Face
connectivity means 4-neighbor 2D/6-neighbor 3D; full connectivity means
8-neighbor 2D/26-neighbor 3D. After assignment, disconnected pieces of a label
are split deterministically, never silently merged with a different label.
Small fragments remain explicit units. `ConnectivitySplits` and final cluster
count expose the split. Output may therefore have more units than initialized
seeds, still subject to `MaxClusters`.

`Converged` means the assignment stopped changing before the connectivity pass.
`IterationLimitReached` distinguishes an exhausted iteration cap. Neither flag
means a global optimum or clinical convergence. `Objective` is the mean last
assignment distance before connectivity splitting, not a claim about the final
refined partition energy. Parameters, actual iterations, availability counts,
initial/final cluster counts and logical reservation/work accompany the map.

## Ownership, limits and cancellation

The returned label map is owned; calls share no mutable state or persistent
cache. The implementation is serial, so there is no hidden parallel budget.
Zero limit fields use finite defaults: 16,777,216 voxels, 65,536 clusters,
1 GiB logical bytes, 100,000,000 work units and 100 maximum iterations. Requested
`Iterations` is required positive and must fit the maximum. Negative limits
and unrepresentable dimensions are rejected.

Before working allocation, checked admission reserves
`32768 + 64*voxelCount + 256*latticeCellCount` bytes. This covers borrowed-input
accounting, validity, distances, old/final labels, a full flood queue and reusable
centers/mean accumulators with conservative slots. It is a logical live-storage
budget, excludes allocator/runtime overhead and is not measured RSS. The lattice
cluster quota is conservative even for empty cells. Connectivity output is
separately checked against the same cluster quota. Validation, assignments,
fallbacks and flood scans consume the finite work quota; context is polled during
execution and before publication. Budget or context errors return nil, never a
partial map presented as a converged result. A caller may supply a context deadline.

## Qualification recipe and current status

Root qualification on macOS 27.0.1 / arm64, Go 1.27.1, completed seven focused top-level tests and the concurrent ownership race case. The selected package and source guards were exact; no focal/race failures or skips occurred. This is scoped functional/race evidence, not a full race suite; canonical baseline qualification is recorded below.

- `TestSLICIndependentPhysicalDistance`: independent brute-force first-step
  distance/partition oracle; compares equivalence relations rather than label IDs.
- `TestSLICPhantomsPhysicalCompactness`: constant scalar 2D/anisotropic 3D and
  sharp edges; physical lattice, convergence, compactness response and coverage.
- `TestSLICTextureOwnershipAndConcurrentDeterminism`: synthetic texture,
  connectivity, unchanged source and concurrent independent owned outputs.
- `TestSLICConnectivityAndUnavailablePolicy`: diagonal pieces under face/full
  connectivity, unavailable/nonfinite and admitted empty-domain policies.
- `TestSLICAdmissionAndCancellation`: voxel/cluster/byte/work/iteration/output
  quotas, initial and in-operation cancellation, invalid metric/geometry and
  squared-distance overflow.

Connectivity is checked by a separate disjoint-set graph oracle, independent
of the implementation's flood queue. A fixed benchmark recipe covers constant
2D, textured 2D and anisotropic 3D. Input creation/preflight is outside operation
timing. Metrics include operation time/B/allocs and explicitly logical input,
label, reservation/work counts. Whole-process peak RSS, if collected, includes
input/preflight/runtime/harness; none of those counts is relabelled as RSS.
Native cost is characterized below; interaction efficiency has not been measured.

`TestSLICITKOracleOptIn` requires `SLIC_ITK_FIXTURE` produced by the independent
public-API ITK recipe. Default absence is a skip, not an oracle pass. Six finite
isotropic scalar cases use odd 3-voxel cells, no initialization perturbation,
five iterations and one ITK work unit; physical/index distance normalization
has the same isotropic weight/3 scaling in the distance metric. This does not
make the solvers identical: ITK searches one `SuperGridSize` step in each
index direction and rounds the moving center with `RoundHalfIntegerUp`, whereas
Go searches two seed steps and bounds its window with floor/ceil. ITK's
connectivity pass may merge small fragments, whereas this API only splits;
partition-pair disagreements are recorded, not hidden by matching arbitrary IDs
or an invented precision threshold. ITK elapsed time and native cost are separate
measurements with their own setup/runtime limits. The preserved generation2 and optimized Release generation3 actual comparisons
are recorded below; their documented differences remain part of the profile.
The root recipe validates each actual reference fixture's procedural values,
geometry, zero-based nonnegative integer labels, cluster counts and face-connected
label graph before comparison; label zero is a valid ITK cluster. A degenerate
single-label constant reference is rejected because it would not qualify the
specified spatial partition. The comparison reports partition disagreement,
not an identical-algorithm or bit-exact ITK claim.

Canonical library `make check` and `make docs-check` passed after source
integration; focused/race, cost and reference evidence is recorded below. No UI was integrated: apply/reject/undo must be
qualified separately before any desktop exposure, and 3D presentation remains
GPU-only on both platforms/frontends. Interaction efficiency has not been measured.

## Actual native cost characterization

The fixed three-cell recipe ran five one-call benchmark measurements per cell
(`benchtime=1x`), with
`GOMAXPROCS=2`, `GOMEMLIMIT=1GiB` and a single compiled test binary. Each cell used
a fresh process. SLIC options requested up to ten algorithm iterations per call.
Input construction and preflight were outside operation timing;
SLIC allocations, assignment/update and connectivity splitting were timed.
Times and allocation counts are medians with observed min–max, not confidence
intervals, steady-state throughput or a speedup claim.

| Case | ns/op median (min–max) | B/op median (min–max) | allocs/op median (min–max) | Whole-process peak RSS, bytes |
|---|---:|---:|---:|---:|
| constant2D32 | 1,630,125 (1,502,833–1,709,750) | 32,864 (32,864–32,880) | 11 (11–12) | 10,240,000 |
| texture2D64 | 5,567,417 (5,262,875–6,341,833) | 129,312 (129,312–129,312) | 11 (11–11) | 9,830,400 |
| anisotropic3D | 11,190,167 (11,020,375–11,388,333) | 147,424 (147,424–147,424) | 11 (11–11) | 10,485,760 |

RSS is the Darwin `time -l` maximum for the complete case process, including
fixture, preflight, harness and runtime. It is not per-call retained heap or a
measurement of the logical reservation. Caller input and owned labels are
separately accounted below; the work counts are charged algorithmic visits, not
CPU instructions. Every sample in each case had these same logical counts.

| Case | Caller input bytes | Owned label bytes | Logical reservation bytes | Connected clusters | Charged work |
|---|---:|---:|---:|---:|---:|
| constant2D32 | 8,192 | 4,096 | 107,520 | 36 | 208,738 |
| texture2D64 | 32,768 | 16,384 | 325,888 | 231 | 901,252 |
| anisotropic3D | 36,864 | 18,432 | 343,040 | 222 | 1,843,839 |

## Actual independent reference and delivery gates

The preserved ITK generation2 characterization produced six valid procedural
partitions with nine connected labels each. Pair disagreements were zero among
3,240 pairs in each 2D case and 29,403 pairs in each 3D case. That oracle used
unoptimized C++17 while Go used its default optimized compiler, so those elapsed
values cannot establish a fair comparative speedup. Its complete archive set
was observed afterward; one archive had not been individually pinned before
that generation. The separate generation3 recipe pins all eleven actual linked
SDK archives and requests Release `-O3 -DNDEBUG`, without rebuilding the SDK.

The generation3 oracle was compiled in Release with C++17, `-O3 -DNDEBUG`
and arm64 architecture; all eleven linked SDK archive hashes were pinned before
its build and verified afterward. Its six procedural label maps equal generation2
apart from elapsed metadata. All reference labels form connected components and
each case has nine clusters. The explicit Go comparison passed all six subcases
without failure or skip. Parameters: isotropic 1 mm spacing, sizes 9×9×1 or
9×9×3, 3 mm target/3-voxel super-grid step, unit intensity scale, five iterations,
compactness 4 (constant/edge) or 16 (texture), initialization perturbation off,
ITK connectivity enforcement on and one ITK work unit. Go uses face connectivity.

| Case | Disagreeing label pairs / compared pairs | ITK Release elapsed ns | Go elapsed ns | Go charged work | Clusters, both |
|---|---:|---:|---:|---:|---:|
| constant2D | 0 / 3,240 | 480,458 | 66,250 | 4,265 | 9 |
| edge2D | 0 / 3,240 | 215,000 | 71,958 | 4,265 | 9 |
| texture2D | 0 / 3,240 | 204,500 | 30,292 | 4,265 | 9 |
| constant3D | 0 / 29,403 | 377,333 | 88,250 | 12,759 | 9 |
| edge3D | 0 / 29,403 | 295,333 | 87,000 | 12,759 | 9 |
| texture3D | 0 / 29,403 | 291,875 | 82,959 | 12,759 | 9 |

These are single latency observations, not repeated matched throughput samples,
confidence intervals or a statistical speedup result. ITK times its filter
`Update`; Go times the API call with its own admission, allocations and
connectivity. Input generation is outside both observations, but the pipelines
and runtime costs differ. Zero pair disagreement qualifies only these six
procedural partitions, not identical algorithms or universal numerical/clinical
accuracy. Window extent, rounding and split-versus-merge differences above remain.

Independent review verified the actual graphs/parameters, native cost rows,
Release flags, archive/binary hashes, normal termination, drained output and
exact selected/owned source guards. No raw failed or historical generation was
relabeled as a newer optimized execution.


After canonical integration, Go 1.22.12 ran the same seven selected top-level
ROI tests with the external linker and default vet behavior: all seven passed
in one package without failure or skip. The current native toolchain completed
`make docs-check` and the full library `make check` baseline; the baseline process
completed normally in 137.650670 seconds. Its 4,810-file module source snapshot,
file modes, five owned postimages and selected ROI dependency/path guards remained
exact before and after execution; process output drained and the owned group was
reaped. Independent audit confirmed the minimum-toolchain, documentation and
library results. These gates qualify the documented scalar library profile;
they do not establish a full race suite or desktop adoption.

The ordinary baseline ran with `SLIC_ITK_FIXTURE` unset, so the optional ITK test
remained absent/skipped by default. This is distinct from the six explicitly
executed Release-reference comparisons above; no zero-optional-skips claim is
made for the ordinary baseline. No desktop integration or interaction-efficiency measurement
was performed. Original apply/reject/undo remains conditional on a future UI
integration; existing segmentation is never changed by this library API.
