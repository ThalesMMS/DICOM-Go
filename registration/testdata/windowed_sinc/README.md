# Offline windowed-sinc oracle fixtures

These are synthetic, non-DICOM images generated through the public
SimpleITK 2.5.3 API, using its ITK 5.4 backend. No ITK implementation was
read or copied. Normal Go tests consume the frozen JSON and do not require
Python or SimpleITK.

The generator records float32 source samples, float64 oracle results,
patient-space geometry, and the fixed-to-moving affine mapping. Samples
are stored with x varying fastest, then y, then z. The filter uses one work
unit, explicit output geometry, and padding -777; none of these synthetic
images can produce -777 inside the buffer, so it also records exact
inside/outside membership.

Regenerate with the pinned Python environment available during development:


For another environment, install SimpleITK 2.5.3 and check that
`Version_ITKVersionString()` is 5.4. The generator rejects a different
SimpleITK version or backend version family. Python is a development
dependency only.

The 19 input/output scenarios are each evaluated with Lanczos and Welch:
identity; impulses along x/y/z across and outside the kernel support;
constant images at fractional phases; singleton axes; an unclipped step
with ringing; an affine transform with anisotropic, oblique geometry;
ten positions around the half-voxel buffer bounds and image edges; and a
corner with border extension. The JSON contains 38 cases and 962 output
samples. Comparison casts the float64 reference to float32 and allows the
larger of 2e-5 absolute error and four adjacent float32 ULPs. Coverage,
padding, output geometry and outside counts are also checked.

The library adds two interpolators with a fixed radius of five, ten taps
per axis, and a separable sinc kernel. For distance t, Lanczos multiplies
`sinc(t)` by `sinc(t/5)`; Welch multiplies it by `1-t*t/25` within support.
The integer tap coordinates are clamped to the source samples, while the
continuous mapped coordinate is retained. Weights are not renormalized,
and output is not clipped: constant fractional samples can differ from
the constant, and edges can overshoot. The original four interpolators
retain their enum values and sampling behavior.

The input buffer is the existing half-voxel interval [-0.5, size-0.5).
Undefined or non-finite mapped points are padded. A non-finite source
sample in a visited sinc stencil, including a zero-weight tap, or a
non-finite float32 result instead returns `ErrResample` and no partial
image. This explicit failure contract is stricter than ITK's NaN output;
numerical oracle equivalence is claimed only for finite images/results.
`SourceSlices` reports the clamped z taps with non-negligible weights;
the 1e-12 threshold affects source-reference metadata only, never the
interpolated values.

`MaxOutputBytes` checks the float32 data, bool coverage and source-range
payload before mapping, B-spline prefiltering or output allocation.
Negative limits are invalid. Zero uses a 512 MiB default only for these
two new kernels; zero remains uncapped for the four existing kernels.
A positive value is an explicit cap for every interpolator. All modes
reject count and payload arithmetic overflow. This is an output payload
limit, not a process RSS, input-image, B-spline-coefficient or total CPU
budget. Cancellation checks within sinc slices/stencils return no partial
image. Work remains offline library computation, with no viewer integration.

Public references:

- [ITK windowed-sinc public class documentation](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1WindowedSincInterpolateImageFunction.html)
- [ITK Lanczos window public class documentation](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1Function_1_1LanczosWindowFunction.html)
- [ITK public window function namespace](https://docs.itk.org/projects/doxygen/en/v5.4.0/namespaceitk_1_1Function.html)
- [SimpleITK public ResampleImageFilter API](https://simpleitk.org/doxygen/v2_5/html/classitk_1_1simple_1_1ResampleImageFilter.html)
