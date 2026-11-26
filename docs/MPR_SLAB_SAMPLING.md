# Scalar MPR slab sampling

`SlabSampling` applies to the scalar orthogonal, corrected gantry-tilt,
geometry-free source fallback, and oblique slab paths. Set it through
`MPRRenderOptions.Slab` for `RenderScalarSlabWithOptionsContext`, or
`MPRResliceOptions.Slab` for `ResliceObliqueSlabScalarWithOptionsContext`.
The legacy image-returning MPR APIs do not consume the new `Slab` field.

```go
options := render.MPRRenderOptions{
    Slab: render.SlabSampling{SpacingFraction: 0.5, Trapezoid: true},
}
plane, err := render.RenderScalarSlabWithOptionsContext(
    ctx, stack, render.MPRPlaneAxial, center, thickness, render.SlabAverage, options,
)
```

Zero `SpacingFraction` means 1; `Trapezoid` defaults to false. No finer
sampling default was introduced. The output dimensions, patient-space plane,
source-axis spacing, and existing clipping/center policy remain unchanged.
An orthogonal slab first determines its actual source range; the corrected
coronal/sagittal slab uses its physical row/column axis. The oblique path
retains the existing extent `(thickness-1)*SliceSpacing` along its normalized
plane normal. A one-position slab remains one sample.

For N coarse positions, the refined count is
`round((N-1)/SpacingFraction)+1`. Both endpoints stay fixed. Thus a five-position
slab at fraction 0.5 takes nine samples over the same four source intervals.
Rounding can make the effective spacing differ from the requested fraction.
`SlabSampling.SampleCount` exposes the bounded count calculation. Fractions
must be finite and in `(0,1]`, with zero as the default alias. Count overflow
and a count above `MPRLimits.MaxSlabSamples` are rejected before integer
conversion or output allocation.

Fractional coordinates use the selected nearest, linear (including the zero
interpolation value), or cubic kernel. Integer coordinates retain the original
source sample access. Fallback interpolation operates on each source frame's
rescaled float64 value; cached paths retain the volume's existing precision and
regular-grid preparation policy. In particular, gantry-tilt regularization may
already contain modality zero at an outside source position; sampling this
canonical grid does not change that prior policy. Geometry-free fallback skips
unavailable values instead.

`Trapezoid` gives the first and last slab positions half weight for `SlabAverage`
and `SlabSum`. It does not affect MIP or MinIP. Average divides by the weights
of valid samples; a missing endpoint is skipped, without turning the next valid
position into a new endpoint. A singleton has weight one. Sum remains a
weighted sum of modality samples: there is no step-size or millimeter factor,
so it is not a physical line integral. Finer spacing can change Sum's magnitude.
No CT-specific unit conversion is imposed on MR or other modality values.

Output pixel/byte limits and conservative sample-work limits use the refined
count before output allocation. These limits bound the render request; they do
not promise a process RSS ceiling or include retained source-volume caches.
Cancellation is observed between rows, pixels and samples, including weighted
and cubic slab loops. Errors return no partial `ScalarPlane`; source samples
are not modified. The resulting plane is owned and immutable as before.


Both MacStyle and WindowsStyle MPR windows offer **Standard** (default source
step) and **Fine (half step)** in their 2D slab sampling controls. This is a
window-session option; it does not alter saved defaults. It applies to ordinary
and fused GPU MPR projections, including oblique panes, through the existing
`SlabSampleSpacingMM` contract. Thickness in millimeters remains fixed.

The option enters detached render-state snapshots, image cache identities, and
oblique work admission. The shared study planner admits at most 512 GPU slab
samples. Ordinary requests use the existing `floor(thickness/spacing)+1` rule.
The fused adapter uses `ceil(thickness/targetSpacing)+1` and normalizes its
actual step to retain both physical endpoints. Fine controls halve the target
spacing; normalization can make a fused step smaller than that target. A fine
request exceeding the ceiling is refused; it is not silently truncated or
rendered on the CPU. Scalar library counts and GPU counts have the distinct
endpoint/count conventions stated above. Preview/settled output resolution
can be bounded more tightly because a fine slab requires more samples, while
patient-space field of view remains fixed.

The GPU worker does not implement trapezoid endpoint weights. The Twin control
therefore exposes spacing refinement only and makes no trapezoid claim. Volume
Rendering and volume-rendered slabs are unchanged, and all existing GPU
availability/unsupported-feature refusals remain. The headless scalar adapter
is available to tools and tests; the application does not use it as a fallback.

# Qualification and references

Synthetic tests cover all three orthogonal axes with and without patient
geometry, nearest/linear/cubic interpolation, default/explicit/fine spacing,
endpoint weights, all four scalar reductions, corrected coronal/sagittal tilt,
singletons, missing endpoints, signed big-endian pixels, anisotropic spacing,
rescale, cancellation, and work/count limits. Existing default scalar/image
oracle comparisons and checked-in VTK slab fixtures remain controls.

A one-voxel-wide synthetic vessel parallel to Y is sampled along a phase-shifted
45-degree X/Z ray. Its independent trilinear reference is
`100*(1-abs(dx))*(1-abs(dz))`. The standard peak is 41.789321881; half- and
quarter-step sampling reach 100 at the known peak position. This demonstrates
that phase case only. Sampling refinement cannot reconstruct unrecorded detail
and is not a clinical accuracy claim. Cubic interpolation may overshoot sharp
edges as documented by the existing kernel.

Twin headless tests inspect ordinary/fused GPU protocol requests, controls,
cache/snapshot changes, quota rejection and the retained CPU refusal. Mock GPU
protocol tests qualify wiring, not a native GPU device or clinical display.
Windows execution on a native Windows host is outside this qualification.

The clean-room behavioral references are VTK's public
[vtkImageReslice API documentation](https://vtk.org/doc/nightly/html/classvtkImageReslice.html)
(`SlabTrapezoidIntegration`, `SlabSliceSpacingFraction`) and DICOM's
[Image Plane Module](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.2.html)
for patient-coordinate geometry and spacing. Only public descriptions and
existing checked-in synthetic reference fixtures were used; no upstream
implementation was copied or reviewed for this change.
