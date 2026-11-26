# Canny edges and edge-guided level sets

The opt-in APIs in `imagefilter` and `roi` provide dense Canny edges, Shape
Detection (propagation and curvature), refinement toward Canny edges, and a
Canny edge cost for the existing livewire shortest-path solver. Twin's shared
study service dispatches these operations; both Fyne shells use the same
parameter form and service. The existing geodesic, threshold, fast-marching and
central-gradient livewire methods remain the defaults. Their enum values and
non-cancelled numerical operations are preserved. These are CPU segmentation
operations; they add no 3D rendering or fallback path.

## Canny

```go
edges, err := imagefilter.Canny(ctx, volume, imagefilter.CannyOptions{
    CannyParameters: imagefilter.CannyParameters{
        SigmaMM: imagefilter.IsotropicSigma(1),
        LowerThreshold: 5, UpperThreshold: 10,
    },
})
```

The source is a regular, dense float32 grid, with x and y at least two samples
and z either one (2D) or more (3D). Each active spacing is positive and finite.
Samples must be finite; irregular coordinates, missing data and padding holes
have no implicit interpolation. Twin requires known pixel spacing and, for 3D,
regular ordered geometry with consistent orientations and pixel spacing.
It refuses irregular/reordered/resampled inputs and unknown geometry.
Regularization must be requested separately by the caller.

Sigma is in mm; thresholds are in source intensity units/mm. Twin passes
rescaled modality intensity, independently of the display window. The zero
sigma array selects 1 mm on active axes; a nonzero array requires positive,
finite active values. In 2D the z sigma is ignored. Thresholds are nonnegative,
finite and ordered. Zero/zero links every positive peak, including noise; it
is not an automatic estimate of a clinically useful threshold.

The filter uses the existing constant-end recursive Gaussian derivatives,
physical gradient magnitude, interpolated directional nonmaximum suppression
and full 8/26-neighbour hysteresis. The physical gradient is mapped to index
space so the largest displacement is one voxel. Its largest absolute component
fixes a canonical sign; exact ties choose the earlier axis. The negative probe
uses `>=` and the positive probe `>` to choose one plateau side. Interpolation
clamps outside the grid. A positive probe clamped exactly to the current voxel
cannot be a distinct strict comparison: the inward probe supplies the strict
comparison at that border. Threshold equality is inclusive and strength must
be positive. Constant images are exactly empty. Each hysteresis voxel enters
the queue at most once.

`CannyResult.Edges` owns a 0/1 array; `Strength` owns the gradient magnitude on
retained edges and zero elsewhere. Neither array aliases the other or the
source. Caller data/options must stay stable during calls. No provider is
retained or closed.

This is an independently implemented interpolated Canny algorithm. ITK uses
a different discrete Gaussian/zero-crossing extremum procedure. This API does
not promise bitwise output equivalence to ITK.

## Shape and Canny refinement

`ShapeDetectionLevelSet` accepts intensity or an explicit edge potential
`g` in [0,1]. `Input: EdgePotentialImage` forbids `Edge` parameters; it never
interprets a supplied feature image as raw intensity. Intensity mode reuses
the existing Gaussian-gradient sigmoid; its default sigma is max(1 mm, smallest
active spacing), with alpha/beta estimated from the initialization unless
specified. Missing samples act as the existing unavailable/zero-flux boundary.

With phi negative inside, in mm, the Shape equation is

```
dphi/dt = -P*g*|grad(phi)| + C*g*kappa*|grad(phi)|
```

There is no advection term. Positive P expands, negative P shrinks. A strictly
positive small potential slows a front; it does not create an impermeable
barrier. Shape defaults to intensity input, P=1 and C=1. P has units mm per
pseudotime and C mm² per pseudotime.

`CannyLevelSet` computes edges on a dense finite cropped domain, then the
Euclidean distance D in mm to retained edge voxel centres and `a=D²/2` in mm².
Its equation is

```
dphi/dt = -P*D*|grad(phi)| + C*kappa*|grad(phi)|
          + A*grad(a).grad(phi)
```

The velocity `-A*grad(a)` attracts the front to edges. Defaults are sigma 1 mm,
P=0, C=.2, A=1; P and A have inverse pseudotime units and C mm² per pseudotime.
Pseudotime is numerical, not elapsed seconds. Seeds initialize the existing
geometric ball with unit-speed fast marching; D is not the initialization speed.
Holes/padding produce `ErrCannyDomain`, and no edges produce `ErrCannyNoEdges`.
No result or alternative algorithm is returned on error.

Both APIs reuse the existing dense narrow-band solver, crop, zero-flux
neighbours, upwind terms, CFL time step and signed-distance rebuilding. They
support explicit seed or mask initialization. Result masks are independently
owned. The source/callback/initial masks/options must be stable throughout the
call; callbacks may not race mutations.

### Curvature and stopping

The additive parameters select `CurvatureConvention`:

- `MeanCurvature` (zero/default) preserves the existing normalization: 1/r for
  a circle or sphere.
- `SumCurvature` uses the sum of principal curvatures. The coefficient is
  multiplied by dimensions minus one (one in 2D, two in 3D), checked for finite
  float32 range before image reads or allocations. The same solver/CFL rule
  then operates on that coefficient. This maps the public ITK curvature
  convention, without duplicating its engine.

`Stopping` selects `RMSOrStableMask` (default), `RMSOnly`, `MaskStabilityOnly`
or `FixedIterations`. Zero MaxIterations uses 1000; new modes cap it at 10000.
Zero MaximumRMSChange uses .005. The numerical stopping comparison is
`RMSChange < MaximumRMSChange * smallestSpacing`: MaximumRMSChange is the
existing spacing-scaled tolerance, not an absolute mm value. The reported
RMSChange is the per-step change in phi over active front samples.

New-mode `StopReason` distinguishes RMS, stable mask (two quiet windows),
iteration limit, empty active band and zero time step. `Converged` retains the
solver's structural meaning: it can be true for a stable mask, empty band or
zero time step without reaching the RMS tolerance. FixedIterations disables
RMS/mask stopping but still reports an empty band or zero time step when further
updates are impossible. No diagnostic claims RMS was reached in those cases.
Legacy APIs leave these additive diagnostics unspecified/false.

`TouchesRegionBoundary` reports the crop limit. New-mode
`TouchesVolumeBoundary` separately reports source image contact (z is inactive
in 2D). `Boundary: ReportTouches` returns these flags;
`RejectTouches` returns nil and `ErrEdgeLevelSetBoundary` after solving if either
flag is true. It never publishes partial masks. Source and crop boundaries use
zero flux; no outer voxel layer is frozen to emulate ITK.

## Livewire and Twin

`NewCannyLivewireCostMap` admits a callback source before allocating/sampling,
requires explicit positive finite pixel spacing, and applies the same Canny
parameters. Cost is `1-strength/maxStrength` on retained edges and one elsewhere.
No edges, unavailable data, nonfinite samples or cancellation return nil/error.
The existing image/length/curvature weights and Dijkstra tree/path APIs are used.
LengthWeight<0 disables length; zero uses the existing .1 default. ImageWeight
zero uses one. Nonfinite/negative image or curvature weights are refused.

Twin's `LivewireSessionOptions` selects Canny explicitly. A valid options
transition clears incompatible maps/tree/anchor; invalid options leave state
unchanged. A failed/cancelled map/tree does not replace a valid cache or anchor.
New Canny frame snapshots are captured inside the first library callback,
after its admission; frame decoding continues to use the existing reader,
codec/context and bounded pixel cache. Decoder-owned frame/cache bytes are
separate from the segmentation reservation. Session use remains serialized;
caller study data must be stable. Frontends preserve the selected mode across
series rebinds and discard incompatible pending livewire points on transitions.

Smart Contour's new shared options expose stopping, curvature convention and
boundary rejection. Both shells display sigma in mm and thresholds in rescaled
intensity/mm and report why an operation stopped. Unknown spacing, padding,
missing edges or geometry limitations surface as errors; there is no silent
switch to the old edge algorithm.

## Finite resources and cancellation

Reservations are logical bounds on owned array capacities, not measured RSS.
Caller source/initial masks, decoded frames/cache and Go runtime overhead are
excluded. Zero limits select finite defaults; new APIs allow only lower
positive overrides, and refuse negative/above-ceiling values.


New edge level sets reject spacing below twice the smallest positive float32,
source-axis physical extents above sqrt(MaxFloat32)/4 and initialization radii
above MaxFloat32/4 before sampling, so physical phi/D² fields have finite range.
These are explicit precision limits, not spacing estimates.

Checked dimensions/products precede makes. Cancellation is polled during
source validation/sampling, Gaussian causal/anticausal recurrences, NMS,
hysteresis, coordinate/distance lines, sigmoid sampling/quantiles, initialization,
mask traversal, band rebuilding/sort, rates/updates and output masks. All helpers
join before an error returns, and private partial arrays are discarded. No
background goroutine/provider lifetime is introduced. Legacy mathematical
operations and ordering are unchanged; new context polls only strengthen their
cancellation.

## Independent fixtures and public oracle limits

The authored generators use public SimpleITK 2.5.3 / ITK 5.4 APIs. They do not
copy upstream implementation or fixtures. Array geometry, signs, spacing,
thresholds and expected outputs were frozen before Go comparison. Criteria
remain: Canny bidirectional edge distance <=one maximum-spacing voxel and equal
component counts; refinement Dice>=.95 and symmetric Hausdorff<=two
maximum-spacing voxels. Raw phi/RMS/iteration counts are not parity criteria.

Public threshold sweeps characterize intensity/mm scaling. Public Canny
segmentation uses ReverseExpansionDirection=true for this negative-inside
advection convention. Its exposed Canny edges plus public Danielsson distances
with image spacing confirm the D/mm and D²/2/mm² relationship; independent
analytic plane/distance tests confirm the attraction sign. This does not claim
access to ITK private fields or bitwise distance/evolution equivalence.

The ten Canny fixtures cover constant, axis/oblique steps, ring, two components,
anisotropic steps/planes, sphere and tube. The four original refinement probes
are retained unchanged. Canny 2D/3D passes the frozen criteria. The two original
Shape probes do **not** pass: weak positive g slows rather than blocks expansion,
and dense/CFL/window stopping differs from ITK's sparse-field boundary/time-step
handling. These RED comparisons are preserved as explicit limitations:

| Original probe | Go / ITK voxels | Dice | Hausdorff mm | Go stop |
| --- | --- | --- | --- | --- |
| Shape 2D | 421 / 1521 | .433573635 | 15.556349186 | stable mask, 70 iterations |
| Shape 3D | 24381 / 19675 | .892818231 | 6 | iteration limit, 300; source boundary |
| Canny 2D | 421 / 413 | .971223022 | 1 | stable mask, 80 |
| Canny 3D | 6643 / 6214 | .961966244 | 1 | stable mask, 110 |

A separately frozen positive extension retains both original Shape failures:

| Positive anisotropic Shape probe | Go / ITK voxels | Dice | Hausdorff mm |
| --- | --- | --- | --- |
| 2D, exact g=0 barrier | 637 / 637 | 1 | 0 |
| 3D, exact g=0 barrier | 7907 / 7907 | 1 | 0 |
| 2D, curvature only, 3 iterations | 357 / 349 | .988668555 | .7 |
| 3D, curvature only, 3 iterations | 3329 / 3302 | .995928216 | .7 |

These use explicit SumCurvature with RMSOnly or FixedIterations; all four pass
unchanged criteria. Barrier cases end at the 300-iteration cap with nonzero RMS,
so matching masks are not described as numerical convergence. This supports
scoped Shape operation and explicit limits, not complete ITK parity or clinical
segmentation validation.

References: [ITK Canny API](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1CannyEdgeDetectionImageFilter.html),
[Shape Detection API](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1ShapeDetectionLevelSetImageFilter.html),
[Canny Segmentation API](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1CannySegmentationLevelSetImageFilter.html).
Local public ITK headers were pinned at be770907fc62a2ae0b1395a1f092465d7ab9b535
(Apache-2.0, NumFOCUS); only behavioral descriptions/formulas/APIs were consulted.

## Explicit interface-local Shape profile

Shape Detection additionally offers opt-in `InterfaceLocal` for dense supplied potentials. `DenseBand=0`, all existing defaults and the Canny/geodesic/threshold schedules remain unchanged. See [SHAPE_WEAK_EDGE_LEVELSETS.md](SHAPE_WEAK_EDGE_LEVELSETS.md) for domain, physical units, boundaries, logical capacity admission and the protected weak-edge acceptance gates. This profile is numerical segmentation work; it adds no desktop 3D rendering path.
