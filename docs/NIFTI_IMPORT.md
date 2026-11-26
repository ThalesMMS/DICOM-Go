# NIfTI import

Package `nifti` also reads scalar NIfTI-1 and NIfTI-2 volumes, for example AI
segmentations, and places them in the DICOM patient coordinate system (LPS)
used by `render`, `roi` and `seg`.

```go
volume, err := nifti.ReadFile(ctx, "labels.nii.gz", nifti.ReadOptions{})
if err != nil {
    return err
}
geometry := volume.Geometry          // qform/sform decision, RAS and LPS affines
slices := volume.SliceGeometries()   // one render.SliceGeometry per slice k
value := volume.Value(volume.Index(i, j, k, 0))
```

`Read` takes a single-file stream (`.nii`, or `.nii.gz`: gzip is detected from
its signature), `ReadPair` a `.hdr`/`.img` pair (either may be compressed), and
`ReadFile` chooses by extension and finds a pair's companion by base name.
`ParseHeader` decodes a header without reading voxels.

## Inputs

- NIfTI-1 (348-byte) and NIfTI-2 (540-byte) headers in either byte order,
  detected from `sizeof_hdr`. Voxels are normalized to little-endian.
- Single files (`n+1`, `n+2`) and pairs (`ni1`, `ni2`). A pair header given to
  `Read` fails with `ErrPairedFile`. A NIfTI-2 signature whose
  `\r\n\032\n` tail is damaged is reported as malformed (newline translation).
- Analyze 7.5 headers (no NIfTI magic) are refused with `ErrNotNIfTI`.
- Header extensions are skipped up to `vox_offset` and never parsed: code 2
  carries DICOM data and can hold PHI. `Header.Extended` only reports the flag.
  Free-text fields (`descrip`, `aux_file`, `intent_name`, `db_name`) are not
  exposed.
- A single file whose `vox_offset` lies inside the header (legacy writers left
  zero) is read from byte 352/544 with warning
  `vox-offset-below-header-adjusted`; a fractional or negative offset is
  malformed. Trailing bytes after the payload are ignored with a warning, but
  a gzip checksum error at the end of the stream fails the read.
- Up to four dimensions (x, y, z, t). `dim[5..7]` must be 1: vector, tensor and
  other intent payloads fail with `ErrUnsupportedDimensions`.

## Datatypes, scaling and NaN

`uint8`, `int8`, `int16`, `uint16`, `int32`, `uint32`, `float32` and `float64`
are read (`DT_*` codes 2, 256, 4, 512, 8, 768, 16, 64). Every other code
(binary, complex, RGB/RGBA, int64/uint64, float128, unknown) fails with a
`*ReadError` of code `datatype` whose `Datatype` field holds the code and which
wraps `ErrUnsupportedDatatype`. A `bitpix` that disagrees with the datatype is
tolerated (the datatype decides) with warning `bitpix-datatype-mismatch`.

`Stored` returns the on-disk value; `Value` applies `scl_slope`/`scl_inter`.
As in nifti_clib, scaling applies only when the slope is finite and non-zero,
and a non-finite intercept reads as zero; `Volume.Scaling` reports the
decision. Float NaN and infinities are returned unchanged by both accessors:
masks of statistical maps keep their meaning.

## Resource limits

`Inspect(ctx, reader, options)` applies the same single-file datatype, scaling,
geometry and resource admission before allocating voxel payloads. Its `Info`
reports the admitted payload size. Reader buffering can consume a bounded
prefix beyond the header. Inspection does **not** validate a complete payload,
extensions or the gzip checksum: `Read` must succeed before publishing a source.

`Volume.SliceImage` windows an existing file-native 2D slice directly from scaled
scalar values, without integer quantization or volume reslicing. Nonfinite
samples appear as black background while `Stored`/`Value` retain NaN/infinity.
`Volume.GPUInput` prepares one timepoint in the existing float32 modality
transport, retaining file order and the original LPS affine. Float64 samples
round to float32 only in this display representation. Nonfinite values and
float32 overflow refuse the entire input with `ErrGPUScalarProfile`; `maxBytes`
limits the allocation. The original source is unchanged. This API prepares
transport data; it does not perform CPU 3D rendering or qualify a GPU runtime.

`ReadLimits` mirrors the exporter: `MaxVoxels` (default `1 << 32`) and
`MaxPayloadBytes` (default 16 GiB, the exporter's `MaxUncompressedBytes`) are
checked from the header before any voxel is read, and `MaxHeaderBytes`
(default 64 MiB) bounds `vox_offset`. The payload is stored in 8 MiB chunks
allocated as bytes arrive, so a header that claims a huge volume cannot force a
large allocation, a truncated stream fails with `ErrTruncatedData`, and a large
volume never needs a second contiguous copy. Decompression reads only the
bytes the header requires. The context is checked between chunks.

## Geometry and qform/sform precedence

NIfTI can carry two transforms: the qform (method 2: quaternion, `pixdim` and
`qfac`, always rigid) and the sform (method 3: a general 3x4 matrix). The
reader validates both independently:

- qform: finite quaternion with `b²+c²+d² <= 1` (a zero `a` is the 180° case
  and is renormalized), finite offset and positive `pixdim[1..3]`. Rotation
  coefficients below 2⁻²² are snapped to zero, removing float32 noise from
  axis-aligned volumes.
- sform: finite rows and an invertible matrix, tested scale-free on the
  column-normalized matrix so micrometer voxels are not mistaken for singular
  ones. It is *orthonormal* when every pair of normalized columns has
  `|cos| <= 1e-4`.
- qform and sform *agree* when every column matches within `1e-4` of its length
  and the origins within `1e-3` of the smallest voxel.

With the default `XformAuto` policy the transform is chosen as follows:

| Situation | Transform used |
| --- | --- |
| Both codes 0 | Error `ErrNoSpatialTransform`; with `AllowMissingTransform`, method 1 `diag(pixdim)` at the origin (as written by the exporter's `SingleSliceImageGrid`) |
| Orthonormal sform, and no valid qform, or `sform_code` 1 (scanner), or sform agrees with qform | sform |
| Otherwise a valid qform exists | qform (warning when a differing or non-orthogonal sform is ignored) |
| Only a non-orthogonal sform, `SFormPermissive` | sform directions orthogonalized |
| Only a non-orthogonal sform | Error `ErrNonOrthogonalSForm` |

`XformQForm` and `XformSForm` force one transform and fail when it is absent
or invalid. `Geometry.Source`, `Code`, `QFormCode`, `SFormCode`,
`QFormSFormAgree` and `Warnings` record the decision.

This follows ITK's `NiftiImageIO` (the sform's float32 matrix is closer to the
original DICOM directions than the quaternion, and dcm2niix writes both with
code 1), with two deliberate differences: a valid qform wins over an sform that
would need orthogonalization, and invertibility is scale-free. VTK's
`vtkNIFTIImageReader` does not choose; it exposes both matrices and leaves the
decision to the application, as its documentation explains. nibabel prefers any
sform with a non-zero code; for files where the two disagree with a template
sform code (2 to 5) this reader keeps the scanner qform instead, because only
scanner coordinates can correspond to a DICOM Frame of Reference.

Permissive mode (ITK's `SFORM_PERMISSIVE`) replaces the normalized sform columns
by the nearest orthonormal frame (polar decomposition, handedness preserved),
keeps the translation and takes spacing from `pixdim` when it is positive.
`Geometry.SFormCorrected` and `SFormDeviationDegrees` report it; the largest
column deviation is bounded by `PermissiveMaxDeviationDegrees` (default 10°).

### Units and coordinates

`xyzt_units` meters and micrometers are converted to millimeters (an unknown
unit is read as millimeters). `Geometry.IndexToRAS` maps a zero-based voxel
index to NIfTI RAS+ world coordinates and `IndexToLPS` is the same affine
premultiplied by `diag(-1,-1,1,1)`, the inverse of the exporter's conversion.

`SliceGeometries` returns a `render.SliceGeometry` per slice: voxel index `i`
runs along `RowDir` (DICOM columns, `ColSpacing`), `j` along `ColDir` (DICOM
rows, `RowSpacing`), and the origin of slice `k` is `IndexToLPS·(0,0,k)`.
`VolumeGeometry` classifies them with `render.BuildVolumeGeometry`. A
left-handed file (`qfac = -1` or a flipped sform, `Geometry.LeftHanded`)
advances against `RowDir × ColDir`, so the render geometry reports
`ReversedInput`. A code other than scanner-anatomical is reported with
`non-scanner-coordinate-system`: such coordinates are not a DICOM patient
frame unless the caller knows they were aligned to one.

## NIfTI to SEG

`Volume.SEGDocument` converts an integer labelmap into a binary
`seg.Document` referencing the source series; pass it to `seg.Write`.

```go
document, report, err := volume.SEGDocument(nifti.SEGOptions{
    SOPInstanceUID:      sopUID,
    StudyInstanceUID:    studyUID,
    SeriesInstanceUID:   segSeriesUID,
    FrameOfReferenceUID: frameOfReferenceUID,
    References:          references, // render.SliceGeometry + seg.ReferencedImage per source plane
    Segments:            map[int]seg.Segment{1: liver, 2: spleen},
})
```

The source planes must form one regular grid (identical in-plane geometry and
a uniform slice step). The labelmap grid must be that grid up to an axis
permutation, flips and an integer offset, within `VoxelTolerance` voxels
(default 0.01) at every corner: the same grid, a crop, or a copy reoriented to
RAS or transposed maps voxel for voxel. No interpolation is performed; a
shifted, rotated or resampled labelmap fails with `ErrSEGGeometryMismatch`.
`SEGReport` returns the axis mapping, flips and offset.
`nifti.SEGReferences` builds `References` from the files of the series: one
plane per frame, with the exporter's strict geometry and the frame number of
a multi-frame image.

Every non-zero value must be a finite integer label up to 65535
(`ErrSEGInvalidLabel`) and land inside the series (`ErrSEGOutsideReference`);
zero voxels outside it are ignored. With a `Segments` map every label present
must be defined; a nil map creates one generic segment per label. Segment
numbers default to 1..n in ascending label order. The pixdim-only grid of
`AllowMissingTransform` has no patient position and is refused.

`nifti.WriteVolume` writes any `Volume` (read, or built by `NewVolume`, such
as a labelmap rebuilt from a Segmentation) as a single-file NIfTI-1 volume,
gzip-compressed or not, with its datatype, scaling and geometry. The
command-line tools `dcm2nii`, `nii2seg` and `seg2nii`
([CLI_TOOLS.md](CLI_TOOLS.md)) are built on these functions.

## NRRD and MetaImage

The `nrrd` and `metaimage` packages read into the same `nifti.Volume`
(`nifti.NewVolume` builds one from any voxel stream and an LPS affine), so
their volumes have the accessors, geometry classification and SEG conversion
described above, and write any `nifti.Volume` back. Both are implemented from
the format descriptions; no Teem, MetaIO or VTK code is used.

- **NRRD** (`NRRD0001`–`NRRD0006`, `.nrrd` or `.nhdr` with a detached data
  file): types int8 to uint32, float and double; raw, gzip, bzip2, text and
  hex encodings; little or big endian; `line skip` and `byte skip`
  (including `-1` for raw detached data). The `left-posterior-superior`,
  `right-anterior-superior`, `left-anterior-superior` and
  `right-posterior-superior` spaces (and their `LPS`/`RAS`/`LAS` aliases) are
  converted to LPS. Non-anatomical spaces (`scanner-xyz`, `3D-right-handed`)
  and files with only per-axis spacings are refused unless
  `AllowNonAnatomicalSpace` reads them as LPS with a warning. One non-spatial
  axis (Slicer segmentation layers, vector components, time) becomes the
  fourth dimension; when it is the first axis the samples are de-interleaved.
  A file with two non-spatial axes, such as ITK's 5D layout of a component
  axis and a `list` axis (time series of displacement or velocity fields,
  RGB sequences), is refused with `ErrMultipleNonSpatialAxes` (which wraps
  `ErrUnsupported`), since a volume has a single fourth dimension.
  Measurement frames are reported, not applied. What `NRRD0006` adds is
  refused: the `zrl` encoding and `SKIPLIST` (like `LIST`) data files with
  `ErrUnsupported`, and the 2D `right-up` and `right-down` spaces as
  non-anatomical.
- **3D Slicer segmentations** (`.seg.nrrd`): `SlicerSegments` reads the
  `SegmentN_` names, label values, layers, colours and terminology codes, and
  `Image.SEGDocument(layer, …)` converts one layer to a DICOM Segmentation
  through `Volume.SEGDocument` (overlapping segments live in different
  layers). `WriteSlicerSegmentation` writes an unsigned 8- or 16-bit
  labelmap back as a `.seg.nrrd`: `SegmentN_ID`, `_Name`, `_Color`,
  `_LabelValue`, `_Layer`, `_Extent` (computed inclusive voxel bounds) and
  `_Tags` (status and terminology entry), `Segmentation_MasterRepresentation`
  and `Segmentation_ReferenceImageExtentOffset`; several layers are written
  on Slicer's first, interleaved list axis. Every non-zero label must belong
  to a segment. `NewSlicerSegment` maps a DICOM segment's label, colour and
  codes.
- **MetaImage** (`.mha` inline, `.mhd` with a raw file): element types
  `MET_CHAR` to `MET_DOUBLE` with one channel, byte order, zlib compression
  (`CompressedData`), `HeaderSize` (including `-1`), `ElementSpacing` or
  `ElementSize`. As ITK writes them, `TransformMatrix` row *i* is the LPS
  direction of axis *i* and `Offset` the LPS position of the first voxel;
  a disagreeing `AnatomicalOrientation` is reported and ignored. In a
  `FileFormatVersion = 1` file the geometry is `ElementOrigin` and
  `ElementDirection` (each falling back to `Offset` or `TransformMatrix` when
  absent), as ITK's MetaImageIO reads it; `Offset` and `TransformMatrix` are
  then the SpatialObject object-to-parent transform, which is not applied and
  is reported when it is not the identity. Other versions are refused, and
  the writer always produces version 0.
- **Limits and safety:** both readers bound the header (1 MiB), voxel count
  and payload with `nifti.ReadLimits`, refuse 64-bit and multi-channel
  elements, and read a detached data file only by a relative path inside the
  header's directory (no absolute paths, `..`, lists or patterns). Stored
  values are written unscaled; a volume with NIfTI scaling is refused by the
  writers.

## Validation

- Round trips through the exporter for axial, coronal, sagittal and oblique
  volumes, gzip, and every scaling policy.
- Handcrafted third-party headers: qform only, sform only, both agreeing, both
  with different codes, forced policies, invalid transforms, `qfac = -1`,
  units, NIfTI-2, both byte orders, pairs, extensions and malformed input.
- Scenarios ported from ITK tests (assertions only, synthetic data): sform
  preferred over the lossy qform (ITK `34231b5702`), permissive non-orthogonal
  sform with its 8°/4° direction thresholds (`5cdf6ee6c9`, `71449a713c`),
  small-voxel sform invertibility (`6278c4171f`), NaN/Inf preservation
  (`0d8c703921`) and `scl_slope` precision (`a6fb755498`).
- `FuzzReadHeader` with committed seeds in
  `nifti/testdata/fuzz/FuzzReadHeader`; see [FUZZING.md](FUZZING.md).

Interpolating a labelmap onto a different DICOM grid and a viewer import
workflow are not part of this package.
