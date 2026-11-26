# NIfTI export

Package `nifti` converts one DICOM Series/Frame of Reference into a scalar
NIfTI-1 volume. It accepts replayable sources, plans from metadata first, and
writes `.nii` or streaming `.nii.gz` in a second pass.

```go
options := nifti.DefaultOptions()
options.Compression = nifti.CompressionGZIP

report, err := nifti.Write(
    ctx,
    destination,
    nifti.NewPathSource(paths),
    options,
)
```

`NewFilesSource` borrows already-open `*object.File` values. `NewPathSource`
reopens each Part 10 file and owns those handles. A custom `Source` must be
replayable because metadata and pixels are deliberately separate passes.

## Geometry policy

The default is fail-closed. Every frame must have finite Image Position
Patient, Image Orientation Patient, and Pixel Spacing (the opt-in image-grid
single slice below is the one exception). The exporter requires a
single Series Instance UID and Frame of Reference UID, rejects QUADRUPED
coordinates, separates explicit temporal positions, and uses tighter geometry
tolerances than the viewer. A volume is ordered by projection onto the slice
normal, never by path or input order.


## Single-slice export

One frame is refused with `CodeSingleSlice` unless `Options.SingleSlice`
opts in. The output is a 3D image with `nz = 1`, so the through-plane
voxel size is recorded:

- `SingleSlicePatient` exports a frame with Image Position, Image
  Orientation (Patient) and Pixel Spacing. The third affine column is the
  slice normal (row x column cosine), and qform/sform keep scanner code 1.
- `SingleSliceImageGrid` also accepts one frame without patient geometry
  (DX, CR, scalar US, secondary capture) from a single source, which may
  then lack a Frame of Reference UID. It writes `qform_code = sform_code = 0`
  (NIfTI method 1, no quaternion or srow): the grid claims no patient
  position or orientation. In-plane `pixdim` comes from
  `object.PixelSpacingAt` (for example detector Imager Pixel Spacing, named
  in `Report.PixelSpacingSemantic` and the sidecar's
  `pixel_spacing_semantic`), or 1 mm with the warning
  `pixel-spacing-default-1mm`. The warning `no-patient-geometry` is always
  reported, and `Read` needs `ReadOptions.AllowMissingTransform`. A frame
  whose patient geometry is present but invalid still fails; it is never
  demoted to an image grid.

The through-plane `pixdim[3]` follows `object.ResolveSliceSpacing`: Spacing
Between Slices (its magnitude, when negative), then Slice Thickness, then
1 mm, the precedence ITK's DICOM readers use (commits 9467d94115 and
296bed66ff). `Report.SliceSpacingSource` and the sidecar's
`slice_spacing_source` name the source (`spacing-between-slices`,
`slice-thickness`, `default`); the latter two also add the warnings
`slice-spacing-from-slice-thickness` and `slice-spacing-default-1mm`. A
volume reports `positions`, because its spacing is always measured between
slice positions. Two or more frames, or one frame per time point of a 4D
input, keep the volume rules.

## Pixels and scaling

Scalar MONOCHROME1/2 native pixels support signed and unsigned 8, 16, and
32-bit storage, including shifted Bits Stored/High Bit and either byte order.
MONOCHROME1 is not inverted: VOI, windowing, GSPS, and other presentation
operations never enter the quantitative export path.

`ScalingPreserveUniform` keeps normalized stored integers and places one
uniform slope/intercept in the NIfTI header. `ScalingApplyFloat32` and
`ScalingApplyFloat64` apply each frame's transform exactly once. A Parametric
Map is accepted only through its validated Real World Value Mapping; coded
units and quantitative concept must remain consistent across every source, and
the calibrated payload is emitted as float64. Pixel Padding, stored NaN and
finite values outside all applicable mapping domains or whose calibration
overflows are written as NaN.
Stored ±Inf is carried as ±Inf in the float64 payload. `Read` preserves
these representations. `Report.UnmeasuredVoxels` counts each kind, and
`Report.Warnings` and the sidecar name them with
`parametric-map-padding-written-as-nan`, `parametric-map-nan-preserved`,
`parametric-map-outside-mapping-written-as-nan`,
`parametric-map-mapping-overflow-written-as-nan`, and
`parametric-map-infinite-preserved`. Invalid mapping metadata fails with
`CodePixels`.

Native path sources stream through `FrameSink` with O(one frame) working
memory. A codec that only supports whole-object decode uses the explicitly
bounded fallback `Limits.MaxInMemorySourceBytes`; an oversized source returns
`ErrLimitExceeded` instead of allocating without a ceiling. The final volume
is staged in a temporary spool so input reordering does not require holding all
voxels in RAM. Applications writing a path should use an atomic destination;
the Twin integration does so.

## Temporal volumes

4D export requires an explicit temporal position, offset, trigger, or phase for
every frame. Acquisition order alone is not a temporal dimension. Uniform
offsets or durations determine `pixdim[4]`; `TemporalSpacingSeconds` is allowed
only when metadata identifies temporal order but omits timing, and cannot
override contradictory irregular timing. Every time point must resolve to the
same spatial grid and affine.

## NRRD and MetaImage from the same plan

`Plan.Layout` describes the planned grid (dimensions, datatype, the
full-precision LPS affine, spacings and the uniform scaling) and
`Plan.WriteVoxels` runs the same validated pixel pass as `Plan.Write` without
the NIfTI header. `nrrd.WritePlan` (gzip or raw encoding) and
`metaimage.WritePlan` (single-file `.mha`, zlib or uncompressed; the
compressed stream is spooled to a temporary file so `CompressedDataSize`
precedes it) frame those voxels in the left-posterior-superior space, so the
three formats share ordering, geometry checks, resampling and limits, and a
NRRD or MetaImage file carries the affine without NIfTI's float32 rounding.
Neither format stores a value transform: a plan that preserves a non-identity
slope/intercept is refused before the pixel pass (choose an applied
floating-point policy), as is an image-grid single slice without patient
geometry. `nrrd.WriteSlicerSegmentation` writes labelmaps as 3D Slicer
segmentations (`.seg.nrrd`); see `NIFTI_IMPORT.md`.

## Provenance and PHI

NIfTI extensions are not emitted. `Report.Sidecar` is a closed, size-capped,
non-PHI JSON schema containing only dimensions, datatype, scaling policy,
reordering/resampling, coded units and quantity, timing, the slice spacing
source and pixel spacing semantic codes, and stable warnings. See
`NIFTI_EXPORT_SECURITY.md`. Pixel data may itself contain burned-in identifying
information; exporting a volume is not de-identification.

## Independent-reader smoke check

The Go suite covers header goldens, qform/sform reconstruction, voxel order,
enhanced equivalence, 4D timing, gzip, limits, cancellation, and streaming. To
also check a generated file with nibabel:

```sh
export DICOM_GO_NIFTI_INTEROP_OUTPUT=/tmp/dicom-go-nifti-smoke.nii.gz
go test ./nifti -run '^TestWriteIndependentReaderFixture$' -count=1
python3 - <<'PY'
import nibabel as nib
import numpy as np
import os

base = os.environ["DICOM_GO_NIFTI_INTEROP_OUTPUT"]
suffix = ".nii.gz" if base.lower().endswith(".nii.gz") else ".nii"
stem = base[:-len(suffix)]
c, s = np.cos(np.pi / 6), np.sin(np.pi / 6)
expected = {
    "axial": np.diag([-1.0, -2.0, 2.0, 1.0]),
    "coronal": np.array([[-1,0,0,0], [0,0,-2,0], [0,-2,0,0], [0,0,0,1.]]),
    "sagittal": np.array([[0,0,2,0], [-1,0,0,0], [0,-2,0,0], [0,0,0,1.]]),
    "oblique": np.array([[-c,2*s,0,0], [-s,-2*c,0,0], [0,0,2,0], [0,0,0,1.]]),
}
for orientation, affine in expected.items():
    path = base if orientation == "axial" else f"{stem}-{orientation}{suffix}"
    img = nib.load(path)
    assert img.shape == (2, 1, 2)
    assert int(img.header["qform_code"]) == int(img.header["sform_code"]) == 1
    np.testing.assert_allclose(img.get_qform(), affine, atol=2e-5)
    np.testing.assert_allclose(img.get_sform(), affine, atol=2e-5)
    np.testing.assert_array_equal(np.asarray(img.dataobj).reshape(-1, order="F"), [1, 2, 3, 4])
PY
```

Nibabel is an optional external verification dependency and is not required by
the normal Go build.

The package's own reader round-trips every export; see
[NIFTI_IMPORT.md](NIFTI_IMPORT.md) for reading NIfTI back into DICOM
geometry.
