# Masked physical rank filters

`imagefilter.MaskedRank` and `MaskedRankRegion` compute exact order statistics
of eligible binary32 modality samples on a regular 2D/3D grid. Source data and
an optional full-source validity mask are borrowed and must remain stable for
the call. Returned values and validity are owned independently. No filesystem,
reader, UI, ROI or renderer is involved.

```go
opts := imagefilter.DefaultMaskedRankOptions() // rank .5
opts.RadiusMM = [3]float64{1.5, 2, 0}
opts.Neighborhood = imagefilter.PhysicalEllipsoid
opts.Boundary = imagefilter.ClipBoundary
result, err := imagefilter.MaskedRank(ctx, volume, inputValidity, opts)
```

## Rank, mask and validity

Rank is finite in `[0,1]`. For `n` eligible samples it selects sorted index
`floor(Rank*(n-1))`, where the binary64 product defines rounding. Rank zero
means minimum, even in zero-valued options; rank one means maximum. Rank .5
selects the lower median for an even count, without averaging. The convenience
default function explicitly sets .5. Ties retain their numerical value. Every
finite output except canonical positive zero is an eligible input value:
fractional/wide-range samples are never quantized.

A nil input mask means all samples are mask-eligible. Other masks have exactly
one entry per source sample. `RequireValidCenter` (default) requires an eligible
center as well as a nonempty neighborhood. `AnyValidNeighborhood` permits a
derived rank at a missing center if eligible neighbors exist; it does not turn
the original missing intensity into an observation. An empty window or refused
center produces finite `EmptyValue` and `Valid=false`. Empty masks successfully
produce all-invalid output, not apparent finite validity.

`RejectNonFinite` (default) rejects a mask-eligible NaN/Inf during a cancellable
source scan. Masked-out nonfinite payload is ignored. `ExcludeNonFinite`
excludes such samples and, under `RequireValidCenter`, invalidates a nonfinite
center. EmptyValue must be finite and defaults to zero.

## Physical kernels and borders

Layout is x-fastest. Axes with more than one sample need positive finite spacing
in millimetres. Singleton axes collapse to offset zero; their spacing is ignored.
Every radius, including collapsed axes, must be finite and nonnegative. This
API accepts regular materialized grids, not irregular slice coordinates.

`PhysicalBox` (default) includes integer offsets satisfying
`abs(offset[a]*spacing[a]) <= RadiusMM[a]` on active axes. `PhysicalEllipsoid`
includes offsets satisfying the binary64 sum of squared physical distances
normalized by each positive radius, `sum((offset*spacing/radius)^2) <= 1`.
Zero-radius axes require zero offset. Bounds are the greatest positive axial
offset accepted by that exact predicate, found by a quota-bounded monotone
search. A division-derived floor can omit a rounded multiplication endpoint:
binary64 `43*.1 == 4.3`, although `4.3/.1 < 43`. The search includes that
endpoint and rejects a quota endpoint that is still inside, rather than
shrinking the physical kernel. Membership has no epsilon, half-voxel inflation
or image-dimension radius cap.
The center is included. Binary64 multiplication/division and axis order are
part of the declared discrete membership.

`ClipBoundary` (default) discards outside offsets. `ReplicateBoundary` clamps
outside coordinates to the complete source border before checking mask/finite
eligibility. Repeated mapped samples keep multiplicity. Bounds and membership
are identical for both policies; requests are never silently shrunk to avoid
resource limits.

Existing `Median` still rounds half-sample radii up, caps its radius to the
image and replicates borders. Its implementation/results remain unchanged.
Equivalence is asserted only for rank .5, the same discrete box, no exclusions
and replicated borders. For example .5mm on a 1mm grid contains only the center
in the new physical kernel, but uses a three-sample axis in legacy Median.

## Regions, cancellation and ownership

`RankRegion` is a nonempty half-open output rectangle in full-source coordinates.
Its origin/size must fit the source without overflow. `MaskedRank` selects the
complete region. A regional result has cropped dimensions and reports its
source-coordinate region; the caller supplies physical origin/direction and
applies the region offset. Halos are read from the full materialized source.
Tile borders do not become source borders. Outputs are bit-identical across
worker counts, output tile decomposition and admitted exact execution paths.

Nil context means Background. Pre-cancelled requests reject before scans or
output/scratch allocation. Polls occur at at most 4096 bounded work operations
in admission/kernel enumeration, eligibility scan, row-cache setup, gathers,
histogram clear/update, partition and heapsort. Existing small selection and
two-level histogram lookup are reused only within intrinsically short work.
Workers join before any error returns nil result. No partial result is
published, source/mask is never mutated, and scratch is request-local.
This is not a streaming SliceReader/Writer or transactional external writer;
existing slab APIs and their committed-prefix behavior are untouched.

## Finite resources and exact paths

Zero RankLimits selects the following defaults; negative options and options
above finite ceilings are invalid. Exceeded quotas wrap `ErrRankResourceLimit`;
invalid headers/options wrap `ErrInvalidVolume`.

| Limit | Default | Ceiling |
|---|---:|---:|
| Full source voxels | 16 Mi | 16 Mi |
| Bounding box and retained kernel samples | 65536 | 65536 |
| Logical working bytes | 256 MiB | 1 GiB |
| Conservative sample visits | 1 Gi | 1 Ti |

Headers, source/mask lengths, kernel geometry, checked products and all quotas
are admitted before source scans or output/scratch allocation. Logical visit
admission charges `sourceN + boundingKernel + outputM*retainedKernel`. The
sliding histogram is admitted only when `K >= 2R`, so its first-window reads
plus two row-face reads per later output fit this sample-visit charge. Other
kernels use exact selection. Bin clearing reads no source payload. Selection
has a finite depth limit and cancellable heapsort fallback; worst-case work is
bounded by the finite voxel/kernel ceilings, not promised wall time.

Workers zero captures the process-wide default once, capped at 64 and output
rows. Explicit workers must be 1–64. Shared `parallel.Run` limits helpers across
concurrent callers. Only workers that claim a row allocate their scratch;
admission charges the entire captured possible bound. WorkerLimit reports that
bound, not actual helper use.

With `M` output voxels, `K` retained samples, `R` convex kernel rows and captured
worker bound `W`, the conservative logical capacity ledger is:

```
5*M + 12*R + W*(16*R + max(4*K, 4*(65536+256)) + 256) + 4096
```

Values/validity cost 5 bytes per output; exact spans use 12 bytes per row and
worker row caches at most 16 bytes per row. Selection capacity is exactly K
binary32 samples. The existing histogram pads fine bins to multiples of 256
and stores at most 65536 fine plus 256 coarse int32 counts. Only the selected
scratch path is allocated; no late switch retains both. Headers/closures and
scheduler/cancellation state have conservative ledger allowances. Capacities
are fixed with no append growth; source payload is not cloned. Caller memory,
allocator rounding, goroutine stacks, runtime/GC memory and process RSS are not
represented as exact measurements by this logical ledger.

Safe integral binary32 data spanning at most 65536 bins can use the existing
exact two-level histogram; masked sliding accounts for variable valid counts
and physical box/ellipsoid row spans. Arbitrary data uses exact gather and
selection. Up to 32 gathered samples reuse the existing bounded selector;
larger requests use independently authored cancellable introspective selection
with heapsort fallback. Both select existing sample values. Performance
promotion requires exactness and a separate coordinated measurement; no time
or memory reduction is inferred from the ledger.

## Qualification status and external scope

Analytic/direct-sort, path/worker/tile, ownership, finite-limit and cancellation
fixtures qualify the declared API. Source hashes for Median/slab/parallel are
preserved. The public corpus has 96 independently authored inputs sealed before
comparison: 72 native ITK clipped cases and 24 explicit replicated-padding
adapter cases, covering dimensions, box/ellipsoid, ranks 0/.5/1, masks and exact
binary fractions. The adapter pads both input and mask globally and reads the
corresponding interior output. It does not imply native ITK uses replicated
borders. Own nonfinite/AnyValidNeighborhood policies have analytic coverage,
not a claim about undefined or unsupported public ITK behavior.

The harness targets public ITK 5.4.5, commit
`f51594ad88194a72cdc06a314dafff709fcdc2e9`, with explicit kernel bitmap, rank,
mask values, finite fill, output mask and one work unit. Public getters/defaults
are recorded before setters and Update. The authorized campaign ran each of
the 96 cases once, without retries, and produced 12096 finite binary32 value
bits and validity entries. The Go corpus comparison matches every entry
exactly, with no tolerance or omitted cases. This qualifies the declared
clipped profile and explicit replicated-border adapter on these fixtures.
Raw-output, process, campaign, binary, SDK, harness and input provenance are
retained, and all sealed input objects remain unchanged. The fixed local cost
campaign below complements this correctness evidence; clinical qualification
is not established. See the
[fixture contract](../imagefilter/testdata/masked_rank/README.md).

## Fixed local cost measurement


The fixtures were 128×128×1 and 32×32×12, spacing (0.5, 1, 1.5) mm, physical
radius (1.5, 2, 1.5) mm, rank 0.5, mask `i%7 != 0` and one worker. Integer and
fractional data each covered physical box/ellipsoid and clipped/replicated
boundaries. Auto selects the eligible histogram or exact selection path;
the forced-selection baseline uses the same candidate with its exact scalar
path forced; direct sort is independently authored. Correctness was qualified
separately by the analytic and public-ITK corpus described above.

The predeclared histogram promotion criterion passed on these fixed fixtures:
the geometric mean Auto/selection median-time ratio across **all eight integer
profiles was 0.441896** (required ≤0.90),
and **eight of eight integer median times were faster** (required at least six).
Fractional data retained exact selection; its Auto/selection ratios ranged
from 1.003405 to 1.149546, an observed overhead of 0.34%–14.95%. This campaign
does not establish a fractional-data speedup.

Each strategy cell below reports **median ns/op [minimum–maximum]; B/op;
allocs/op** across its three repetitions. B/op and allocs/op are allocation
traffic, not retained memory or process RSS.

| Profile | Auto | Forced selection | Direct sort | Auto/selection |
|---|---:|---:|---:|---:|
| 2D/fractional/box/clip | 2185617 [2167400–2212983]; 83081; 18 | 2131250 [1984633–2209150]; 83081; 18 | 11148200 [10884242–11281300]; 4344936; 98256 | 1.025509 |
| 2D/fractional/box/replicate | 2419100 [2137033–2429858]; 83081; 18 | 2302475 [2283600–2387667]; 83081; 18 | 11251775 [11227475–11263750]; 4360040; 98339 | 1.050652 |
| 2D/fractional/ellipsoid/clip | 1104167 [1069908–1241942]; 83017; 18 | 1049800 [1038917–1068067]; 83017; 18 | 8053075 [7812858–8103575]; 3659816; 92901 | 1.051788 |
| 2D/fractional/ellipsoid/replicate | 1317867 [1300367–1362367]; 83017; 18 | 1300333 [1279708–1329583]; 83017; 18 | 8068592 [7795750–8118517]; 3724004; 93405 | 1.013484 |
| 2D/integer/box/clip | 691117 [689117–710950]; 84032; 20 | 2094833 [2026708–2270250]; 83081; 18 | 11155683 [11064533–11306325]; 4344932; 98256 | 0.329915 |
| 2D/integer/box/replicate | 752267 [730283–763317]; 84032; 20 | 2345517 [2266375–2385767]; 83081; 18 | 11611717 [11493475–11682233]; 4360040; 98339 | 0.320725 |
| 2D/integer/ellipsoid/clip | 746767 [721858–747642]; 84032; 20 | 1173192 [1152867–1197967]; 83017; 18 | 8046983 [7943175–8123692]; 3659812; 92901 | 0.636526 |
| 2D/integer/ellipsoid/replicate | 755217 [738575–784092]; 84032; 20 | 1236883 [1171025–1326808]; 83017; 18 | 8256433 [8125975–8338758]; 3724004; 93405 | 0.610581 |
| 3D/fractional/box/clip | 5898525 [5664192–5981667]; 63193; 18 | 5788092 [5747442–5810508]; 63193; 18 | 33338967 [33301975–33603983]; 9695267; 91526 | 1.019079 |
| 3D/fractional/box/replicate | 7563783 [7352058–7595675]; 63193; 18 | 7469225 [7405800–7470108]; 63193; 18 | 37417650 [37214608–37704592]; 11351843; 94791 | 1.012660 |
| 3D/fractional/ellipsoid/clip | 1010000 [987667–1087758]; 62617; 18 | 878608 [877600–1030058]; 62617; 18 | 17284375 [17169158–19087383]; 2930198; 71112 | 1.149546 |
| 3D/fractional/ellipsoid/replicate | 1171542 [1137825–1277183]; 62617; 18 | 1167567 [1054692–1245825]; 62617; 18 | 18031450 [17325842–18324100]; 3234582; 73503 | 1.003405 |
| 3D/integer/box/clip | 1427450 [1400933–1453317]; 63840; 20 | 6103442 [5974017–6113183]; 63193; 18 | 34087250 [34014342–34346742]; 9695260; 91526 | 0.233876 |
| 3D/integer/box/replicate | 1744217 [1735417–1801783]; 63840; 20 | 7710883 [7697258–7731017]; 63193; 18 | 38546133 [38438475–38640217]; 11351846; 94791 | 0.226202 |
| 3D/integer/ellipsoid/clip | 868575 [842275–943433]; 63616; 20 | 1011608 [946367–1051692]; 62617; 18 | 17900275 [17598467–18701400]; 2930198; 71112 | 0.858608 |
| 3D/integer/ellipsoid/replicate | 898867 [893300–973825]; 63616; 20 | 1154800 [1111875–1251092]; 62617; 18 | 17979033 [17335775–18577692]; 3234579; 73503 | 0.778375 |

The complete guarded phase took 10.391674 seconds under a fixed 90-second
deadline, including environment admission, a 3.149043-second build and a
7.132360-second benchmark process. All three owned process groups and census
children exited, were reaped and had empty groups; source/input hashes stayed
unchanged. Raw logs totalled 20,378 bytes under the 1 MiB cap. Sampled aggregate
group RSS was at most 340,525,056 bytes during build and 17,432,576 bytes during
the benchmark, below the 1 GiB guard. Sampling is diagnostic and does not
establish an exact peak or per-strategy memory saving.

A separate approximately 121 MiB archive transfer completed during the
measurement window. There were no concurrent Go builds or heavy GUI work,
but background network/file I/O means these are descriptive observations on
a shared host, not an isolated-host experiment. The disclosure caused no
retry, discarded result or threshold adjustment. The local criterion supports
the eligible histogram path on these fixtures; it does not establish
statistical significance, a universal improvement or clinical latency.
