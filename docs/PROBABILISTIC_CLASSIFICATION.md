# Scalar Gaussian mixture, Bayes and physical MRF

`roi.FitGaussianMixture` learns Gaussian means, variances and mixture weights
from scalar intensities by expectation maximization. `ClassifyGaussianBayes`
uses an explicit model without fitting. Both optionally run a six-neighbor
attractive Potts MRF using synchronous damped mean-field inference. This is not
MultiOtsu, STAPLE label consensus, seed-based Mahalanobis growing or an AI model.
No Twin integration or automatic clinical segmentation is introduced.

## Model and interpretation

One scalar channel initially is supported. The x-fastest `Values` length must
match `Dimensions`. A mask selects valid voxels; `SpacingMM` is positive finite
physical spacing. Unit and normalization identifiers must exactly match the
options. They identify already calibrated samples; this API never silently
converts units, scales or mixes channels. Multichannel/duplicate-channel
covariances are outside this initial scalar API, rather than being silently
accepted as a singular multichannel model.

For Gaussian class j, density is
`exp(-(x-mean[j])²/(2*variance[j])) / sqrt(2*pi*variance[j])`.
Bayes posterior is `prior[j]*density[j] / sum(prior*density)`; computation uses
log-sum-exp. Priors default to learned/supplied mixture weights. Explicit
nonnegative priors replace those weights for classification only, are normalized,
and may assign zero probability to a class. All-zero or nonfinite priors fail.
`LogLikelihood` is the sum of log evidence of this unary classifier, including
explicit priors; it is not a joint MRF likelihood or clinical accuracy metric.

Fitting uses weighted online central moments, avoiding subtraction of large
raw second moments. Initial parameters may be explicit; otherwise a local
`math/rand` source seeded by `Seed` uses k-means++ distance sampling. Initial
variances equal the population variance, floored. This initializes an EM mixture;
it does not run a separate k-means clustering pass. Every update learns weights,
means and population variances. Responsibility mass below 1e-12 fails with
`ErrProbabilisticEmptyClass`; classes are never silently dropped or relabelled.
Constants can fit one regularized class; seeded initialization of multiple
classes from identical values refuses the unidentifiable initialization.
Singular nonnegative variance is raised to explicit `VarianceFloor` (default
1e-6 in input units squared). Negative or nonfinite variance is invalid.

EM stops before another update when change in summed log evidence is at most
`EMTolerance*(1+abs(previous evidence))`, default 1e-8, or at `EMIterations`
(default100). Budget exhaustion returns a valid finite result with
`EMConverged=false`; it is never called converged. Component order is canonical
by increasing mean, then variance, then weight. Explicit priors refer to this
canonical order. Labels are one-based; exact ties choose the first class.
Seed and stopping are reproducible for the same input, options and Go runtime;
there is no claim of finding the global optimum or anatomically identifying
classes. Identical Gaussian components remain statistically unidentifiable.

## Spatial inference

The MRF has unary negative log Bayes probabilities and attractive Potts pair
energy `-Beta/distanceMM * [label_i==label_j]`. Its six axial neighbors are
weighted by inverse physical spacing, so longer inter-slice distances contribute
less than short in-plane distances. If `ContrastSigma>0`, multiply an edge by
`exp(-0.5*((value_i-value_j)/ContrastSigma)²)`. Beta has units mm; sigma has input
units. Beta0 disables spatial inference. Masked or skipped voxels provide no
neighbor evidence; boundaries are clipped, never wrapped between rows/planes.

Each mean-field iteration computes all new marginals from the previous volume:
`softmax(log unary posterior + sum(edgeWeight*neighbor marginal))`, damped by
`Damping` (default0.5). It stops on maximum absolute probability change below
`Tolerance` (default1e-6) or the finite iteration budget (default20 if enabled).
`MRFIterations/MRFConverged` expose this approximation and stopping. Returned
posteriors with MRF are approximate marginals, not exact joint probabilities.
An actual zero prior remains impossible. Normalized unary log probabilities are
preserved before exponentiation, so a positive prior whose displayed Bayes
probability underflows to zero can still be recovered by neighbor evidence.
No artificial pseudoprior is invented.
Spatial agreement or high likelihood is not anatomical truth. This solver does
not claim bit-equivalence to ITK's different smoothing/MRF optimizer.

## Admission, cancellation and ownership

Defaults limit voxels to1,048,576, classes to8, iterations to256 and logical
workspace to256MiB; zero limit fields select these defaults. Absolute classes
are capped64 and requested iteration limits4096. Dimensions are checked for
integer/product overflow before allocation. Workspace conservatively includes
three voxel-major buffers (output probabilities, normalized log unary and next
MRF probabilities; log unary replaces rather than adds a fourth buffer), validity mask, uint16 labels and bounded
class/model/provenance/EM scratch; input storage and allocator/runtime overhead
are excluded. Admission happens before allocating any voxel-sized buffer.
Numeric scalar magnitude is limited1e100, variance is at most1e201, variance
floor1e-100..1e200, spacing1e-9..1e9mm, Beta<=1e9 and identifier length<=256.
Out-of-domain values fail instead of publishing nonfinite arithmetic.

Default policy rejects selected NaN/±Inf. Explicit `SkipNonFinite` excludes them
and records count. Masked-out values never participate. Empty selection fails.
Excluded voxels have label0 and all-zero posterior rows, rather than a fake
normalized classification. Selected posterior rows normalize to1.

Calls are synchronous, use no worker goroutines or global caches, and poll
context during input validation, initialization, EM, unary/MRF inference,
labelling and provenance hashing. Failure/cancellation returns nil result; no
partial label map or stale result is published. Scratch becomes unreachable
on return; garbage-collector timing is not a deterministic process RSS bound.
Inputs, model and option slices are never edited. Callers must keep borrowed
input arrays immutable during the call (including concurrent writers); results
and metadata are detached caller-owned allocations. Concurrent independent
calls may share read-only input. Source SHA256 binds dimensions, physical spacing,
raw samples, selection mask and calibration identifiers. Parameter SHA256 binds
normalized solver options and the canonical final model.

## Qualification recipe and limits

Synthetic tests provide independent direct density/evidence/posterior values,
known separated mixture parameters, seed/permutation/stopping checks, constant
and empty/singular policies, nonfinite/calibration refusal, physical anisotropic
and contrast mean-field oracles, masked boundaries, quota/overflow admission,
detached ownership, cancellation and concurrent independent calls.
Actual focused qualification passed 16 top-level tests and three race cases in
`roi`, with zero failures or skips, using Go1.27.1 on darwin/arm64 (CGO enabled).
The positive-prior underflow fixture independently verifies recovery of the
center voxel under physical neighbor evidence. The original G1 defect was
found statically; G1 was not executed and is not a runtime failure record.

Four fresh-process, single-iteration cost cells used the same synthetic resident
16³/32³ inputs, EM fitting followed by Bayes, with or without MRF. All four had
maximum mean absolute error0 against independent component means -3 and+5,
and maximum posterior-sum error0. These errors are separate numerical measures.

| Profile | Grid | ns/op | B/op | allocs/op | Process peak RSS (bytes) |
| --- | --- | ---: | ---: | ---: | ---: |
| EM + Bayes | 16³ | 1570875 | 120528 | 307 | 10321920 |
| EM + Bayes + MRF | 16³ | 1062250 | 251440 | 308 | 10403840 |
| EM + Bayes + MRF | 32³ | 7156750 | 1713872 | 309 | 12025856 |
| EM + Bayes | 32³ | 5465750 | 665296 | 307 | 11091968 |

RSS is actual Darwin `/usr/bin/time -l` maximum resident set size of the whole
process, including the resident synthetic input, test harness and Go runtime.
It is distinct from benchmark allocations and the logical workspace admission
bound; it is not an isolated scratch peak. Input construction was outside the
benchmark timer; the timed workload includes its numerical checks. One sample
per cell provides neither statistical comparison nor a universal speedup claim.
The smaller MRF cell's shorter elapsed sample is not evidence that adding MRF
reduces work. No legacy baseline, live clinical accuracy or ITK runtime was
measured.

Receipts: targeted `eb8f997f6e82ea54afbeae658e0ac1f74604662ff239aa34395a61edfdf99ae9`,
race `d99b2f1e0a3d2fd1686fe5febacce349bb9bb478f3a400b2d732dff6e8d0b8ec`,
and cost `60f3c64262ad59547ef2af884ce320e1a9f3109f3eaa8d03732ad8e506b08ced`.
Before/after/final guards verified all4777 base library paths, four unchanged
Go overlays, execution artifacts and toolchain; each child was reaped and its
owned process group/output drained. The finite supervisor uses a4GiB minimum
operational disk threshold and requests idle cache cleanup below40GB decimal;
these are operational limits, not classifier RSS claims.

All sixteen selected cases also passed on the real integrated tree with Go
1.22.12 and default vet, using external linking and CGO on Darwin/arm64.
Library `make check` and `make docs-check` passed for the integrated delivery.
ITK is opt-in:
only compare a matching scalar fixed Gaussian model/prior Bayes configuration,
with explicit numerical tolerances; mean-field MRF is not an equivalent ITK
optimizer. No optional runtime, clinical fixture acquisition or CI is required.

Clean-room references: public Gaussian/Bayes/EM/Potts mathematics and official
[ITK Bayesian classifier documentation](https://docs.itk.org/projects/doxygen/en/latest/classitk_1_1BayesianClassifierImageFilter.html)
and [ITK MRF contract](https://docs.itk.org/projects/doxygen/en/latest/classitk_1_1MRFImageFilter.html).
No reference implementation code or assets were copied.
