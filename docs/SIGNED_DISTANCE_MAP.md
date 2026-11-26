# Signed ROI distance maps

`VolumeMask.SignedDistanceTransform(ctx, options)` returns an owned float64
array with the mask's x-fastest ordering. `RasterMask.SignedDistanceTransform`
accepts column and row spacing in millimetres and returns the equivalent 2D
map. The zero options select negative inside, positive outside and physical
millimetres. `InsideIsPositive` reverses the sign; `SquaredDistance` selects
signed squared millimetres. Source dimensions, axes, voxels and runs must stay
stable during a call. Calls share no result or scratch arrays.

The sites are **foreground boundary voxel centres**: a set voxel is a boundary
site when any in-grid neighbour in its full 3^D neighbourhood is unset.
Diagonal neighbours count; neighbours outside the array are ignored. Boundary
values are signed zero. This discrete convention matches ITK Signed Maurer
on the qualified regular grids; it is not a continuous surface or a half-voxel
interface estimate. It also differs from the nearest-opposite-centre field
privately used by raster contour interpolation, whose behaviour is unchanged.

An entirely empty/full mask has no boundary sites and returns positive/negative
infinity with the default sign. Reversing the sign reverses those infinities.
ITK's float32 no-site sentinel is approximately sqrt(MaxFloat32); this API uses
infinity explicitly instead of inheriting that finite implementation sentinel.
No exterior background padding is invented for a full mask.

The implementation marks boundary sites and reuses the library's existing
separable Euclidean transform. Volume axes may be anisotropic or irregular,
finite and strictly ascending. They describe orthogonal independent axes;
this API does not infer a patient transform or support sheared grids. Each
axis is translated to zero before envelope arithmetic, avoiding cancellation
from a large absolute patient-coordinate origin. Translation cannot recover
detail already lost in caller float64 coordinates. Squared adjacent gaps must
remain representable and centred axes remain strictly ascending; axis spans
above sqrt(MaxFloat64/64) are refused. Float64 rounding and poorly scaled
coordinates still limit numerical precision.

## Resource and error contract

Zero limits select 67,108,864 voxels and 1 GiB managed-array reservation;
negative limits are invalid and positive limits are explicit finite bounds.
Dimension products and capacity arithmetic are checked by division before
allocation. Count/byte failures wrap `ErrSignedDistanceLimit`; malformed
dimensions, axes, raster runs and nonrepresentable geometry wrap
`ErrInvalidVolumeMask`. Cancellation returns no partial array. Nil context
means Background. Cancellation is checked during validation, construction,
between existing transform lines and while applying sign/units; an active
contextless transform line or its final square-root loop finishes its step.

With N voxels and S = columns+rows+slices, the admitted reservation is
`9*N + 520*S + 8192` bytes for VolumeMask, or
`10*N + 528*S + 8192` for RasterMask. It includes boundary booleans, float64
output, centred coordinates, the raster's dense source/axes, and all line
workspace capacities for up to sixteen transform workers across three passes.
The extra allowance covers fixed bookkeeping. Input arrays, allocator/runtime
overhead and process RSS are excluded; no measured RSS or speed claim is made.

## Numerical qualification

The checked-in corpus contains twenty synthetic 2D/3D masks, eighty option
results and 15,120 values generated independently with SimpleITK 2.5.3
(ITK 5.4). It includes blocks, a corner cavity, isolated foreground, multiple
components, checkerboards and deterministic mixed masks, with anisotropic
spacing. Both sign conventions and squared/unsquared physical distances are
compared at every voxel. The tolerance, fixed before production comparison,
is `1e-5 + 2e-6*abs(reference)`, accounting for the reference's float32 output.
The no-boundary sentinel difference is tested and documented separately.

Independent exhaustive searches cover irregular axes and all four options.
Additional cases cover 2D/3D equivalence, large translations, source/result
ownership, concurrent calls, malformed input, finite admission and cancellation.
This is local synthetic numerical qualification; it does not qualify clinical
margins, priors or arbitrary patient-coordinate data. It adds a reusable ROI
operation and no application 3D rendering path.

Reference API contracts:
[ITK Signed Maurer](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1SignedMaurerDistanceMapImageFilter.html)
and [SimpleITK Signed Maurer](https://simpleitk.org/doxygen/latest/html/classitk_1_1simple_1_1SignedMaurerDistanceMapImageFilter.html).
Only public contracts and independently generated outputs were used; no ITK
implementation was ported. Corpus provenance is in
[`roi/testdata/signed_distance/README.md`](../roi/testdata/signed_distance/README.md).
