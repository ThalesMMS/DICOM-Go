# Scalar slab row composition

The headless scalar library composes cached orthogonal slabs with the slab
sample outside the output-row pixel loop. Each pixel still receives exactly
the original increasing sample sequence. This changes traversal, while
preserving sample count and coordinates, interpolation, modality precision,
validity, min/max comparison order, Sum and Average arithmetic, and the
`SlabSampling` endpoint/spacing contract. The immutable output plane itself
holds each row's accumulation; no volume-sized scratch or new cache is used.

An Average with multiple samples retains one float64 denominator row per
admitted worker. Affine oblique thick slabs additionally retain a Vec3 row of
the original ray origins. Every point uses the original `Plane.At`, `voxelLine`
and `ray.at` arithmetic, without advancing pixels or samples by recurrence.
Scratch is allocated only when a granted worker claims output rows. It fits
inside the existing conservative sample-work reservation: eight extra bytes
per output pixel per additional slab sample. No caller limit is increased.

Non-affine/tilted or irregular-position oblique rays, slabs without enough
admitted scratch room, and separable cubic slab rays keep the original pixel
path. The separable cubic path retains its existing precomputed taps. The
geometry-free orthogonal fallback and legacy image-returning APIs are unchanged.
Existing source-lease and shared helper-budget ownership remain in force.

Cancellation is checked during row preparation, between samples and pixels,
and while finishing Average. Workers join before an error returns; cancellation
returns no partial plane. Results continue to own their scalar/validity arrays.


## Local warm-kernel measurements


Four repeats per side were collected in eight sequential processes ordered
baseline, candidate, candidate, baseline, baseline, candidate, candidate,
baseline. Each process selected all 14 frozen cases, with a 150 ms target per
case, CPU/GOMAXPROCS 2, one shared helper and allocation reporting. One helper
allows up to two total workers including the caller. Every initial N=1
admission, later calibration invocation, excluded warm-up and actual public
render was accounted for. All repeats remain in the results; no partial
campaign or best-repeat selection was used.

The table reports median ms/op and the complete four-repeat [min, max] range.
“Lower median” is relative to baseline, not a confidence interval. B/op and
allocs/op cover the complete public call; a fractional median averages the
two middle observations.

| Case | Baseline ms/op [min, max] | Candidate ms/op [min, max] | Lower median | B/op baseline → candidate | allocs/op baseline → candidate |
| --- | ---: | ---: | ---: | ---: | ---: |
| axial-mip | 1.711 [1.680, 1.737] | 1.218 [1.216, 1.221] | 28.85% | 148604 → 148862 | 13 → 16 |
| axial-min | 1.621 [1.611, 1.656] | 1.172 [1.167, 1.176] | 27.69% | 148498 → 148846 | 13 → 16 |
| axial-average | 1.571 [1.557, 1.606] | 1.195 [1.194, 1.198] | 23.94% | 148504 → 150891 | 13 → 18 |
| coronal-mip | 1.653 [1.609, 1.666] | 1.324 [1.314, 1.329] | 19.91% | 148578 → 148900 | 13 → 16 |
| coronal-min | 1.414 [1.408, 1.440] | 1.191 [1.190, 1.192] | 15.75% | 148562 → 148832 | 13 → 16 |
| coronal-average | 1.407 [1.404, 1.626] | 1.234 [1.229, 1.235] | 12.25% | 148512 → 150880 | 13 → 18 |
| sagittal-mip | 1.641 [1.634, 1.686] | 1.315 [1.313, 1.317] | 19.90% | 148512 → 148832 | 13 → 16 |
| sagittal-min | 1.422 [1.417, 1.426] | 1.149 [1.147, 1.151] | 19.18% | 148512 → 148832 | 13 → 16 |
| sagittal-average | 1.414 [1.408, 1.416] | 1.197 [1.194, 1.205] | 15.38% | 148512 → 150880 | 13 → 18 |
| oblique-mip | 7.322 [7.301, 7.469] | 5.522 [5.354, 5.629] | 24.58% | 112120 → 118832 | 15 → 18 |
| oblique-min | 7.876 [7.823, 7.929] | 5.843 [5.762, 6.006] | 25.81% | 112120 → 118832 | 15 → 18 |
| oblique-average | 7.297 [7.211, 7.436] | 5.595 [5.538, 5.636] | 23.32% | 112120 → 120880 | 15 → 20 |
| axial-cubic-fine-average | 13.999 [13.947, 14.021] | 12.481 [12.356, 12.526] | 10.85% | 148496 → 150880 | 13 → 18 |
| oblique-cubic-fine-average | 81.150 [80.856, 81.672] | 79.046 [78.872, 87.625] | 2.59% | 113020 → 120884 | 17 → 20 |

The twelve linear thick cases have 12.25–28.85% lower medians and separated
observed ranges in this campaign. Axial cubic/fine Average has a 10.85% lower
median with separated ranges. Oblique cubic/fine Average has a 2.59% lower
median but overlapping ranges: its 87.625 ms candidate repeat exceeds the
baseline's 80.856–81.672 ms range, while the other candidate repeats are
78.872–79.141 ms. That control does not support a robust improvement claim.
A baseline coronal-average repeat of 1.626 ms also lies above its other
1.404–1.409 ms repeats. Both variations are preserved without discarding runs.
Four repeats in one local synthetic campaign do not establish a universal
workload speedup.

B/op and allocs/op increase in every case. The traversal gain trades additional
admitted row state and optional scratch buffers for lower observed linear
kernel time; this is not an allocation or process RSS reduction.

The 30° `obliqueInteriorPlane` cubic/fine fixture is nonseparable and eligible
for row composition. It is distinct from separable cubic rays, which retain
the original precomputed-tap pixel path. This corrects the broader cubic
classification in the original campaign plan without changing its sealed
measurement inputs.

## Observed row-buffer payload and owner lifetime

A separate external test-only lane ran 18 controls per baseline/candidate
variant under the race detector. It recorded cap immediately after the actual
weights/origins allocations, attached each buffer to the actually granted
worker, and replayed buffer creation/release and owner termination through
join. The observed unit is backing-capacity payload during the worker-owner
lifetime. It excludes allocator size-class rounding, GC reclamation, process
RSS and the original fallback cubic-plan storage. Observer metadata does not
retain the owning slices. Instrumented binaries were never used for timing
comparisons.

All observed arrays have capacity 128: weights use 8 bytes per element and
origins 24. Natural single-request controls observed the following peaks:

| Path | Peak new row-buffer payload |
| --- | ---: |
| Orthogonal MIP/MinIP | 0 B |
| Orthogonal Average, including fine cubic | 2,048 B |
| Eligible oblique MIP/MinIP | 6,144 B |
| Eligible oblique Average, including the 30° fine cubic fixture | 8,192 B |

A post-factory overlap control observed 8,192 B in two actually granted
workers. Cancellation after both factories allocated, before row work,
observed the same peak and then returned no plane with `context.Canceled`.
All buffer/owner metadata, helpers and source leases were zero after join.
Two concurrent requests observed 12,288 B globally in three actually granted
workers under the one shared-helper budget: one request retained 4,096 B and
the other 8,192 B. The control did not assume four desired workers.
The test-only factory observer and overlap barrier affect scheduling and
serve capacity/lifetime accounting only.

Baseline and an additional aligned/separable cubic fallback control observed
0 B of these new row buffers. This does not mean their total scratch is zero;
retained original cubic-plan storage is outside this new-buffer metric.
The observed payload stayed inside the existing admission without increasing
limits. No observer, production lock, cache, caller change or CPU 3D fallback
was added to the runtime implementation.

## Qualification and evidence seals

The complete campaign required all 14 unique cases in each of the eight
processes, valid calibration accounting, unchanged source/toolchain/helper/
overlay/binary hashes, and reaped children with empty process groups. The
separate capacity lane required all 18 controls per variant, exact event
balance and no live buffers/owners/helpers/leases after join. Both authorized
runs passed once, with no retry. Guard RSS observations are resource-cap
evidence only, not an RSS comparison.

Complete public equivalence covers 7,560 scalar/window frames, including
value bits, validity, dimensions and grayscale output. Baseline and candidate
frame artifacts have the same SHA256:
`a35b24c8e04bd64af3f2572b04a162e2754749d0c6cf3789480b372d6b346758`.
Proportional native, race, Go 1.22.12 and vet checks passed. These local
fixtures and checks qualify the library behavior and scoped measurements;
they do not establish clinical, PACS, native Windows/GPU or application
latency acceptance.

The retained task evidence has the following SHA256 seals:

| Evidence | SHA256 |
| --- | --- |
| Uninstrumented campaign result, all cases/repeats/accounting | `5cc522a5da849ae6560d76d3064a9e545302d58ce6543e4d5458c643507fc37a` |
| Campaign complete input seal, equal before/after | `7ea2ae8b376105c0d62ad7a6034af13c24b71572d42ae4462720a682a6a7f651` |
| Uninstrumented baseline binary, equal before/after | `dc6dfcbcec86ba1a6def325e08445fea8789de4cfda4f605e21999d1a4006efd` |
| Uninstrumented candidate binary, equal before/after | `28eecf4ddd73a7a3e908a84768ca5c85727dc4939a63b0899bad02e4d6c21953` |
| Capacity/lifetime lane result | `8e3298df20a30b28fbab57964684ee5fff2d1b70cbe782141bdbfca7d31e62d6` |
| Capacity/lifetime lane input seal | `65b239bec8ef4cf0b23e0b5d8e82c4f15a59f8d88f78807c65a8dd4499e3a1f9` |
| Instrumented baseline binary, equal before/after | `bd2cfad4c280f126141e403358da37c13063999c44c4cf56bf5ed444c63e12fe` |
| Instrumented candidate binary, equal before/after | `6aeeca685d87bba70eed1aa4d2058884a64cce10450c11a11b879c2f3e184a11` |

The original plans, raw per-process logs and accounting, capacity events and
full reports remain with the task artifacts. Documentation integration does
not rewrite the measured plans, frozen runtime sources or timing binaries.
