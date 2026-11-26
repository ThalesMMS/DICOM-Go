# Binary surface anti-aliasing

`roi.BinaryAntiAlias` is an opt-in offline estimate of a zero-level surface
from a regular binary mask. It returns an owned implicit field `Phi`, an owned
initial field, an unchanged mask snapshot and method/convergence parameters.
The source is never replaced. Cluster/segment identity, anatomy, clinical
measurements and 3D presentation are not inferred. No Twin UI or renderer is
changed: later 3D presentation remains GPU-only on both styles/platforms.

## Model, sign and physical scale

Positive field samples are inside; negative samples are outside. Coordinates
come from `VolumeMask.X/Y/Z` in millimetres. Axes must be finite, ascending,
regular to `1e-6*spacing`, and have at least two x/y samples. Reflection,
oblique reorientation and source DICOM geometry are caller responsibilities:
the mask has coordinate axes, not a direction/FoR module. A single z sample
is a 2D evolution with placeholder unit z spacing; extracting it as a padded
3D slab would invent thickness and requires an independently supplied physical
field affine. For actual 3D data, all three spacings are derived from its axes.

The initialization is distance to the nearest opposite-class voxel centre
minus half the minimum active-axis spacing, with the inside/outside sign.
A sequential separable squared Euclidean transform reuses the existing exact
parabola-envelope kernel; shifting coordinates to zero avoids origin-squared
overflow. This is an initialization in mm, not exact distance to voxel-box
faces. Initialization and final field are exposed separately for comparisons.

Evolution uses projected mean-curvature flow:

`next = phi + dt * (trace(Hessian(phi)) - grad(phi)^T Hessian(phi) grad(phi)/|grad(phi)|²)/(dimensions-1)`.

The existing level-set physical central/mixed derivative stencil is reused,
including its `|grad|² <= 1e-12` flat-gradient cutoff and float32 rate. Domain
faces repeat their boundary sample (reflecting/zero-flux convention). Inside
samples project to at least `SignMarginMM`; outside samples project to at most
its negative. Default margin is `1e-4*minimum spacing`, which keeps zero ties
from relabeling outside samples. This stricter positive margin is explicit;
it is not an automatic claim of equality to an ITK sparse-field solver.

Only the fixed initial band `abs(InitialPhi) <= BandWidthMM` evolves. Default
band is three times the largest active-axis spacing; values outside it remain
initial values and still contribute to boundary stencils. No reinitialization,
auto topology repair, statistical prior or volume-conservation correction occurs.
The evolved field remains in mm but is not a signed distance function. Surface
volume and continuous/extracted topology may change even though signs at voxel
centres are constrained; the result is an estimated surface, not voxel-exact
geometry. Thin structures must be examined quantitatively, not just visually.

## Stopping and resources

Defaults:100 iterations, RMS tolerance `.005*minimum spacing` mm, timestep
`minimum spacing²/(4*dimensions)` mm². An explicit positive timestep cannot
exceed that conservative finite-difference limit. Tolerance, band, margin and
all options must be finite and positive; margin is smaller than half the
minimum spacing. Empty/full masks and unavailable/malformed axes refuse,
rather than creating a missing surface or silently guessing background.

History records per-iteration RMS across the fixed band and the number of
samples at the sign margin. Stopping is `rms-tolerance` or `iteration-limit`.
RMS convergence reports numerical stabilization, not geometric accuracy or
clinical adequacy; no monotonic energy/volume claim is made.

Defaults bound16 Mi voxels,512 MiB logical request payload and2^31 requested
voxel-iterations; there is an absolute int32 voxel-index bound and100000-iteration
ceiling. Admission precedes output/EDT/band allocation and includes borrowed
mask plus snapshot, three float fields, distance state, fixed int32 band,
source/snapshot coordinates, sequential line scratch and the bounded history.
The logical reservation is not byte-exact runtime overhead, cumulative
allocations or peak RSS. Axis lengths/dimension products and physical arithmetic
are checked; nonfinite evolution refuses before projection can hide overflow.

The preparation transform is sequential with bounded scratch. Evolution helpers
come from the shared parallel budget, with a caller ceiling and maximum64 workers.
Disjoint updates followed by fixed-order RMS reduction are deterministic under
worker availability. Inputs remain stable during a call; result fields are
caller-owned and must stay stable during reading/extraction. Context is checked
during admission, transform lines/kernels, band preparation, evolution and final
publication. Cancellation returns nil, after workers join; no stale partial field.
Progress callbacks run on the caller after an iteration and are not retained.

## Extraction and qualification scope

The result structurally implements `mesh.Field`; callers may pass it to the
existing `mesh.Extract` with Iso0 and explicit extraction budgets. No extraction
is automatic or counted invisibly in the solver cap. Combined workflows reserve
solver plus extraction bounds (the cost fixture uses64 MiB each,128 MiB combined).
Marching Cubes estimates/interpolates a surface; VoxelFaces is the separate
voxel-exact reference. The default extractor pads the grid outside and closes
faces at domain edges; this differs from the solver's reflecting face stencil.

Independent oracles passed: separable initialization versus exhaustive all-pairs
physical distances, analytic quadratic curvature plus a separate Hessian, and
one projected step versus a direct stencil. Root executed 14 focal top-level
tests across `roi`/`mesh` and two affected ownership/cancellation tests under
`-race`, with no skips or failures. Sphere, oblique step, thin plate and hollow
shell exercised anisotropy, constraints, stopping and unchanged source. This is
synthetic mathematical qualification, not clinical or ITK solver equivalence.

### Actual surface comparison

The 15×13×9 phantoms use spacing .7/1.1/1.4 mm and at most 20 iterations.
Both initial and estimated surfaces use Marching Cubes; source VoxelFaces is
a separate voxel-exact reference. Values below are the actual focal results.

| Phantom | Source voxel volume mm³ | Initial surface volume mm³ | Estimated volume mm³ | Symmetric vertex-set distance mm | Initial→estimated components / cavity shells | Iterations / RMS mm |
| --- | ---: | ---: | ---: | ---: | --- | --- |
| sphere | 139.06201039 | 126.66123221 | 100.38963920 | 0.69981098 | 1→1 / 0→0 | 20 / 0.00749026 |
| step | 533.60998379 | 518.69764813 | 518.04718501 | 0.14001656 | 1→1 / 0→0 | 18 / 0.00343216 |
| thin | 106.72201024 | 86.71638804 | 33.95654906 | 0.58325386 | 1→1 / 0→0 | 20 / 0.00861915 |
| hollow | 120.73601102 | 113.97050874 | 95.82077291 | 0.69985247 | 2→2 / 1→1 | 20 / 0.00699718 |

**The thin plate lost 60.84% of initial estimated surface volume** even though
voxel-centre classification remained fixed. Sign constraints do not conserve
surface volume or protect thin geometry. Components/cavity shells happened to
remain unchanged in these four fixtures; no general continuous topology guarantee
follows. Source/estimated centre-mask cavity counts were respectively 0/0 for
sphere, step and thin, and 1/1 for hollow. Vertex-set distance is not continuous
Hausdorff distance. Solver logical reservation was 47,310 bytes per surface
fixture; extraction remains separately budgeted. Estimated surface payloads were
7,080 / 16,872 / 8,520 / 8,976 bytes respectively.

### Actual cost and native RSS

Darwin arm64, Go 1.27.1, GOMAXPROCS=2, GOMEMLIMIT=1GiB, CGO_ENABLED=1;
solver Workers=1. The fixed 24×20×12 sphere uses spacing .7/1.1/1.4 mm and
10 maximum iterations; every measured solver operation performed 10 iterations.
Fixture creation is outside each child benchmark timer. AntiAliasOnly includes
EDT initialization, outputs, band preparation and evolution; the combined profile
also extracts Marching Cubes. Solver and extraction caps are each 64 MiB
(128 MiB combined), not measured allocation or RSS limits. The operation families
perform different work and do not provide a quality-equivalent speed comparison.

The root Go benchmark executed three 200 ms samples per profile (12 rows).
Each cell reports median [min, max]. B/op is cumulative allocated bytes per
operation; it is not live scratch or resident memory.

| Profile | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| OriginalMarchingCubes | 61204 [61086, 61684] | 60160 [60160, 60160] | 57 [57, 57] |
| OriginalVoxelFaces | 118350 [117256, 118744] | 34144 [34144, 34144] | 46 [46, 46] |
| AntiAliasOnly | 2176966 [2171950, 2178663] | 160425 [160424, 160425] | 74 [74, 74] |
| AntiAliasAndExtract | 2243696 [2234334, 2249300] | 220584 [220584, 220584] | 131 [131, 131] |

RSS used a separately compiled native Mach-O arm64 test binary and twelve fresh
processes, one profile/sample per process. `/usr/bin/time -l` records maximum
resident set size of the whole Go test process, including runtime, fixture,
retained mask, solver and/or extraction. RSS is not isolated algorithm scratch;
these small process maxima do not establish lower algorithm memory use. Native
child benchmark results are separate observations from the Go benchmark above.

| Profile | Native ns/op | Native B/op | Native allocs/op | Whole-process maximum RSS bytes |
| --- | ---: | ---: | ---: | ---: |
| OriginalMarchingCubes | 60974 [60901, 61265] | 60160 [60160, 60160] | 57 [57, 57] | 14319616 [13991936, 14385152] |
| OriginalVoxelFaces | 118874 [115818, 121562] | 34144 [34144, 34144] | 46 [46, 46] | 14024704 [13795328, 14401536] |
| AntiAliasOnly | 2165714 [2140006, 2187315] | 160425 [160424, 160426] | 74 [74, 74] | 13549568 [13402112, 13598720] |
| AntiAliasAndExtract | 2223903 [2218435, 2225852] | 220586 [220584, 220638] | 131 [131, 131] | 13598720 [13451264, 14319616] |

All 12 native children passed with owned process cleanup and pinned build receipt,
executable and source pre/post checks. The solver adds work and allocation;
neither these costs nor the surface observations imply universal quality,
volume preservation, efficiency improvement, clinical adequacy or UI benefit.


The independent model follows public constrained-curvature mathematics; see
[ITK's AntiAliasBinaryImageFilter contract](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1AntiAliasBinaryImageFilter.html)
and its cited Whitaker2000 surface-estimation method. The dense fixed-band,
initialization, strict sign margin and stopping conventions above are this API's
own declared choices; no implementation or assets were transcribed.
