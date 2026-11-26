# Recursive Gaussian memory and ownership


`render.PlanVolumeVesselEnhancement` adds the display enhancement's physical
scales, anisotropic Hessian coefficients, singleton-axis rules, slab margin,
Frangi weights and ramp. It takes a scalar volume descriptor only; computing
the modality minimum and evaluating the multiscale response belong to the
executing backend. Its output is metadata for a display derivation, not a
filtered or quantitatively measurable volume.

`imagefilter.PlanVesselnessSlabSlices` exposes the seven-buffer mathematical
slab division used by the library. The vessel enhancement planner includes
this division in each scale. Device storage limits may require smaller output
cores, but both recursive directions must still use the parent mathematical
slab extended by its six-sigma margin. Using device texture boundaries as
recursion endpoints changes the response at partition seams.

The Gaussian APIs preserve the source and return independently owned float32
volumes. The request scheduler filters z, then y, then x and shares matching
z/y passes between components. A private intermediate can supply its backing
to its final consumer. The caller invalidates that previous owner immediately;
borrowed source buffers, intermediates needed by another component and emitted
outputs cannot be consumed. Identity passes transfer ownership under the same
rule. An identity of the borrowed source is copied before emission.

The full-line kernel supports exactly the same source/destination slice and
indices. The causal recurrence finishes before writing outputs. During the
anticausal recurrence each lane retains the next original sample in a float64
latch before storing the current output. This preserves the original
`x[j+1]` operand, including the final sample. Overlapping slices with different
indices are unsupported. Compact output windows retain their non-aliasing
contract.

Poles, coefficients, normalization, axis order, float64 recurrences and causal
scratch are unchanged. Each axis still rounds its stored samples to float32.
There is no float32 scratch variant and no public opt-in mutation API.

Worker scratch belongs to one request and is reused after each pass joins.
Each active worker grows its causal allocation only to the lane group needed
by the current pass, retaining it for later passes. Its maximum is
`max(axis length × min(32, available lanes))` float64 values across the axes
that worker actually processes. A long axis with one group does not allocate
its large scratch for workers used only on shorter axes. A y group cannot
cross a slice. State contains 128 float64 values per worker; the local
latch contains at most 32 values. Requested workers, available groups and the
shared helper budget bound scratch slots, and only workers that claim work
allocate their sample/state arrays. The request holds no helper reservation
between passes. No global scratch or volume pool is added.

Cancellation stops claims between groups and joins every started helper before
returning. A partially written private destination does not escape. The public
APIs return an error and no output; the borrowed source stays byte-identical.
Scratch and volume buffers become unreachable when the request ends.

## Backing inventory

For a 3-D volume with all three axes filtered and no identically zero
components, the scheduler's number of distinct private volume backings is:

| API | Previous scheduler | Reuse scheduler | Including source, previous → reuse |
| --- | ---: | ---: | ---: |
| Smoothing / one derivative | 2 | 1 | 3 → 2 |
| Gradient, three outputs | 4 | 3 | 5 → 4 |
| Gradient magnitude | 3 | 2 | 4 → 3 |
| Hessian, six outputs | 7 | 6 | 8 → 7 |
| Laplacian | 3 | 2 | 4 → 3 |

The previous smoothing scheduler already needed only two private backings,
not three. One 256³ float32 backing is 64 MiB, so the smoothing backing
inventory including its source changes from 192 to 128 MiB. That calculation
excludes runtime, scratch, stacks, metadata and allocator effects; it is not
an RSS result. Zero sigma, singleton axes, zero derivatives and different
requested components can change the inventory. Emitted Hessian/gradient
components remain independent. Reducers retain an accumulator and return
their other emitted component buffers to the request pool.

The reuse test observes every emitted backing and the pool after the scheduler
finishes, while keeping outputs alive. It checks the table against the frozen
previous scheduler. No production counter or allocation hook is required.

## Numerical and lifetime checks

`recursive_gaussian_reuse_test.go` keeps the previous scheduler and line
arithmetic from `d5b3e3796a390fc4b6f02afac6d77e28d94ac679` for bit-exact
comparisons across all affected public APIs and their reduction order.
Independent line convolution explicitly sums fitted kernel terms and the
geometric infinite tails of replicated faces. Its float32 comparison tolerance
is `3e-6 × max(1, abs(reference))`; this tests recurrence execution and is not
an ITK accuracy tolerance. Existing sampled-Gaussian, polynomial, anisotropic
blob and ITK fixture tolerances remain unchanged.

Checks cover derivative orders 0/1/2, small and large sigma, scale
normalization, anisotropic grids, 1/31/32/33-lane boundaries, compact windows,
singleton axes, absent kernels, source bytes, independent outputs, ownership
transfers, request-local scratch, shared helper limits, cancellation during
work and simultaneous requests with both shared and distinct immutable inputs.

## Reproducible measurement

`recursive_gaussian_reuse_bench_test.go` depends only on public APIs and is
copied identically into the isolated baseline and candidate. Baseline starts
at the frozen HEAD above. Candidate overlays only this change's four paths.
Other owners' working changes are excluded. The bench fixture is 96×80×48,
spacing 0.7/1.3/2.1 mm, sigma 1 or 8 mm and a deterministic trigonometric
signal. Input preparation and source/output SHA-256 checks are outside the
timed repeated API calls. Every component participates in the output hash.
The timed request includes source validation, all passes, output allocation
and reductions. Report ns/op, B/op and allocs/op with runtime, GOMAXPROCS and
the helper budget; alternate baseline/candidate runs in an exclusive window.

For whole-process RSS, invoke an already compiled test binary with
`-test.run=^TestRecursiveGaussianReuseProcess$ -test.v` under
`/usr/bin/time -l`. Set `DICOM_GAUSSIAN_PROCESS_API` to `smooth`, `gradient`,
`magnitude`, `hessian` or `laplacian`. Defaults are a 256³ input, sigma 1 mm
and the shared worker budget. Optional `DICOM_GAUSSIAN_PROCESS_DIM`,
`DICOM_GAUSSIAN_PROCESS_SIGMA` and `DICOM_GAUSSIAN_PROCESS_WORKERS` freeze
other cells. Set GOMAXPROCS identically for both binaries. Each process creates
one input, runs one API and hashes the input and all outputs. RSS and wall
time therefore include runtime, preparation, hashes and the complete request.
The logged TotalAlloc/Mallocs delta spans only the API call. Allocation
profiles and logical backing counts are separate evidence from kernel time
and process RSS.


Vesselness has its own seven-buffer slab scheduler and persistent worker
scratch; it does not call this request scheduler. Its separate benchmark is a
regression cell for the shared line kernel and does not establish a reduction
in Vesselness buffers. Registration pyramid smoothing consumes the improved
Gaussian API without changing its registration workflow. These checks do not
establish native Windows, PACS, viewer or clinical validation.


The frozen baseline above and the four-path candidate were compiled with
Go 1.22.12, external linking, on macOS 27.0.1 (26A434), arm64 Mac16,11,
12 logical cores and 24 GiB RAM. Focused Gaussian/ITK/Vesselness/pyramid tests,
reuse race tests, helper/cancellation checks and vet passed. The same Gaussian
and ITK fixtures passed in both isolated binaries. An independent review
identified excessive eager scratch for elongated volumes; lazy growth per
active worker/pass corrected it, and a 65537×3×3 fixture checks that only the
single long-axis worker obtains the large scratch. No remaining review
blockers were found.

The 96×80×48 kernel campaign used five alternating pairs per sigma/worker
cell and `-test.benchtime=100ms`. The following sigma-1 medians are
representative; sigma 8 was also measured with both worker budgets. Times
describe these short runs and do not establish statistical significance.

| API | GOMAXPROCS | ms/op, previous → reuse | B/op, previous → reuse | allocs/op, previous → reuse |
| --- | ---: | ---: | ---: | ---: |
| Smoothing | 1 | 2.823 → 2.840 | 3,011,408 → 1,534,816 | 28 → 26 |
| Smoothing | 3 | 1.183 → 1.228 | 3,133,298 → 1,653,027 | 55 → 53 |
| Gradient | 1 | 7.072 → 7.308 | 6,070,584 → 4,486,393 | 65 → 53 |
| Gradient | 3 | 2.816 → 2.910 | 6,408,868 → 4,605,896 | 137 → 105 |
| Gradient magnitude | 1 | 7.523 → 7.781 | 4,596,546 → 3,012,329 | 84 → 72 |
| Gradient magnitude | 3 | 3.022 → 3.055 | 4,935,905 → 3,131,998 | 175 → 143 |
| Hessian | 1 | 12.914 → 13.534 | 10,652,316 → 8,913,388 | 117 → 91 |
| Hessian | 3 | 5.137 → 5.212 | 11,302,412 → 9,035,526 | 252 → 178 |
| Laplacian | 1 | 7.246 → 7.493 | 4,596,258 → 3,012,040 | 74 → 62 |
| Laplacian | 3 | 2.938 → 2.928 | 4,935,693 → 3,131,965 | 156 → 124 |
| Vesselness, separate scheduler | 1 | 34.455 → 35.202 | 5,642,754 → 5,642,754 | 267 → 267 |
| Vesselness, separate scheduler | 3 | 13.836 → 14.295 | 5,699,886 → 5,699,864 | 510 → 511 |

Across sigma 1/8, Gaussian median times varied from −0.3% to +4.8% and
Vesselness from +2.1% to +3.3%. This change delivers lower memory use with
numerical preservation; it does not claim a speedup. Vesselness allocation
variation at GOMAXPROCS 3 is small worker bookkeeping noise.

Whole-process measurements used one already compiled process per variant/cell,
a 256³ input and `/usr/bin/time -l`. RSS below includes the input, output,
runtime, package initialization, test framework and source/output hashing.
Nothing was subtracted to approach the original target.

| API/configuration | GOMAXPROCS | Maximum RSS MiB, previous → reuse | Process real seconds, previous → reuse |
| --- | ---: | ---: | ---: |
| Smoothing, sigma 1 | 1 | 213.20 → 149.83 | 0.56 → 0.55 |
| Gradient, sigma 1 | 1 | 341.47 → 277.30 | 0.96 → 0.93 |
| Gradient magnitude, sigma 1 | 1 | 277.53 → 213.59 | 0.80 → 0.81 |
| Hessian, sigma 1 | 1 | 533.72 → 469.58 | 1.48 → 1.49 |
| Laplacian, sigma 1 | 1 | 277.25 → 214.02 | 0.78 → 0.80 |
| Smoothing, sigma 8 | 3 | 213.91 → 149.27 | 0.47 → 0.47 |

The approximately 130 MiB process target was **not reached** in this fixture
and test-binary environment. Smoothing nevertheless removed one 64 MiB
private backing and reduced measured RSS by about 63–65 MiB. The 128 MiB
source-inclusive backing inventory is not a process RSS claim.

Separate allocation profiles used `-test.memprofilerate=1`; they were not
collected during the kernel or RSS cells. At 256³, `bufferPool.get` accounts
for 128 → 64 MiB in smoothing, 256 → 192 MiB in gradient and 448 → 384 MiB
in Hessian. Smoothing allocates exactly two → one private backings; its
API TotalAlloc delta is 134,419,296 → 67,177,312 bytes. A separate 96³
Vesselness profile shows the same 4,154,416 bytes at `vesselPlan.allocate`
and the same API delta of 7,718,808 bytes in both variants. Whole-profile
package initialization varies between processes and is not attributed to
these API deltas.

