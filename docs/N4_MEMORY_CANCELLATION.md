# N4 memory and cancellation

`imagefilter.N4BiasFieldCorrection` retains both public float32 outputs:
`Corrected` and `LogBiasField`, with the input dimensions and spacing. The
input and optional mask remain caller owned and unchanged. The corrected
value uses the unrounded float64 log field before either output is rounded
to float32. The histogram, control lattice, fitting levels and convergence
mathematics retain their existing definitions; this does not add clinical
qualification or expand the N4 algorithm's existing scope.

Spline evaluation collapses z, then y, then x. At occupancy of at most 1/16
of the estimation grid, it evaluates the selected mask samples directly
using 64 lattice contributions and tiny local plane/line arrays. For denser
masks, it reuses complete collapsed planes and lines, emitting only selected
samples. The crossover is a fixed cost heuristic; it is not a measured
claim that every sparse case is faster. Both paths avoid materializing a
full float64 estimation field just to select the mask samples. The final
reconstruction emits directly into the two required float32 outputs instead
of holding an additional full float64 field.

The mandatory output payload is 8 bytes per input voxel. This excludes the
input float32 payload, mask, allocator overhead and runtime memory. The
block-averaged estimation grid still uses full-grid sum/count buffers during
shrinking. Its selected samples and iteration vectors, lattice fitting
accumulators (at most 16 deterministic blocks), per-axis positions/weights,
and per-worker collapsed lattice planes/lines remain scratch costs. Thus
this change does not make the complete N4 procedure streaming or admit an
unbounded volume under a fixed memory quota. Public input dimensions and
options retain their existing admission policy.

The caller context reaches validation, shrinking, axis/weight preparation,
histogram sharpening and FFTs, fitting, lattice updates/refinement,
convergence, selected evaluation and final reconstruction. Long scalar loops
poll at intervals of at most 1024 elements; fitting polls after at most
1024 samples, and sparse evaluation polls once per selected sample. Dense
evaluation also polls at each output row. Polling intervals bound algorithmic
work between checks, not a wall-clock cancellation deadline: allocations,
runtime scheduling and caller progress callbacks can delay return.
Parallel kernels join their workers before returning an error. A canceled
public operation returns a nil result and its context error; partially
written private arrays are discarded. Callers must not concurrently mutate
the input/mask, and must arrange publication/admission of completed outputs.
Twin owns its job lifecycle and application memory admission.

## Numerical and cost evidence

The independent polynomial B-spline oracle prescribes a 1e-12 scaled absolute
bound and includes endpoints, nonuniform parametric positions and thin axes.
The frozen original N4 regression cases require exact public values, metadata
and convergence on sparse/dense masks, anisotropic spacing and thin grids.
The committed ITK fixture retains its existing worst log-field bound of
1e-4; it is not an ITK runtime rerun or whole clinical qualification.

The affected-package gates passed 24 top-level tests with no skips, including
both original/candidate numerical cases and the committed ITK fixture, exact
original-output regressions, internal/public cancellation, FFT and DSA callers.
The fixed comparative campaign executed once: six cases, three independent
process runs per implementation (36 cost runs), then three public cancellation
runs per implementation (six runs). All cost input/mask/output hashes,
geometry, selected samples and convergence levels matched between original
and candidate and across repetitions. These are synthetic host observations,
not clinical qualification or a statistical claim of universal improvement.

The original is commit `08fc8b422b6b0a66f4b7678480788066fd9199dc`;
the tested candidate's implementation and numerical tests were frozen before
measurement. The primary checkout admission was `9a48fe73e1c59c680bb9245da23e230564552cca`.
Runtime: Go 1.27.1, darwin/arm64, GOMAXPROCS=2, GOMEMLIMIT=1GiB,
Workers=2. Both implementations used identical prepared inputs/measurement
oracles: values `100+i%17`, spacing 0.7/1.8/4.2 mm, dense or 1/64 mask,
ShrinkFactor=1 and Iterations=[3,3]. Each cost run used benchtime=1x;
setup and post-timer hashes were excluded from time/allocations but included
in process RSS. Reported iterations retain actual convergence behavior rather
than assuming all requested iterations execute. Order alternated by repetition;
there was no filtering, retuning or repeated campaign.

Cells show median [minimum, maximum] over three runs. Time is milliseconds
per complete N4 call/volume (ns/op converted to ms), not per DICOM frame;
B/op is cumulative allocation bytes; allocations/op is an allocation count;
native RSS is bytes from `/usr/bin/time -l` for the whole measured Go process.

| Grid / mask | Implementation | Time ms | B/op | Allocations/op | Native peak RSS bytes |
| --- | --- | ---: | ---: | ---: | ---: |
| 64x48x32 / dense | baseline | 11.383209 [11.135041, 11.390917] | 18608600 [18608248, 18608616] | 261 [261, 261] | 20348928 [20332544, 20512768] |
| 64x48x32 / dense | candidate | 12.094459 [12.000500, 12.165792] | 16255024 [16254912, 16255392] | 422 [421, 422] | 19677184 [19136512, 19693568] |
| 64x48x32 / 1/64 | baseline | 1.606125 [1.587541, 1.606416] | 5018344 [5017864, 5018344] | 167 [166, 167] | 13942784 [13860864, 13975552] |
| 64x48x32 / 1/64 | candidate | 1.566417 [1.489792, 1.576250] | 2658352 [2658240, 2658352] | 187 [186, 187] | 13221888 [13025280, 13221888] |
| 128x96x1 / dense | baseline | 1.842916 [1.796042, 1.846000] | 2148752 [2148752, 2148752] | 172 [172, 172] | 12058624 [12009472, 12091392] |
| 128x96x1 / dense | candidate | 1.944375 [1.925500, 1.952166] | 1854224 [1854224, 1854224] | 177 [177, 177] | 11763712 [11730944, 11796480] |
| 128x96x1 / 1/64 | baseline | 0.340375 [0.318709, 0.343375] | 757408 [757408, 757408] | 138 [138, 138] | 10502144 [10240000, 10502144] |
| 128x96x1 / 1/64 | candidate | 0.339625 [0.337083, 0.342709] | 463216 [463216, 463216] | 140 [140, 140] | 10240000 [10174464, 10420224] |
| 128x96x3 / dense | baseline | 4.753791 [4.730000, 4.798208] | 7034824 [7034328, 7035288] | 223 [222, 224] | 14565376 [14532608, 14958592] |
| 128x96x3 / dense | candidate | 5.080417 [5.025000, 5.082792] | 6150440 [6149960, 6151272] | 238 [237, 239] | 14860288 [14303232, 15007744] |
| 128x96x3 / 1/64 | baseline | 0.695375 [0.679708, 0.713625] | 1985128 [1985128, 1985128] | 160 [160, 160] | 12025856 [11976704, 12107776] |
| 128x96x3 / 1/64 | candidate | 0.649166 [0.639209, 0.659250] | 1099000 [1099000, 1099480] | 151 [151, 152] | 11075584 [10977280, 11108352] |

Cumulative allocation fell in all six pairs, but the count of allocations rose
in five pairs. Dense median operation time increased by 6.25%, 5.51% and
6.87% for the three grids respectively. Sparse median times decreased by
2.47%, 0.22% and 6.65%; overlapping ranges and three runs do not establish
statistical significance. Native peak RSS decreased in five pairs; dense
128x96x3 increased from 14,565,376 to 14,860,288 bytes (+2.02%).
There is no universal speedup or peak-memory reduction claim.

Both returned float32 volumes require 8N bytes (4N each). The following
B/op-minus-8N values are cumulative allocation remainder, not simultaneous
scratch, retained memory, or peak RSS. Output allocator/runtime overhead,
iteration temporaries and other allocations remain in that remainder.

| Grid / mask | Mandatory two-output bytes | Baseline cumulative remainder bytes | Candidate cumulative remainder bytes |
| --- | ---: | ---: | ---: |
| 64x48x32 / dense | 786432 | 17822168 [17821816, 17822184] | 15468592 [15468480, 15468960] |
| 64x48x32 / 1/64 | 786432 | 4231912 [4231432, 4231912] | 1871920 [1871808, 1871920] |
| 128x96x1 / dense | 98304 | 2050448 [2050448, 2050448] | 1755920 [1755920, 1755920] |
| 128x96x1 / 1/64 | 98304 | 659104 [659104, 659104] | 364912 [364912, 364912] |
| 128x96x3 / dense | 294912 | 6739912 [6739416, 6740376] | 5855528 [5855048, 5856360] |
| 128x96x3 / 1/64 | 294912 | 1690216 [1690216, 1690216] | 804088 [804088, 804568] |

Process supervision also sampled group RSS every 50ms, including time/env
parents. These very short operations can complete between samples; that
sampler is not the native Go peak and cannot isolate scratch or guarantee
that no sub-sample peak crossed a quota. The 1GiB sampled-group stop limit
and 1MiB raw-output cap are instrumentation limits, not an N4 admission
budget. All native observed Go peaks were below 1GiB. End-to-end process
elapsed time includes setup, verification and supervision rather than just
N4 operation time. All group/reap/EOF receipts completed.

| Grid / mask | Implementation | Sampled group peak RSS bytes | Process elapsed ms |
| --- | --- | ---: | ---: |
| 64x48x32 / dense | baseline | 1785856 [1769472, 2277376] | 52.192959 [51.161083, 52.393208] |
| 64x48x32 / dense | candidate | 2244608 [1622016, 2277376] | 51.457584 [50.020292, 55.116416] |
| 64x48x32 / 1/64 | baseline | 2244608 [1933312, 2277376] | 45.066709 [43.438000, 47.341208] |
| 64x48x32 / 1/64 | candidate | 2179072 [1343488, 2277376] | 46.785250 [45.072959, 48.525666] |
| 128x96x1 / dense | baseline | 2260992 [2211840, 2277376] | 45.127583 [44.659875, 47.699334] |
| 128x96x1 / dense | candidate | 2277376 [2179072, 2277376] | 46.227625 [45.211459, 46.765708] |
| 128x96x1 / 1/64 | baseline | 2277376 [2179072, 2277376] | 46.613583 [43.525375, 47.115833] |
| 128x96x1 / 1/64 | candidate | 2015232 [1179648, 2277376] | 45.762375 [42.421334, 47.433125] |
| 128x96x3 / dense | baseline | 1163264 [1146880, 2277376] | 45.233500 [44.604250, 46.686125] |
| 128x96x3 / dense | candidate | 2260992 [2244608, 2260992] | 45.751958 [45.625833, 47.784875] |
| 128x96x3 / 1/64 | baseline | 1458176 [1392640, 1703936] | 47.680000 [46.708583, 48.500042] |
| 128x96x3 / 1/64 | candidate | 2277376 [2162688, 2277376] | 45.649333 [44.772583, 48.098500] |

Public cancellation used an independent standard context timer requested at
5ms, input128x96x32, no explicit mask, shrink1, requested iterations[50,50]
and Workers=2. Every call returned nil plus context.Canceled and joined its
workers. The request timestamp precedes cancel() inside the timer callback;
request-to-return includes that callback's scheduling/cancel delivery and the
remaining public call. No phase was selected or latency threshold tuned.

| Implementation | Public total ms | Actual request after start ms | Request-to-return ms |
| --- | ---: | ---: | ---: |
| baseline | 5.346084 [5.250417, 22.710541] | 5.152417 [5.038125, 5.810333] | 0.307959 [0.098000, 16.900208] |
| candidate | 5.108625 [5.040125, 5.153209] | 5.106916 [5.035709, 5.151167] | 0.002042 [0.001709, 0.004416] |

These six observations describe this host and workload; polling bounds do
not turn them into a universal wall-clock cancellation guarantee. The fixed
1/16 sparse crossover remains a heuristic, without a measured crossover
search. Earlier preparation and gate STOP evidence was retained: protected
setuid /bin/ps identity instrumentation and the standalone test package cwd
were corrected; product/math/fixtures were unchanged and the cost campaign
was executed only once. This guide was updated after measurement; all tested
implementation/test bytes remain unchanged. Full-module integration,
minimum-version, race and documentation checks remain separate qualification
steps and are not represented by the affected-package/cost results above.
