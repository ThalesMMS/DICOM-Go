# Connected grayscale area and volume morphology

`roi.GrayscaleAttributeOpen` and `roi.GrayscaleAttributeClose` consume an
`imagefilter.Volume` and return a new volume with unchanged dimensions and
spacing. Samples are finite float32 modality values. There is no UI, DICOM
encoding, intensity quantization, CPU/GPU rendering policy change or clinical
qualification associated with these filters.

Opening retains connected upper-level-set components of size at least the
threshold, lowering smaller bright peaks to their first retained ancestor.
Closing uses lower level sets, raising small dark valleys. The global root is
retained: an excessive threshold makes opening the global minimum and closing
the global maximum. A zero threshold produces an independent identity copy
(after validation and resource admission). Equality at the threshold retains a
component; fractional sample thresholds are allowed. Connected components at
image borders are treated exactly like interior ones, with no outside padding.

These are shape-independent component-tree operators. They differ from
`GrayscaleOpen`/`GrayscaleClose`, which use a ball structuring element in physical
millimetres, and from removing small components of one binary mask: grayscale
components nest across intensity levels. Plateaus are resolved as complete
connected level sets, not independently discarded pixels. The implementation
is an independent upper/lower component-tree sweep with union-find and
cancellable stable merge sorting; it is not a transcription of an external
library. Its mathematical contract is the increasing-attribute level-set
construction described by [Breen and Jones, 1996](https://people.cmm.minesparis.psl.eu/users/marcoteg/cv/publi_pdf/MM_refs/vincent/Breen_96.pdf).

## Topology and units

`GrayscaleAttributeOptions.Dimension` defaults to 2D for `Dims[2]==1`, otherwise
3D. Explicit `Attribute2D` requires a single slice. Explicit `Attribute3D` also
accepts a single-slice thin volume: it has 3D sample volume and 3D neighborhood
rules, with nonexistent adjacent slices ignored. Singleton axes do not create
periodic neighbors. Connectivity is 4/8 in 2D and 6/18/26 in 3D; zero selects
4/6 respectively. Explicit mismatched dimension/connectivity is rejected.

`AttributeSamples` counts samples. `AttributeSquareMM` requires 2D and measures
count × x-spacing × y-spacing. `AttributeCubicMM` requires 3D and measures count
× x-spacing × y-spacing × z-spacing, including a one-slice volume. Physical
measurement requires positive finite spacing on every measured axis even when
that axis has one sample. Sample-count mode follows `imagefilter.Volume`'s
spacing contract for axes with more than one sample. Physical sample measures
that overflow or underflow to zero are rejected. A 2D image's unused z-spacing
is copied unchanged. Anisotropy changes size measurement, not graph edges.
The caller owns origin/direction; irregular grids must be regularized first.

Intensities remain their original finite float32 values, including extreme
positive/negative values; closing orders lower levels directly without negating
samples. NaN and either infinity are rejected, not omitted as holes or replaced.
No output is returned for empty/invalid input, invalid options, exhausted
resources or cancellation. Input storage is never modified or shared with the
output. Numeric equality treats +0/-0 as the same plateau; the identity path
preserves bits, while pruned equal-zero nodes may inherit an ancestor's zero
sign. Algebraic properties use numeric equality rather than zero-sign identity.

## Resource and cancellation scope

Finite defaults are 64M samples and 1GiB algorithm-array/workspace storage.
`MaxVoxels`/`MaxBytes` accept smaller or larger positive caller budgets, while
indices always restrict total samples to int32 range. Negative limits are
invalid. Admission reserves 24 bytes per sample plus1024 bytes of fixed
workspace, including output, order and merge scratch, union-find, tree parent
and counts. This conservative reservation also applies to zero threshold.
Caller input and Go allocator/runtime/RSS overhead are outside `MaxBytes`;
it is an algorithm-storage budget, not an OS memory limit. No recursive flood,
per-component allocation or independent-tile connectivity shortcut is used.
Sorting takes O(n log n) comparisons; the graph has at most26neighbors/sample,
and the component structure uses O(n) storage. Sorting, input scans, tree sweep,
long root traversals and reconstruction check context cancellation. Cancellation
returns nil output; geometry and input samples remain unchanged.

## Measured synthetic validation and current limits

On macOS 27.0.1, darwin/arm64, Go 1.27.1, the three connected-grayscale tests
and three affected ball-morphology tests passed (six top-level tests, one
package, zero skips or failures). Exact sample comparisons against an
independent threshold-at-each-intensity BFS oracle cover 2D/3D connectivity,
anti/extensivity, idempotence and duality. Additional fixtures cover plateaus,
boundaries, diagonal contacts, physical units, thin 3D, native floating values,
nonfinite rejection, resource admission and cancellation. Input and geometry
preservation are checked. The existing ball-filter contracts passed unchanged.

The following commands actually ran from the library module with
`GOMAXPROCS=2`, `GOMEMLIMIT=1GiB`, `GOOS=darwin`, `GOARCH=arm64` and the local
Go 1.27.1 toolchain:

```sh
go test -json -count=1 -p=1 -timeout=90s ./roi -run '^(TestGrayscaleAttributeIndependentLevelsAndAlgebra|TestGrayscaleAttributePlateausDiagonalsBordersAndPhysicalSize|TestGrayscaleAttributeLimitsValidationAndCancellation|TestBallErosionAndDilationMatchDefinition|TestTopHatsExtractDetailsNarrowerThanTheBall|TestBallFilterRejectsInvalidRequests)$'
go test -json -count=1 -p=1 -timeout=90s ./roi -run '^$' -bench '^BenchmarkGrayscaleAttribute$' -benchmem -benchtime=1x
```

All 16 benchmark cases passed. Each row below is one measured iteration, with
threshold 12 samples and spacing [0.7, 1.3, 2.5] mm. The small 2D image is
64×64×1 with connectivity 8; the small 3D volume is 24×24×12 with connectivity
26. Both have 16 intensity levels. Before timing each small opening/closing
pair, the benchmark asserts exact sample equality between the component tree
and independent threshold-BFS oracle. The larger 2D image is 256×256×1 with
connectivity 8; the larger 3D volume is 64×64×32 with connectivity 26. Their
32-level and deterministic native-float32 cases measure the component tree
without a large-volume oracle comparison.

| Shape / intensities | Operation / implementation | ns/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| 64×64×1 / 16 levels | Open/Tree | 405833 | 98384 | 7 |
| 64×64×1 / 16 levels | Open/ThresholdBFS | 1568000 | 1015928 | 605 |
| 64×64×1 / 16 levels | Close/Tree | 307084 | 98384 | 7 |
| 64×64×1 / 16 levels | Close/ThresholdBFS | 1434208 | 1012664 | 559 |
| 24×24×12 / 16 levels | Open/Tree | 1072041 | 172112 | 7 |
| 24×24×12 / 16 levels | Open/ThresholdBFS | 4431250 | 1896120 | 382 |
| 24×24×12 / 16 levels | Close/Tree | 993125 | 172112 | 7 |
| 24×24×12 / 16 levels | Close/ThresholdBFS | 4099625 | 1895864 | 377 |
| 256×256×1 / 32 levels | Open/Tree | 5249208 | 1572944 | 7 |
| 256×256×1 / 32 levels | Close/Tree | 5340250 | 1572944 | 7 |
| 256×256×1 / native float32 | Open/Tree | 6491417 | 1572944 | 7 |
| 256×256×1 / native float32 | Close/Tree | 6041500 | 1572944 | 7 |
| 64×64×32 / 32 levels | Open/Tree | 22010041 | 3145808 | 7 |
| 64×64×32 / 32 levels | Close/Tree | 20841167 | 3145808 | 7 |
| 64×64×32 / native float32 | Open/Tree | 23904542 | 3145920 | 8 |
| 64×64×32 / native float32 | Close/Tree | 24401625 | 3145808 | 7 |

`B/op` is cumulative Go heap allocation per operation, not peak RSS or the
algorithm's maximum live workspace. No peak-RSS measurement, statistical
speedup, external-library parity or clinical-stack performance is claimed.
These single-iteration synthetic measurements establish the recorded local
time/allocation observations and small-fixture equivalence only. They do not
qualify clinical data or large-volume oracle equivalence. Full module baseline
and race checks were not part of these focused commands; integration validation
is recorded separately. Binary-component and ball-structuring-element APIs
remain unchanged.
