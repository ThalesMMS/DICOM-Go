# Landmark-supervised 3D warp

`registration.FitLandmarkWarp3D` maps paired **fixed to moving** patient-space
coordinates in millimetres. It complements the existing rigid/similarity/affine
`RegisterLandmarks`; it does not change those modes. This is a reusable
nonparametric map, not a `MatrixTransform` or optimizer `Transform`. It supplies
no parameter gradient, automatic inversion, transform-file serialization,
desktop workflow, or anatomical correspondence guarantee. Swapping pairs fits
another map and does not certify its inverse. No Twin 3D presentation is added.

```go
fit, err := registration.FitLandmarkWarp3D(ctx, fixed, moving,
    registration.LandmarkWarpOptions{Regularization: 0.5})
if err != nil { return err }
mapped, err := fit.Warp.PointContext(ctx, point)
```

## Mathematical profile and physical regularization

Only the 3D order-two polyharmonic radial profile is supported, with kernel
`U(r)=-r` and an affine displacement tail. This is the sign-reversed form of
the 3D TPS `r` interpolation kernel; interpolation is equivalent with negated
weights. It is not the two-dimensional `r²log(r)` profile. The public
[kernel mathematics survey by Schaback and Wendland](https://num.math.uni-goettingen.de/schaback/teaching/KBMM.pdf)
describes conditional definiteness and polynomial augmentation; the
[ITK public TPS contract](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1ThinPlateSplineKernelTransform.html)
identifies its 3D radial profile. Implementation and analytical fixtures are
independently authored from this mathematics; no toolkit implementation is
copied or translated and no runtime peer comparison is claimed.

In physical coordinates, each displacement component solves
`(K+λI)w+Pa=d`, `Pᵀw=0`, where `Kij=-|fixed_i-fixed_j|` and
`P=[1,x,y,z]`. `λ≥0` has units **mm**, not mm² and not a dimensionless stiffness.
Zero interpolates eligible distinct knots; positive λ permits residuals and
penalizes radial coefficients, preserving an unpenalized affine component.
Using `+r` with `+λI` would not implement this smoothing convention.
The center is the fixed AABB midpoint and scale is its greatest side length.
With `q=(x-center)/scale`, the solved displacement is divided by scale and
`λnormalized=λ/scale`; output adds `scale*displacement(q)` to the input.
Scaling all physical coordinates and λ by the same positive factor preserves
the dimensionless solution. An affine mapping is reproduced for any eligible λ.

The augmented saddle-point system is indefinite. General partial-pivoted
Gauss-Jordan elimination simultaneously solves three displacement components
and computes its inverse. A pivot ≤`1e-15*||A||∞`, nonfinite arithmetic, or
condition `||A||∞*||computed inverse||∞` exceeding `MaxCondition` refuses a
usable warp. This is a numerical conditioning diagnostic of the normalized
augmented matrix, not a rigorous forward-error bound or condition of an
anatomical inverse. Successful reports include per-landmark residuals and RMS
FRE in mm. Failure returns a nil `Warp`, with diagnostics reached so far;
absence of an error is the only fit-success contract.

Matching at least four noncoplanar distinct fixed landmarks is necessary.
Exact duplicates are refused even when corresponding moving points agree and
even with regularization; conflicting duplicates are distinguished in the
error text. Insufficient sets, count mismatch, nonfinite coordinates/options,
invalid limits, coplanarity/rank loss, condition failure and numeric overflow
produce deterministic wrapped error classes. No invalid input is silently
merged, jittered, clamped or converted to an apparently successful fit.

## Derivatives, folds and extrapolation

`JacobianContext` returns the position Jacobian using the derivative of `-r`
away from its centers. At a center with nonzero radial weight the kernel has a
cusp and `ErrLandmarkWarpDerivative` is returned. A zero weight contributes no
derivative; small nonzero weights are not silently thresholded to zero.
`InspectContext` retains such samples with `JacobianDefined=false` and no fold
certification. Other evaluation errors return nil samples, including canceled
batches. Defined samples contain the Jacobian, determinant and
`NonPositiveJacobian` (`det≤0`). These are observations only at requested points:
positive samples do not prove a globally diffeomorphic map, absence of folds,
invertibility or a clinically valid correspondence.

`OutsideLandmarkBox` tests the fixed landmarks' axis-aligned bounding box.
Inside that box is not a convex-hull test, and neither region guarantees
accuracy. The radial/affine map extrapolates globally; far-field output or
Jacobian arithmetic may overflow and is then refused. Extrapolation is
inspectable, not prohibited or asserted anatomically safe.

## Resource ownership and bounds

Zero budgets select finite defaults: 128 landmarks (hard upper admission1024),
16MiB logical matrix, 16MiB scratch, 64Mi coefficient-update work units,
4096 inspection queries, 1Mi radial query interactions, 1MiB query result
payload, condition1e10. Negative limits are errors. A single point/Jacobian
query also obeys radial-interaction admission. Callers may tighten or explicitly
raise limits within the hard landmark cap; no zero value means unlimited.

For `n` landmarks and `d=n+4`, logical matrix bytes are `8d²`; logical scratch
is `8d(d+3)+104n`, accounting conservatively for the inverse/three RHS plus
normalized centers, weights and residual numeric payload. Work admission is
`d²(2d+3)`, the dense coefficient-update upper bound; it is not an instruction
counter and excludes fixed-factor norm/assembly/admission bookkeeping. Reports
are numeric payloads, not total Go heap or RSS guarantees. Inspection admits
`112*queries` bytes before allocation and `n*queries` radial pairs. Each sample
evaluates both value and derivative, so one admitted pair can be visited twice.
Single queries allocate no model-size scratch; batch results are request-owned.
The model snapshots input coordinates, keeps coefficients private and never
mutates itself, so concurrent queries require no shared cache or lock. Report
residual mutation cannot change the model. A nil context/zero model is invalid.
Cancellation is checked before admissions, during row assembly and elimination,
and every32 kernel centers during queries. Canceled fits publish no usable warp;
canceled inspections publish no partial result slice.

## Qualification and cost recipe

Synthetic analytic tests cover identity/known affine with λ0/0.1/10, alternating
cube radial coefficients with independent knot/interior/exterior values,
positive smoothing amplitude/residual and physical scale invariance; rejection
and condition failures, finite-difference derivatives, cusp disclosure,
reflection determinant/folds, AABB extrapolation, resource admissions, actual
context cancellation checkpoints and immutable concurrent queries. Existing
linear landmark and ICP regressions remain separate affected gates.

Before implementation the private plan fixed `BenchmarkLandmarkWarpCost` at
16/32/64 landmarks, fit/query cases, five repetitions and100ms per cell:

```sh
GOMAXPROCS=2 GOMEMLIMIT=1GiB go test -p1 ./registration -run '^$' \
  -bench '^BenchmarkLandmarkWarpCost$' -benchmem -benchtime=100ms -count=5
```

Fit timing includes dense allocation/solve/condition/residual work. Query
timing excludes fit and reuses an immutable model at one off-knot point.
Actual qualification on darwin/arm64, Apple M4 Pro, GOMAXPROCS2 and
GOMEMLIMIT1GiB used the above five100ms repetitions per cell (all30 rows).
Root focal22 tests (8new +14linear/ICP regressions) and the two selected race
cases passed with zero failures/skips; the3046Go source guards and four owned
postimages stayed exact, and all process groups reaped/drained normally.
This is a selective race gate, not a full-package race campaign. The initial minimum Go1.22.12 overlay
attempt stopped before any tests: its vet step could not open the absent virtual
`registration/landmark_warp.go` target. That failed receipt remains preserved;
it is not a passing minimum-version gate. After canonical source integration,
the same22 focused tests passed on Go1.22.12 with default vet enabled and no
overlay, zero failures/skips,3049 unchanged Go source guards and normal owned
process reap/drain. This establishes the selected minimum-version contract,
not a full-suite Go1.22 qualification. The subsequent combined delivery passed
33 focused tests on Go1.22.12 with default vet and no overlay, and the library
`make check` baseline passed in51.205seconds, including documentation checks.
Both delivery checks preserved6041Go source files across the two modules and
all12owned postimages. These checks preceded the documentation-only update of
this results paragraph; the final documentation check is a separate gate.
No external toolkit runtime comparison or clinical qualification is claimed.

| Operation | Landmarks | Median time (min–max), ns/op | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Fit |16|9624 (9596–9723)|8128|5|
| Fit |32|44927 (44905–45026)|23872|5|
| Fit |64|288846 (287920–291306)|85824|5|
| Point query |16|69.71 (66.96–70.25)|0|0|
| Point query |32|129.4 (128.5–130.4)|0|0|
| Point query |64|275.1 (261.7–278.7)|0|0|

Bytes and allocations were constant across all five samples in each cell.
Fit includes the allocation, dense solve, computed inverse/condition and
residuals; query reuses a fitted immutable model at one off-knot point.
These numbers measure small synthetic clouds and one query on this host,
not worst-case conditioning, batch inspection/Jacobian cost, clinical data,
RSS, or an external-baseline speedup. A zero-allocation single query does not
make fit or inspection allocation-free. Logical admissions above remain
separate conservative numeric payload bounds; allocator rounding and runtime
memory are not measured by that model. Fit storage grows quadratically and
work cubically;64landmarks here costs about30times the16landmark fit time.
Do not extrapolate these measurements to the1024hard admission ceiling or
recommend unbounded/whole-volume use. Retain deployment-specific landmark,
query and work limits, and inspect residues/conditioning/folds/extrapolation.
