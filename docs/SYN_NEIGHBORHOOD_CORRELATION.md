# Local correlation and greedy symmetric normalization


## Metric contract

For each eligible fixed centre, collect pairs in its rectangular neighbourhood,
subtract the respective local means, and compute

`E = -mean(C*C/(F*M))`, where `C = sum(f*m)`, `F = sum(f*f)` and
`M = sum(m*m)` over centred intensities. Online means followed by centred sums
avoid subtracting large raw moments. Reduction follows index order, serially;
the `MetricDomain.Workers` setting does not change this kernel's result or start
additional workers. Concurrent evaluations are safe when borrowed inputs and
the supplied transform remain stable.

Exactly one radius representation is required. `Radius` is in fixed-index
samples; `RadiusMM` converts each fixed-index axis with `floor(mm/spacing)`.
One zero-radius axis is allowed; an entirely zero neighbourhood is refused,
including physical radii which convert to all zero. The radius is a rectangular
half-width, not a patient-space spherical distance. At pyramid levels physical
radii are converted using that level's spacing, while voxel radii keep their
specified number of samples.

Both masks restrict centres and every contributing pair, with nearest-neighbour
nonzero sampling in each mask's own geometry. Image edges clip neighbourhoods;
linear image interpolation uses the existing half-sample/clamped border
contract. Centres outside overlap are excluded, as are neighbourhoods with
fewer than two pairs or either mean squared centred intensity at/below
`MinimumVariance` (zero excludes exact constants). No eligible centre returns
`ErrTooFewSamples`, including empty masks and empty sparse centre lists.
Visited nonfinite intensities, masks, mappings, gradients or Jacobians return
an error; unused samples outside a metric's domain are not scanned. Domain
membership is discontinuous at mask, overlap and variance thresholds; derivatives
apply with membership fixed, away from those thresholds and interpolation knots.

The derivative includes every pair's moving-image interpolation gradient and
transform Jacobian, including mean/normalization dependence. ITK's fast ANTs
neighbourhood metric approximates the transform derivative with the centre
sample. Values agree on qualified fixtures; identical derivatives or optimizer
trajectories are not claimed. No sliding-window optimization is included.
See the [official ITK metric description](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1ANTSNeighborhoodCorrelationImageToImageMetricv4.html).

Default admission limits are 1,000,000 centres, 4,096 samples per neighbourhood,
16 MiB owned payload and 100,000,000 sample/parameter visits. Sparse centres are
copied after admission; images and masks remain borrowed. Payload charges are
24 bytes per captured centre, 64 per window pair and 40 per parameter during
derivative evaluation. Transform derivative arity is limited to 4,096. Limits
can be supplied explicitly; they bound logical work/payload, not elapsed time
or process RSS. Context variants check during pair and derivative evaluation;
errors/cancellation return no partial derivative.

## Halfway fields, energy and domain

SyN's virtual grid is the fixed image grid at each coarse-to-fine level. The
halfway maps are `Hf(p)=p+uf(p)` and `Hm(p)=p+um(p)`; compare `F(Hf(p))` with
`M(Hm(p))`. Full local intensity derivatives are chained through the half-field
position Jacobians. Optional `Regularization` weights the mean physical
membrane energy of both fields (squared neighbouring vector differences divided
by axis spacing squared). The total energy, raw correlation and stop reason
are exposed separately in progress.

Both update fields can receive physical Gaussian smoothing, using the existing
discrete Gaussian kernel (error 0.001, maximum radius 16, sigma/spacing at most
10). They share a maximum-norm step normalization. Each line-search candidate
uses the existing `exponentiate` and `composeField` helpers and must decrease
total energy and satisfy a conservative half-field gradient bound. Virtual
grid faces are pinned to zero displacement. This is a bounded near-identity
profile: arbitrary large deformations, automatic rigid initialization and
unrestricted topology are not supported. Stops include `stationary`,
`no-admissible-descent` and `iteration-limit`; a stop is not anatomical acceptance.
The method follows the halfway idea described by
[ITK SyN](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1SyNImageRegistrationMethod.html),
with an independent full-gradient/membrane-energy update rule.

The final mappings are `FixedToMoving = Hm o inverse(Hf)` and
`MovingToFixed = Hf o inverse(Hm)`, sampled on their respective input grids.
Inversion uses `spatialreg.PreparedInverse`, which inverts the constant
matrices once and snapshots the half field (12 bytes per node) for the
parallel per-voxel queries; its iteration is that of
`spatialreg.Registration.InvertPoint`. The half-field inverse
residual is reported separately from measured forward/inverse dense-grid
composition error. A failed inverse or a point outside the target image yields
triple NaN and a false validity mask, never an invented identity mapping.
Use `DisplacementField.Grid()` for such fields; the existing
`NewDisplacementFieldTransform` requires entirely finite storage.

Each fully defined trilinear cell receives a componentwise edge-derivative
bound whose Frobenius norm bounds the physical displacement gradient. Direction
matrix inverse norm and admitted orthonormal tolerance are included. A norm
strictly below one ensures positive `det(I+grad u)` throughout those cells,
not only at nodes. Final fields must contain at least one certified cell and
both maximum bounds must be below one. Cells containing undefined corners are
excluded and their coverage is reported. This local certificate does not assert
a global inverse across gaps or anatomical accuracy. Input directions must be
right-handed orthonormal within 1e-8; all input/mask samples must be finite.

`spatialreg.WriteDeformable` writes **Deformable Spatial Registration**, not a
Structured Report. With `FixedToMoving.Grid()`, registered FoR is fixed and
source FoR is moving: this DICOM deformable grid maps registered to source.
Exchange those identities for `MovingToFixed`. No file is written by SyN itself.
Vectors and validity masks returned to callers are owned, mutable storage;
certificates describe the returned snapshot before caller modification.

## Admission, cancellation and helpers

Levels must be nonincreasing shrink factors, finish at shrink one, and have
1–500 iterations each; at most 16 levels and 20 backtracks are admitted. Empty
levels select one full-resolution level of 50 iterations. Explicit correlation
radii are still required. Defaults: 1,000,000 voxels per source/mask, 256 MiB
live owned payload, 10 billion logical work charges and 12 backtracks. The
conservative work charge includes neighbourhood searches, smoothing, field
helpers and inversion; the voxel maximum alone does not guarantee admission.

The live payload estimate is `640*largest_input_voxel_count +
80*neighbourhood_samples`, covering simultaneous images, half-fields, gradients,
candidate/update/helper buffers, final maps and validity storage. It excludes
caller inputs, runtime, allocator overhead and uncollected garbage. It is not a
process-memory cap or cumulative allocation estimate. Limits are checked before
large working buffers; tests reject one byte below the estimate and accept the
estimate for the specified fixture. Inverse defaults: 50 iterations, 1e-5 mm
tolerance and 0.001 mm maximum residual; at most 1,000 iterations.

The per-voxel passes (half-field warping, local correlation and its
derivatives, update assembly and scaling, update smoothing, the step norm and
the continuous-cell certificates of every candidate and the final
inversions) run on the calling goroutine plus helpers from the
module's shared worker budget. Each voxel's outputs are computed as
serially: correlation sums run in centre order, and each voxel gathers its
derivative terms in the centre order a serial scatter would add them, so
results are bit-identical for any number of workers. With zero
`Regularization` the membrane energy and gradient are not computed. The
backtracking candidates are composed into two reused buffers that trade
places with the accepted half fields, without copying the fields first.

Context is checked during scans, metrics, smoothing, inversion and certificates,
and around existing joined field helpers. Helpers borrow the module's
shared worker budget; cancellation can wait for one bounded helper phase. No
detached worker or partial proposal is returned. Progress observes committed
iterations and can cancel the call. Tests cancel at both pyramid levels and
verify unchanged borrowed inputs.


| Profile | Before median | After median | Change (benchstat) | B/op before → after |
| --- | ---: | ---: | ---: | ---: |
| Membrane weight 0.1 | 60.03 s ± 35% | 23.87 s ± 111% | -60.24% (p=0.009) | 4.358 GiB → 2.299 GiB |
| Unregularized | 44.12 s ± 71% | 22.79 s ± 56% | -48.35% (p=0.004) | 3.779 GiB → 1.907 GiB |

With only the other passes parallel, a CPU profile of the membrane profile
spent about four fifths of SyN's time in the serial cell certificates of the
backtracking candidates, so they run in parallel too.


Windows/amd64, Go 1.26.4, Intel Core i7-14700K. No GPU is used: these are offline
library numerical APIs, not application's 3D rendering. ITK 5.4.5 / NumPy 2.2.6
ran under CPython 3.12. ANTs was the official Windows x64 2.6.5 release,
`2.6.5-gfdce4d2`, compiled 2026-01-14. Release ZIP SHA-256:
`2cfb09f9bfd796720819054711f493abd750dd3245875caaa5d04000d3a98c3d`.

The original 16³ phantom has known displacement
`ux=0.8*sin(pi*x/15)*sin(pi*y/15)^2*sin(pi*z/15)^2` mm, zero face displacement,
analytic positive Jacobian and an independently bisected inverse. The moving
image is a spatially deformed `2.3*fixed+3`, not identical intensities. Twenty-seven
interior landmarks must remain within 0.3 mm; forward/inverse residual must be
below 0.06 mm. Swapping images reproduces the declared inverse within 1e-5 mm.

| Method | Maximum landmark error | Minimum node Jacobian | Folded nodes |
| --- | ---: | ---: | ---: |
| Go full-gradient SyN, membrane weight 0.1 | 0.227523 mm | 0.844237 forward / 0.868603 inverse | 0 / 0 |
| Native ITK SyN | 0.179828 mm | 0.724486 | 0 |
| Native ANTs SyN | 0.179827 mm | 0.724490 | 0 |

Go forward/inverse maximum residual is 0.016929 mm; half-inverse solver residual
is at most 9.997e-6 mm. Forward/inverse continuous-cell gradient bounds are
0.196077 / 0.211211. An earlier unregularized candidate attained correlation
near -1 but failed the landmark threshold (0.472 mm); metric improvement alone
was insufficient. The qualified membrane weight fixes this fixture, not a
universal aperture problem. All three methods use radius two, 60 iterations and
0.3 mm learning step; references use update Gaussian variance 0.64 and total-field
variance 0.5. Their total-field regularization differs from Go membrane energy.
Reference Jacobians are independent NumPy finite differences at nodes.

Local NCC values on three 7×6×5 fixtures agree with real ITK within 2e-6.
An analytical clipped 1D fixture gives -59/84 instead of the global -3/28.
Full translation derivatives on rotated anisotropic images agree with central
differences (step 1e-5, tolerance 1e-7); dense intensity/membrane derivatives use
step 1e-6, tolerances 1e-8/1e-9. Tests also cover masks, constants, nonfinite
data, resource rejection, cancellation and DICOM round-trip in both directions
with rotated origin/spacing and undefined regions.

| Timed scope, one thread | Observation | Allocation |
| --- | ---: | ---: |
| Go SyN complete API, median of three single operations | 1.948209 s | 422,821,064 cumulative bytes; 11,320 allocations |
| Go local NCC value, median of three | 41.1147 ms | 8,224 bytes; 2 allocations |
| Go local NCC full translation derivative, median of three | 127.2618 ms | 8,328 bytes; 4 allocations |
| ITK `Update`, one observation | 0.255127 s | not instrumented |
| ANTs CLI process, one observation | 0.343797 s | not instrumented |

These scopes differ: the Go API includes both final grid inversions and continuous
certificates; ITK timing excludes Python startup, and ANTs includes native process
startup. They are cost records, not a speed equivalence or percentile claim.
One separately launched Windows test process containing two Go registrations and
geometric QA observed a kernel peak working set of 63,393,792 bytes (25 ms polling,
before process exit). This includes runtime/test overhead and is not the logical
payload estimate or a universal RSS bound. The tested estimate was 2,631,440 bytes.

