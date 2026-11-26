# Patient-space geometry guardrails

`render.Stack.GeometryAssessment` is the single headless decision point before
MPR or VR treats DICOM frames as one volume. It does not decode Pixel Data and
does not affect `RenderFrame`; unsupported inputs therefore retain safe 2D
access.

The result is one of:

- `regular-fast-path`: regular orientation and spacing use the existing volume
  path without a copy or resample;
- `regularizable`: reversed input is sorted deterministically, while gaps,
  irregular spacing, or gantry tilt are resampled in patient LPS space only
  when a uniform-grid consumer calls `Volume.RegularGrid`;
- `unsupported`: duplicate positions without temporal identity, mixed in-plane
  orientation, inconsistent normals or gantry shear, incompatible pixel
  spacing, mixed position sources, non-orthonormal direction cosines, and
  unsplit temporal volumes fail closed with a stable `GeometryIssue`.

`GeometryFrameGroups` separates temporal positions and Stack IDs
deterministically. The caller builds one volume per returned group.

## Volume candidates of a mixed series

`Stack.VolumeCandidates` partitions a series that fails as a whole (an
embedded localizer, several orientations, pixel grids, spacings or echoes)
into coherent sub-volumes, so a viewer can still open its largest coherent
part in MPR or VR. The whole-series assessment is unchanged and still fails
closed; each candidate carries its own `GeometryAssessment`, and
`Stack.VolumeCandidateStack` returns the sub-stack to build it.

A frame joins the first candidate, in frame order, whose first frame shares:

- the orientation basis (row and column cosines within the 1 degree
  orientation tolerance, so a 180 degree flip is a different basis);
- Rows and Columns;
- pixel spacing (within the inter-slice spacing tolerance);
- the patient-position source (Image Position (Patient) or Slice Location);
- the Echo Number (0018,0086), by default;
- the time point and Stack ID of an explicit dynamic acquisition, by default;
- on request, Acquisition Number (0020,0012), Slice Thickness, and any
  caller-defined restriction key.

Frames that are not images, did not decode, or cannot be placed in patient
space (missing or non-orthonormal orientation, no position) join no
candidate. Repeated positions without echo or temporal identity are never
split into invented volumes: they stay together and the candidate reports
`duplicate-positions`. Candidates are ordered usable first (at least two
slices and not unsupported), then largest first, then by first frame. A
series that is already one volume yields one candidate with all its
renderable frames. `Description` reads like `axial 512×512, 120 slices`, with
the spacing, echo or time point appended when those differ between
candidates.

The keys follow ITK's `GDCMSeriesFileNames` with `UseSeriesDetails`
(Series Number, Sequence Name, Slice Thickness, Rows, Columns) and
`AddSeriesRestriction`, adapted to frames: Series Number is constant within
one stack, and Sequence Name is not carried by `render.Frame`. As ITK makes
series details an opt-in, Acquisition Number (which some scanners increment
on every slice) and Slice Thickness do not split unless requested. Echo
Number is read from the classic top-level attribute only; an enhanced
multi-frame echo series is split when its Stack IDs or temporal positions
separate the echoes.

## Tolerances and affine contract

Default classification records the measured spacing, orientation, normal, and
affine residual alongside these thresholds:

- inter-slice spacing: 5% relative or 0.01 mm absolute, whichever is larger;
- duplicate patient positions: 0.000001 mm;
- orientation and gantry-tilt boundary: 1 degree;
- per-interval gantry shear deviation: at most `tan(1 degree)`;
- voxel-to-patient-to-voxel absolute error: at most `1e-9`;
- Image Orientation (Patient): the length of each cosine within `1e-3` of 1
  and their dot product within `1e-3` of 0
  (`object.ImageOrientationTolerance`, the bound of GDCM's
  `DirectionCosines::IsValid` that ITK applies).

That orientation bound admits cosines written with four or more decimals.
Within it, the row cosine is normalized and the column made orthogonal to it.
`GeometryMetrics.MaximumOrientationDeviation` reports the largest departure
(`object.ImageOrientationDeviation`). Beyond it, the frame fails with
`non-orthonormal-orientation` and is not orthogonalized silently. The NIfTI
exporter and the classic multi-frame converter use the same bound, so a
series that opens in MPR also exports.

Affines are finite row-major 4x4 matrices in DICOM patient LPS millimetres.
Voxel index `(0,0,0)` is the centre of the first voxel. The final row is
`[0,0,0,1]` within `1e-12`, X changes fastest, and handedness is preserved
without implicit flips.

Regularization never rewrites acquired frames. It materializes and caches a
new modality-value grid whose bounds cover all source slice corners. MPR may
continue using exact acquired positions and per-slice origins; VR requests the
regular grid because its texture sampler requires uniform spacing.

## Frame positions and slice spacing

`Object.FrameGeometryAt(frame)` resolves, in order, the top-level Image
Plane attributes, the Shared and then the Per-Frame Functional Groups, and
reports where each value came from in `PositionSource` and
`OrientationSource` (`image-plane`, `shared-functional-groups`,
`per-frame-functional-groups`, `nm-detector-information`, `nm-slice-vector`,
`legacy-frame-increment`). A frame with a Plane Position (Patient) functional
group is never re-derived.

An NM tomographic reconstruction (Frame Increment Pointer naming Slice
Vector (0054,0080), or Image Type value 3 `RECON TOMO` / `RECON GATED TOMO`)
has no per-frame Image Plane attributes. Following PS3.3 C.8.4 (NM Image,
NM Detector and NM Reconstruction Modules) and the Frame Increment Pointer
rule that each frame takes its value from the vector the pointer names, frame
`k` is placed at:

```text
position(k) = IPP + (SliceVector[k] - SliceVector[0]) * SpacingBetweenSlices * normal
normal      = row cosine x column cosine
```

Image Position and Orientation (Patient) come from the frame's Detector
Information Sequence (0054,0022) item (the only item, or the one Detector
Vector selects) and locate the first transmitted frame; top-level values are
used only when the item lacks them. Spacing Between Slices keeps its sign: a
negative value places successive slices against the normal and is reported
as `negative-spacing-between-slices`. `PositionDerivation` records origin,
normal, offset, slice index, step and step source. Nothing is invented when a
value is missing: `slice-vector-missing`, `slice-vector-out-of-range`,
`spacing-between-slices-missing`, `nm-detector-ambiguous` and
`nm-detector-geometry-missing` are reported in `Findings` and the frame keeps
no derived position. Planar NM frames (STATIC, WHOLE BODY, DYNAMIC, TOMO
projections) are left untouched. Gated reconstructions repeat the slices per
time slot; they therefore classify as duplicate positions and stay 2D unless
a caller separates the time slots.

Recoveries are opt-in through `FrameGeometryAtWithOptions` and are marked
`Recovered`:

- `DeriveLegacyFramePositions` places a legacy multi-frame image that carries
  one top-level Image Position and Orientation: frame `k` lies `k` steps
  along the normal (Spacing Between Slices with its sign, else Slice
  Thickness) when the Frame Increment Pointer is absent, or at its Slice
  Location Vector (0018,2005) value minus the first one when the pointer
  names only that vector. A pointer to any other attribute (Frame Time,
  Frame Time Vector, ...) means the frames are not slices:
  `frame-increment-not-spatial` is reported and nothing moves. The same
  option uses frame order for an NM reconstruction without Slice Vector
  (`frame-order-as-slice-index`).
- `SliceThicknessAsSpacing` lets an NM reconstruction without Spacing
  Between Slices step by Slice Thickness (`slice-thickness-as-spacing`).

`object.ResolveSliceSpacing` is the single precedence for the spacing of a
slice when neighbouring positions cannot measure it: Spacing Between Slices
(magnitude, `Negative` reported), then Slice Thickness, then 1 mm, with a
`SliceSpacingSource`. It follows ITK's readers (commits 9467d94115,
dbd46b48bf, 296bed66ff), reimplemented rather than copied.
`FrameGeometry.SliceSpacing` applies it to a frame, and `render` uses it for
the depth of a single-slice volume, reported by `Volume.SliceSpacingSource`
(`positions` whenever two or more slices measure it). `render.Frame` and
`render.Stack` carry `SpacingBetweenSlices` beside `SliceThickness` for this.

## Pixel spacing semantics

`Object.PixelSpacingAt(frame)` returns the spacing that applies to a frame
with its source attribute, functional group source and meaning
(`PixelSpacingSemantic`), following PS3.3 10.7.1.3 by IOD family:

| Family | Attribute | Semantic |
|---|---|---|
| CT, MR, PT, NM, enhanced | Pixel Measures (per-frame, then shared), then Pixel Spacing | `patient` |
| CR, DX, MG, IO, PX, XA, RF (and Enhanced XA/XRF) | Pixel Spacing with Pixel Spacing Calibration Type | `calibrated` |
| | Pixel Spacing equal to Imager Pixel Spacing | `detector` |
| | other Pixel Spacing | `unknown` |
| | Imager Pixel Spacing (or X-Ray Frame Pixel Data Properties) | `detector` |
| RT Image | RT Image Plane Pixel Spacing | `rt-image-plane` |
| Secondary Capture | calibrated Pixel Spacing, then Nominal Scanned Pixel Spacing | `calibrated`, `scanned` |
| other | Pixel Spacing | `unknown` |

The calibration type and description and the Estimated Radiographic
Magnification Factor are reported with the result. A detector spacing is
never converted to the patient plane silently:
`PixelSpacingResolution.EstimatedPatientSpacing` divides it by the
magnification factor only when a caller asks. Ultrasound regions keep their
own calibration in package `ultrasound`.

Every spacing attribute is read by `Object.SpacingPair`, whose rules follow
ITK's reader:

- One value (`0.5`), or one followed by an empty value (`0.5\`), is
  isotropic.
- Two values are row spacing, then column spacing.
- More than two use the first two.
- A value that is not a finite positive decimal (`N/A`, `0.5abc`, `0`) fails
  with `ErrMalformedSpacing`; it is never replaced by a default.

`PixelSpacingResolution.Values` and `FrameGeometry.PixelSpacingValues` report
which of these cases applied, and `FrameGeometry.PixelSpacingErr` reports a
malformed value. Rendering treats a single value as isotropic. A stack
without any usable spacing is still laid out at 1 mm per pixel, but its
geometry carries the informational issue `missing-pixel-spacing`, which
leaves the disposition unchanged.

## Patient orientation labels

Package `object` derives the letters a viewer shows at each screen edge
without assuming an axial acquisition:

- `PatientDirectionLabel` maps an LPS direction to up to three letters in
  decreasing weight (`L`, `LA`, `LAH`). Selection is greedy, so exact ties
  (a 45 degree direction) still get a label, ordered x, y, z. Components at or
  below `1e-4` are treated as noise. `SuperiorInferior` writes `S`/`I` instead
  of the DICOM `H`/`F`.
- `ClassifyImagePlane` reports axial, coronal, sagittal or oblique using the
  conventional 0.8 direction-cosine obliquity threshold.
- `EdgeLabelsForImageOrientation` computes the top, bottom, left and right
  letters from Image Orientation (Patient) after a `DisplayTransform`
  (clockwise rotation in degrees, horizontal/vertical mirroring, and whether
  the mirror is applied before or after the rotation). Arbitrary angles yield
  compound letters.
- `EdgeLabelsForPatientOrientation` uses Patient Orientation (0020,0020) for
  projection images (DX, CR, MG); the opposite edges get the opposite letters.
  It requires a multiple of 90 degrees because the letters carry no angle.
- `MammographyPatientOrientation` supplies the conventional hanging
  orientation from View Position and laterality (CC, XCC, XCCL, XCCM, MLO, ML,
  LM) when an MG object lacks Patient Orientation.
- `Object.OrientationInfoAt(frame)` reads these attributes, including the
  per-frame Patient Orientation in Frame and Frame Laterality functional
  groups, and `OrientationInfo.EdgeLabels` applies the precedence Image
  Orientation (Patient), Patient Orientation, then (MG only) the view
  fallback. Objects with none of them get no labels.

`render.MPRPlaneImageAxes` returns the patient-space axes of an orthogonal
MPR plane. The plane names follow the volume axes (the acquired plane is
"axial" even for a sagittal stack), so labels must be computed from these
axes rather than from the name.

## Anatomical MPR views

A viewer pane named "Axial" must show axial anatomy whatever the acquisition
plane. Package `render` maps anatomy to the volume-index planes without
resampling:

- `NearestAnatomicalAxes` assigns a distinct patient axis (x left, y
  posterior, z head) and sign to each of three image axes by the greedy
  global-maximum rule of ITK's `AnatomicalOrientation`: the largest absolute
  direction cosine among all (image axis, patient axis) pairs fixes one pair,
  both are removed, and the rule repeats. Unlike an independent argmax per
  vector, two axes never collapse onto one patient axis, even for a 45 degree
  oblique stack. Exact ties resolve toward the lower image axis, then the
  lower patient axis. Zero, non-finite or coplanar axes are rejected.
- `ReorientedAxes` turns that assignment into the permutation and mirrors
  (`AxisReorientation`) that bring the axes into LPS order, the index
  bookkeeping of `itk::OrientImageFilter`.
- `ImageOrientation` is one of the eight exact symmetries of an image grid
  (transpose, then horizontal and vertical mirrors). It permutes pixels
  (`Image`), pixel-centre points and edge fractions and their inverses, image
  axes, and reslice planes (`Plane`), so a permuted image and its permuted
  plane describe the same voxels.
- `AnatomicalMPRViews(axisX, axisY)` returns the axial, coronal and sagittal
  views of a stack: the index plane whose normal the greedy assignment gives
  to that anatomy, and the `ImageOrientation` whose screen axes are nearest to
  `RadiologicalImageAxes` (axial: patient left on the right, posterior down;
  coronal: left on the right, feet down; sagittal: posterior on the right,
  feet down). A supine head-first axial stack needs no permutation; a
  sagittal stack's axial view is its row-index plane transposed. Degenerate
  axes fall back to `IndexMPRViews`, the index planes under their own names.

