# Explicit Poisson denoising research profile

`imagefilter.DenoisePoisson` is an opt-in 2D/3D nonnegative count-intensity
estimator. It leaves `NonLocalMeans` and its Gaussian-like `NoiseSigma` contract
unchanged. This first profile implements Poisson only. Rician magnitude data,
Gaussian readout noise, unknown offsets, background subtraction, spatial noise
maps and multichannel coupling are not qualified. There are no clinical or
viewer presets. No Twin integration or GPU rendering policy changes are made.

## Model and units

`CountScale` is mandatory, in counts per input unit, within `[1e-12,1e12]`.
For independent integer photon/event counts, the model is Poisson with variance
equal to expected counts. Nonnegative fractional scaled observations use the
continuous quasi-likelihood extension of that objective; they are accepted
explicitly and counted in `FractionalCountSamples`, not certified as discrete
Poisson observations. The caller must justify gain, offset and independence.
The library neither estimates noise sigma nor infers a modality's noise model.
Negatives, NaN and infinity are rejected; no clamp/background replacement occurs.

Given scaled observations `y`, the estimator minimizes

```
F(x) = sum_i [x_i - y_i + y_i log(y_i/x_i)]
       + (lambda/2) sum_axial_edges [(x_i-x_j)/spacing_axis_mm]^2, x >= 0.
```

The zero-count likelihood contribution is `x_i`; positive observed counts
require positive estimates. The fidelity is the public Poisson negative log
likelihood, as discussed in [Figueiredo and Bioucas-Dias, 2010](https://arxiv.org/abs/1001.2244).
The prior and scalar solver here are independently derived from this declared
objective; this is not their alternating-direction solver or ITK's patch entropy
method. With neighbor weights `w=1/spacing²`, one coordinate has derivative
`1-y/x + lambda*(degree*x-neighbor_sum)`. Its nonnegative quadratic root is
computed using `Hypot` and a rationalized branch to avoid subtractive cancellation.
No epsilon floor discards true small positive counts. Coordinates are updated
sequentially, deterministically, x fastest. No extra threads or patch buffers exist.

Only existing nearest axial neighbors contribute at borders (natural/no-flux);
there is no wrap, duplicated edge, padding or PSF. Singleton axes have no edges.
Active-axis physical weights must lie in `[1e-12,1e12]` (approximately spacing
`1e-6` through `1e6` mm). Singleton spacing must be finite and nonnegative.
The quadratic prior can blur edges and small structures and bias means; it does
not preserve acquisition measurements or promise edge-preserving enhancement.

`Lambda=0` is owned exact identity, not an implicit smoothing preset. Positive
lambda is explicit, in mm²/count within `[1e-12,1e6]`. Count gain changes the
relative fidelity/prior scale: to keep identical physical optimization when gain
changes from `s` to `s'`, use `lambda'=lambda*s/s'`. Iterations zero means 20;
`RelativeChange=0` disables stopping, otherwise `(0,1]` compares maximum absolute
coordinate change to the original maximum count. Finite sweeps are an estimate,
not a proof of reaching the exact global minimum; both objective values, stop
reason, completed sweeps and final change are reported. Output rounds to float32.
Deviance, means, objective and penalty describe the float64 pre-rounding estimate;
very small changes may round away. Positive observations remain positive during
optimization; an estimate below float32 representability may round to zero.

## Ownership, provenance and finite admission

Input data and source references must stay immutable during a call. Output data
and cloned source strings are independently owned. At least one stable source
reference is mandatory; the library records source float32 bytes' SHA256,
geometry, model, domain, method, parameters, budgets and stop diagnostics. Hash
covers ordered little-endian source samples; geometry is a separate recorded
field, not part of that hash. Identifiers are caller data, not validated DICOM
UIDs. Repeated runs do not modify the acquisition. An application integrating
this estimator must identify the derived result, preserve/reset the source, and
keep estimation/display values separate from acquisition measurements.

Admission happens before allocating voxel buffers. Zero budget selects the finite
default below; negative limits or limits above hard ceilings are invalid.

| Budget | Default | Hard ceiling |
| --- | ---: | ---: |
| Voxels | 1,048,576 | 16,777,216 |
| Sweeps | 128 | 4096 |
| Float64 scratch payload | 16 MiB | 128 MiB |
| Owned working payload | 32 MiB | 256 MiB |
| Conservative neighbor visits | 100,000,000 | 5,000,000,000 |
| Source references | 32 | 1024 |
| Reference UTF-8 bytes | 4096 | 1 MiB |

Scratch is `8*n`; working is `12*n + reference_bytes + 16*reference_count`,
covering scratch, owned float32 output, cloned string backing and headers on
64-bit hosts. Fixed structs, allocator/runtime, input and SHA state are excluded;
these logical payloads are not RSS. Visit bound is `n*(6*sweeps+24)`; it is
conservative for 2D/borders, identity and stopping, and bounds the two objective
scans plus setup/output accounting. No FFT, full-volume second intensity
collection or unbounded search is used. Cancellation is checked before admission,
then every 256 samples during validation, setup, sweeps, objective and output,
and immediately before publication. Errors/cancellation return nil, never a
partially published result; admitted private scratch may have been allocated.

## Current local qualification and measured limits


### Quantitative fixed-seed controls

The fixed seed1503 corpus has four Poisson means on both 64x64 and16x16x16 grids,
4096 voxels each. CountScale1/lambda0.02/40 sweeps/natural physical spacing1mm
are explicit experimental choices, not modality presets. The same input sequence
is reshaped across grids, isolating adjacency effects. Both zero controls passed
exact zero mean/variance/deviance; the six noisy controls passed independent
bias/variance/MSE and deviance checks. The estimator has visible downward bias,
particularly near the low-count floor; variance reduction does not establish
unbiased quantitative acquisition preservation.

Observed input metrics (mean in counts, variance/MSE in counts squared):

| Truth | Observed mean | Variance | MSE |
| ---: | ---: | ---: | ---: |
| 0 | 0 | 0 | 0 |
| 0.25 | 0.249755859375 | 0.257690370083 | 0.257690429688 |
| 4 | 4.041259765625 | 4.169440209866 | 4.171142578125 |
| 40 | 40.082275390625 | 41.073064744473 | 41.079833984375 |

Restored float32 output metrics below use independent test reductions. Bias is
restored mean minus truth; deviance compares observed counts against restored
values (the API report uses pre-rounding float64 estimates). Values are rounded
for readability; all raw metrics remain in the retained qualification log.

| Grid | Truth | Restored mean (counts) | Bias (counts) | Variance (counts²) | MSE (counts²) | Deviance |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 64² | 0 | 0 | 0 | 0 | 0 | 0 |
| 64² | 0.25 | 0.232945038253 | -0.017054961747 | 0.217395272402 | 0.217686144122 | 6.170597206514 |
| 64² | 4.0 | 3.866734999057 | -0.133265000943 | 2.541367775994 | 2.559127336471 | 239.178949704971 |
| 64² | 40.0 | 39.895017638803 | -0.104982361197 | 4.791643146610 | 4.802664442772 | 2199.699113211923 |
| 16³ | 0 | 0 | 0 | 0 | 0 | 0 |
| 16³ | 0.25 | 0.226940284352 | -0.023059715648 | 0.204601876549 | 0.205133627035 | 11.183176478743 |
| 16³ | 4.0 | 3.825774398836 | -0.174225601164 | 2.143897202313 | 2.174251762414 | 385.078743836546 |
| 16³ | 40.0 | 39.922677531838 | -0.077322468162 | 2.453813547399 | 2.459792311482 | 2661.982325559246 |

Independent bisection and analytic two-voxel optimum controls also passed,
along with identity across gains (including0.7, signedzero, subnormal and
MaxFloat32), constants/noiseless input, anisotropic borders, gain equivalence,
outliers/nonfinite/domain rejection, resource admission, derived ownership,
cancellation and deterministic concurrent calls. Eight flat phantoms do not
qualify arbitrary anatomy, textures, structural fidelity, dose or gain inference.

### Cost and whole-process RSS

The benchmark used ten sweeps, CountScale1/lambda0.1 and synthetic positive counts,
three cells × five100ms samples. Setup was outside timing; admission, source hash,
optimization, diagnostics and owned result allocation were inside. These data
measure this direct scalar-stencil implementation; they do not prove speedup over
NLM, ITK or another solver, nor costs at the hard admission ceilings.

| Grid | Time min (ns/op) | Median (ns/op) | Max (ns/op) | B/op | Allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: |
| 32² | 219785 | 222741 | 226348 | 12880 | 9 |
| 64² | 870406 | 878325 | 881728 | 49744 | 9 |
| 16³ | 931178 | 935965 | 940051 | 49744 | 9 |

RSS used a separately compiled native arm64 test executable and a fresh process
per cell × five, `/usr/bin/time -l` on Darwin. Maximum resident set size is reported
in **bytes**. Whole-process peak includes Go runtime, test runner, input fixture,
output, scratch and allocator overhead. It is not algorithm-only memory, a live
process subtraction, or B/op converted to RSS. RSS variation across these small
cells is dominated by process/runtime effects and does not establish memory
monotonicity or predict large-volume/application peak RSS.

| Grid | Minimum peak RSS (bytes) | Median (bytes) | Maximum (bytes) |
| --- | ---: | ---: | ---: |
| 32² | 5996544 | 6062080 | 6324224 |
| 64² | 6078464 | 6144000 | 6225920 |
| 16³ | 6078464 | 6127616 | 6144000 |

The finite retained recipes were focal17/race2/minimum17 outer300s/test60s/cap4MiB;
cost outer180s/test60s/cap4MiB; RSS compile outer300s/cap4MiB, then child30s/
test20s/cap256KiB and aggregate300s for15 children. Default Go cache was reused,
with parent-controlled idle cleanup below40000000000 available bytes; the runner's
separate operational disk floor was4GiB. No source/cache was removed by the filter.

ITK oracle remains opt-in and not applicable to equality for its current entropy
patch method. A future matched peer must declare the same objective, count gain,
lambda, physical adjacency/borders, solver ordering/stopping and precision. No
external peer was executed. No clinical/viewer presets, GUI/PACS qualification,
full-suite race claim or CPU3D-rendering integration follows from these results.
