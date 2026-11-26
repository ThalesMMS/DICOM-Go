# Scalar Chan–Vese research proposals

`roi.ChanVese` is an offline CPU quantitative segmentation solver returning
owned full-coordinate RasterMasks. It never modifies source masks or Pixel
Data, writes DICOM, or changes a viewer's accepted segmentation. Twin integration
is deferred. It does not perform or enable application CPU 3D rendering.

## Explicit discrete profile

Positive phi denotes inside; phi is measured in mm. Initialization uses supplied
raster membership as phi=+2epsilon inside and -2epsilon outside, not an implicit
seed radius or a signed-distance transform. epsilon is an explicit mm smoothing
width (default smallest active spacing). The smooth membership is
H(phi)=1/2+atan(phi/epsilon)/pi.

The minimized discrete functional is the physical quadrature of
`mu * sqrt(|grad H|² + eta²) - mu*eta + nu*H + lambdaI*H*(I-ci)² + lambdaO*(1-H)*(I-co)²`.
ci/co are weighted means of all **available samples in the fixed explicit box**;
full volume is the default box. Spacing is per physical index axis and the
measure is sx*sy in 2D or sx*sy*sz in 3D. The gradient is a forward difference of
H divided by spacing. Its divergence is the exact negative transpose. This
regularizes total variation of membership, a smooth discrete Chan–Vese model;
it is not a copied ITK implementation or a claim of identical time integrators.
There is no implicit division of curvature by dimension: the divergence is
used directly. eta has units 1/mm (default1e-6/minspacing), mu has units
intensity²*mm, nu intensity² and lambda dimensionless when energy is in
intensity²*mm^dimension. The regularized baseline mu*eta is subtracted without
changing derivatives. Negative nu may favor expansion; mu/lambdas are nonnegative.
When both lambdas are zero the default pair1,1 is selected; individual zero
weights with the other nonzero are explicit. Use DefaultChanVeseOptions to opt
into mu=.2; zero Mu in a literal means zero perimeter regularization.

The phi derivative includes atan delta and the exact discrete TV divergence.
Regional-mean derivatives cancel at their least-squares minimizers. Each explicit
step is capped at0.45*minimumspacing in phi by adapting dt, also capped by
MaximumTimeStep (default1, formal mm²/intensity²). Neither the explicit scheme
nor this adaptive cap guarantees decreasing energy every iteration. History
records actual energy, regional means, membership voxel count, RMS phi change
in mm and timestep, including initialization. Smoothing gives a local two-phase
model, not arbitrary global optimality or clinical boundary assurance.

## Domain and admission

The box does not grow during evolution; means exclude every outside sample,
including when large ROI initialization extends outside the box. No image callback
is invoked outside it. Unavailable samples are excluded from both phases; edges
to unavailable samples are cut, producing zero-flux boundaries. An available
NaN, infinity or magnitude above MaxFloat32 is an error; unavailable NaN is ignored.
Source callbacks/masks must remain stable during the call. Only immutable copies
of sampled data and detached output are retained.

Initial hard inside empty is ErrLevelSetEmpty; initial hard outside empty is
ErrLevelSetInvalidInput. No automatic seed creation, label swap, fallback means
or region repair occurs. Weighted phases must remain numerically nonempty or
return an error. Evolution reaching a hard empty/full phase returns its owned
terminal proposal with explicit empty-inside/empty-outside reason and Converged
false. Numerical stopping is RMS tolerance, or unchanged masks over a specified
window **and RMS <= ten times that tolerance**, otherwise maxiterations.
Stability does not establish clinical adequacy. Boundary-contact flags distinguish
cropped-box contact from source faces; the inactive z axis in 2D is excluded.

LevelSetLimits defaults remain16Mi voxels/1GiB. MaxIterations defaults200 and
is capped100000. MaxWork defaults2e9 estimated eight voxel passes*(iterations+1).
All shape products/bytes/work are checked before allocation. The owned logical
ledger is112*N +24*sourceRows*regionSlices +128*regionSlices
+96*(maxiterations+1)+4096 bytes: image/phi/membership/speed/three flux arrays,
availability, worst-case fragmented result runs and full-coordinate rowheaders,
map metadata and bounded history. It is conservative requested storage, not
measured RSS, allocator/GC/stack/runtime memory or borrowed input storage.
Single caller-thread evolution has no helpers to leak; progress runs on the
caller. Preparation, statistics, flux, update, output and final publication
check context. Any error/cancellation returns nil output wrapping the context
cause and ErrLevelSetCancelled; partially produced masks/history are not returned.

## Actual scoped qualification

On macOS darwin/arm64, Apple M4 Pro, Go1.27.1, selected contract tests passed13 top cases and race qualification passed2 top cases without failures/skips. These cover independent direct energy and finite-difference gradients, the known two-region solution, uniform/low-contrast/noisy/anisotropic/face-contact/internal/external phantoms, invalid/cropped/unavailable inputs, finite limits, fragmented masks, concurrent ownership and late cancellation. Centered weighted means preserved the exact constant-image assertions.

The explicit ITK5.4.5 profile was actually compiled through the existing optimized SDK, run over8 input-bound fixtures and compared with the preset Dice0.95. All8 produced Dice1.0 against Go; the separate fixed acceptance test passed with defaultvet and no skips. The default-counter1 control and its4 original failures remain documented below; the corrected comparison is energy-family/profile validation, not identical evolution or clinical parity.

Canonical Go1.22.12 qualification passed all14 selected functions, including the eight ITK cases, with default vet, zero skips and the external linker required by macOS27. The canonical library `make check` baseline passed in57.84s under Go1.27.1. Twin integration is deferred; no GUI or Twin baseline is implied.

### Measured complete-operation costs

Matched noisy20x20x1 and20x20x7(.8,1.2,2mm) volumes and initial masks were reused across ChanVese, ThresholdLevelSet and GeodesicActiveContour. The models, parameters and stopping differ: observed iteration counts were200 for ChanVese,30/40 for threshold and30/20 for geodesic. This is not an equal-work speedup experiment. ResetTimer excludes fixture preparation; complete admission/evolution/output is timed. The6 profiles were each repeated3times at100ms (18raw rows).

| Profile | ns/op min / median / max | iterations/op | B/op range | allocations/op range | separate process maxRSS bytes |
|---|---:|---:|---:|---:|---:|
| 2d/chan-vese | 1838397 / 1849138 / 1852755 | 200 | 37392–37392 | 24–24 | 9666560 |
| 2d/threshold | 420190 / 424136 / 432779 | 30 | 38712–38712 | 329–329 | 9781248 |
| 2d/geodesic | 224359 / 225771 / 227776 | 30 | 37080–37081 | 313–313 | 10059776 |
| 3d/chan-vese | 13326932 / 13329177 / 13347005 | 200 | 192976–192976 | 44–44 | 9879552 |
| 3d/threshold | 19356632 / 19399660 / 19600590 | 40 | 279840–279840 | 663–663 | 10092544 |
| 3d/geodesic | 2328558 / 2381342 / 2414315 | 20 | 143948–144970 | 298–299 | 9895936 |

B/op is cumulative allocation, not peak memory. Each RSS value came from a separate fresh process using the already compiled benchmark binary under `/usr/bin/time -l`, count1/benchtime1x. It includes Go runtime/test-harness/process memory; these6single observations have no repetitions or baseline-subtracted algorithm-only RSS claim. RSS-run allocation values may differ from the18 warm benchmark rows. No universal quality, timing, memory advantage or clinical qualification is inferred. Original predeclared fixtures/tolerances and all failed historical phases are retained privately.

## Mathematical references

[Chan and Vese, original method](https://pubmed.ncbi.nlm.nih.gov/18249617/),
[author-hosted variational formulation](https://www.math.ucla.edu/~lvese/PAPERS/SS99.pdf),
[ITK dense filter public contract](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1ScalarChanAndVeseDenseLevelSetImageFilter.html).
Only published mathematics and synthetic behavior inform this clean-room code.

Input mask storage is admitted before image sampling: row storage must match
Rows and relevant runs must be ordered, disjoint and within Columns. MaxWork
also charges twice the initial mask count, twice the relevant run count and
crop-row traversal before allocation. Initialization uses a linear row cursor;
output appends ordered runs without per-pixel Set rebuilding, checking context
every256columns/runs. Borrowed inputs are never modified.

Regional weighted means sum intensity differences from the first available
sample and add that anchor afterward. This is the same L2 minimizer, preserving
a constant field exactly rather than accumulating its DC offset. G2 observed
constant5 residual energy2.201e-26 due ordinary floating sums; G3 retains exact
zero-energy/mean/force assertions and adds positive/negative large/tiny constants.

## Explicit ITK reference profile correction

The first actual ITK comparison used the dense filter's implicit counter1, which applies SignedMaurer distance projection after every PDE update. Its four internal seeds remained fixed and failed the unchanged Dice0.95 criterion. The original failed receipts, default corpus and paired control are retained. ReinitializationSmoothingWeight0 does not disable that separate operation.

The declared Go model descends the regularized two-phase regional energy without periodically imposing a signed-distance constraint. The corrected external comparison explicitly uses the public ReinitializeCounter UINT_MAX. ITK still projects the first update when elapsed is zero; subsequent projection is absent within the unchanged600 steps. The two implementations belong to the same regularized regional-energy family, with different initial postprojection phi, discrete curvature/TV(H), step selection and stopping. This is neither an identical discrete equation nor proof of general mathematical or clinical equivalence. Public ITK RMS measures the PDE update before projection displacement.

A paired actual experiment preserved all eight inputs and mu0.005,nu0,lambda1/lambda2=1,epsilon=min-spacing,600iterations and RMS1e-6. Default controls reproduced the original four failures; the no-periodic profile produced Dice1.0 against the actual Go masks in every case. Negative and nonpositive masks coincide for these latter cases because final phi has no exact zeros. The adopted bridge uses public negative-inside phi<0, independently checked against GetOutput label1. The fixed0.95 acceptance test with this explicit profile subsequently passed all8 cases under defaultvet; its actual receipt is distinct from the paired diagnostic. Parameter/profile metadata is mandatory and rejected if absent. This evidence covers only the eight synthetic fixtures, alongside the independent solution/energy/gradient tests. No fixture or acceptance tolerance was retuned.
