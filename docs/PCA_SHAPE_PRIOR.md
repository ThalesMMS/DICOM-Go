# Research PCA shape prior

`roi.TrainPCAShapeModel`, `NewPCAShapeModel`, `PCAShapeModel.Fit` and
`Evaluate` provide an explicit, immutable research shape model.
`ShapePriorDetectionLevelSet` couples a caller-selected model, coefficients and
rigid pose to the existing DenseBand Shape Detection solver. No application UI,
patient model, automatic pose fitting or clinical confidence is introduced.

## Representation and physical contract

Version 1 [model data](../roi/pca_shape.go) declare an ID, provenance, training
sample count, the convention `negative-inside-mm`, regular grid geometry, a mean
field, unit Euclidean-L2 components and positive component standard deviations.
The approximate field is

`S(x) = M(R*x+t) + sum(c[i] * sigma[i] * P[i](R*x+t))`.

This matches ITK's documented
[PCA signed-distance representation](https://docs.itk.org/projects/doxygen/en/latest/classitk_1_1PCAShapeSignedDistanceFunction.html).
Coefficients are standard deviations. Component normalization uses the **sum**
of squares over voxels, not their mean; `sigma` is the square root of a sample
covariance eigenvalue. Training uses the `M-1` denominator and a dual `M×M` Gram
matrix, a bounded cyclic Jacobi eigensolve, descending eigenvalues, and a fixed
component sign (largest absolute entry positive). Numerically zero eigenvalues
are discarded. A requested retained rank is a maximum, not fabricated variance.

Training fields must already be registered on exactly the same declared grid.
There is no implicit pose normalization, spacing change, registration or
interpolation during training. Dimensions, positive spacing, finite origin and
right-handed orthonormal directions are explicit, including singleton z spacing.
Models/fields have finite mm samples and both an inside and outside. Imported
components must have the right length, unit L2 norms and pairwise orthogonality.
Unknown sign/unit conventions, nonfinite samples/parameters, inconsistent
geometry, nonpositive sigmas and invalid basis/rank are refused.

Model construction clones its input arrays. `Data` returns an owned snapshot for
versioned interchange; callers decoding external JSON/data must bound their own
deserialization **before** constructing the model. The library does not discover
or read a patient-model directory. Models can be shared by concurrent evaluators.
Borrowed fields, points and coefficient arrays must remain stable during each
call; the functions do not modify them or retain them in returned results.

`PCAShapePose` maps world mm to canonical model mm (`R*x+t`), rather than the
inverse convention. Only proper rigid transforms are accepted; scaling/shear
would change the distance units and are refused. Evaluation uses trilinear
interpolation. The default outside policy refuses points beyond voxel-centre
bounds; explicit `PCANearestBoundary` clamps to the nearest boundary centre and
reports the extrapolated count. Such an extrapolation, and a linear combination
of signed-distance fields, need not remain a true Euclidean SDF.

`Fit` projects a canonical-grid field on the fixed basis. It reports coefficients,
reconstruction RMS in mm and a flag when a declared residual/coefficient threshold
is exceeded. The flag is numerical mismatch with this model, **not anatomical
confidence**. Pose fitting and clinical model/corpus qualification are separate
work. Evaluation refuses coefficients beyond the finite 100-SD implementation
envelope; fitted outliers are reported rather than silently clamped.

## Coupling to evolution

```go
result, err := roi.ShapePriorDetectionLevelSet(ctx, volume, initial,
    roi.ShapeDetectionParams{Input: roi.EdgePotentialImage,
        MaxIterations: 150, Stopping: roi.FixedIterations},
    roi.LevelSetOptions{MarginMM: 100},
    roi.PCAShapePriorOptions{Model: model, Geometry: geometry,
        CoefficientsSD: coefficients, Pose: roi.IdentityPCAShapePose(), Weight: 1})
```

An enabled prior requires `DenseBand`, a finite available crop and geometry
matching both the canonical model and volume dimensions/spacing. Pose resampling
is explicit. Without the nearest-boundary policy, coverage of the full posed
domain is checked before reading image values. Holes/padding, incompatible grids
and unavailable priors are refused; there is no implicit fallback.

The fixed approximate target is clipped to the base band's `±3*max-spacing`.
Within that band the added reaction is `-weight*(phi-target)`. Its per-voxel
potential is `weight/2*(phi-target)^2`; mean potential and field RMS are reported
before/after evolution over the same crop. The timestep adds `2*weight` to the
existing harmonic denominator and includes the maximum reaction magnitude in
the half-voxel movement bound. Reinitialization and the existing boundary/stopping
rules remain in use. The prior cannot spawn a distant component outside the
evolving band or infer that an initialization is anatomically correct.

Weight zero delegates to the existing solver, preserving its complete result.
Enabled output reports model/provenance, weight, coefficients, pose, initial/final
field residual/potential, crop size and extrapolated counts. The initial clipped
field residual and coefficient thresholds can flag model mismatch; they are not
a posterior probability. The default thresholds are 1 mm and 3 SD. Energy need
not decrease for arbitrary combinations of image forces, clipping and
reinitialization. This fixed-target reaction is an independently specified
research extension; it does not claim to duplicate ITK's sparse-field schedule
or its MAP/pose optimizer.

## Bounds and lifetime

Default model limits are 1 Mi voxels, 8 components, 16 samples, 256 MiB additional
arrays and 2^28 component/work visits. Explicit hard ceilings are 4 Mi voxels,
16 components, 32 samples, 512 MiB and 2^30 visits. The eigensolve stops within
64 sweeps or returns a limit error. Constructor/training account for owned model
arrays, covariance/eigenvector matrices and constructor copies before allocation.
Caller input and runtime/RSS are outside this managed-array ledger.

Evolution retains the existing limit of 10,000 iterations. Its target and
diagnostic/parameter buffers are reserved before source reads, reducing the
solver's available byte budget; both model and level-set budgets apply. Existing
model storage is caller-owned input, not an additional allocation during
evolution. Cancellation is checked during input validation, covariance,
eigendecomposition, interpolation, fit, field preparation, diagnostics and solver
updates. A canceled call returns no final model/result. No partial mask is
published as a completed proposal.

## Numerical qualification

The redistributable [corpus and SDK probe](../roi/testdata/pca_shape/README.md)
contain project-authored analytic circles/spheres and explicitly held-out forms.

| Check | Current result |
|---|---|
| Five analytic circles, 2–6 mm, held-out 4.75 mm | Exact mean/rank-one covariance; reconstruction within 1e-12 mm |
| Eight 3D spheres, rotated anisotropic grid, six retained components | Matches independent NumPy 2.3.5 SVD/eigenvalues/reconstructions |
| Two unseen 3D spheres | RMS residuals 0.0575926 and 0.0288184 mm; below the declared 0.1-mm/3-SD thresholds |
| Unseen elongation | RMS 0.278716 mm and maximum coefficient 95.3547 SD; outside-distribution flag |
| Native ITK 5.4.6 PCA evaluation with physical pose and off-grid points | 15 comparisons, maximum delta 1.33227e-15 mm |
| Enabled prior-only DenseBand circle evolution, 150 iterations | Held-out mask Dice=1; clipped-field RMS 0.975301→0.00398836 mm |
| Enabled 3D sphere evolution on rotated anisotropic grid | Held-out Dice=0.976744; clipped-field RMS 1.38749→0.113576 mm |
| Combined image/curvature/prior rate | Matches independent finite-difference potential gradient |
| Weight zero | Complete solver result identical to the base, including metadata/diagnostics |
| Concurrency/limits/malformed fields/cancellation | Focused race gates passed; nil final output on cancellation |

The ITK oracle qualifies the PCA representation, interpolation and pose for
those cases. It is not whole-solver equivalence or clinical validation. Default
tests use frozen native results; native execution is opt-in. This Windows amd64
delivery uses CPU numerical analysis, without an application 3D rendering path.
