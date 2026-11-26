# MR Spectroscopy Data

`spectroscopy` reads the OF signal stream in `(5600,0020)` through the existing
`object.Object` APIs. It is independent of Pixel Data and image codecs; an object
without `(7FE0,0010)` can contain spectroscopy. This package exposes stored
values, grid dimensions and spectral axis metadata. It does not apply an FFT,
phase correction, fitting, resampling or clinical interpretation.

```go
tag := core.NewTag(0x5600, 0x0020)
file, err := object.OpenFileWithOptions(path, object.ReadFileOptions{
    MaxElementBytes: 64 << 20,
    MaxTotalBytes: 128 << 20,
    InlineValueBytesThreshold: 16 << 10,
    DeferValueTags: []core.Tag{tag},
})
if err != nil { return err }
defer file.Close()

data, err := spectroscopy.ReadContext(ctx, file.Dataset, spectroscopy.Options{
    MaxBytes: 32 << 20,
    MaxValues: 8 << 20,
})
if err != nil { return err }
point, err := data.Point(frame, voxelRow, voxelColumn, pointRow, pointColumn)
```

`Dimensions` distinguishes frames, voxel Rows/Columns `(0028,0010/0011)` and
Data Point Rows/Columns `(0028,9001/9002)`. The reader requires positive scalar
dimensions with the expected IS/US/UL VRs. OF length must match
`frames × voxelRows × voxelColumns × pointRows × pointColumns × components × 4`.
`COMPLEX` has two float32 components, real then imaginary; `REAL`, `IMAGINARY`
and `MAGNITUDE` each have one. `Point` returns the stored components, preserving
that distinction instead of inventing a missing component or phase.

Frames are the outer dimension. Each frame visits voxels left-to-right then
top-to-bottom; each voxel stores its complete spectrum before the next voxel.
The spectral grid is also row-major. `Spectrum` returns the complete component
stream of one voxel; `Point` addresses a spectral row/column. Index and mutated
layout checks return errors rather than panicking. Returned views have capacity
restricted to their length.

The result owns its `Values` buffer and axis metadata. It does not alias raw or
typed OF values in the source object. Point/spectrum views alias that result
buffer. NaN payload bits, infinities, subnormals and signed zero are preserved.
Raw and deferred values use the object's value byte order. Typed
`core.Float32Value` values are already decoded and are copied directly.

The deferred path writes `CopyValueTo` bytes directly into the sole float32
output, carrying at most three incomplete word bytes between writes. It does
not call `GetRaw` or build a second payload-sized byte buffer. Keep the file
open and stable during replay; close it after reading. Unresolved BulkDataURI,
discarded values and unavailable deferred sources cannot supply this OF stream.

`Options` has finite defaults: 64 MiB output and 16,777,216 stored float32 values.
Zero selects the default; a negative limit is invalid. Actual/header metadata
sizes, VR/value-kind compatibility, dimension products, OF byte count and limits
are checked before allocating output. These limits bound output and conversion;
they do not bound an already materialized source object or the file parser.
Configure parser limits separately and defer only the signal tag to keep small
dimension/calibration metadata materialized. Context checks occur before
allocation, during conversion and at deferred writer boundaries. An arbitrary
blocked source must cooperate with cancellation; the reader starts no goroutines
and never closes caller-owned objects/files.

## Spectral axes and coordinates

`ColumnAxis` describes points along a spectral row (sampling axis), and `RowAxis`
describes successive spectral rows (evolution axis). Signal Domain Columns
`(0028,9003)` is required; Signal Domain Rows `(0028,9235)` is required for
Data Point Rows > 1. Each domain is TIME or FREQUENCY. Frequency runs high-to-low,
while time increases.

Optional FD metadata maps in VM order: first value to Columns/sampling, second
to Rows/evolution. Spectral Width `(0018,9052)` is Hz, Transmitter Frequency
`(0018,9098)` is MHz and Chemical Shift Reference `(0018,9053)` is ppm. Missing
calibration remains nil. In a 2D grid, VM1 is accepted and leaves the corresponding
Rows calibration unavailable; it is never broadcast. VM2 in a 1D grid or more
than two values is rejected. Width and transmitter frequency must be finite and
positive; chemical shift reference must be finite. These checks do not reject
non-finite OF intensities.

The coordinate helpers use an explicit uniform-grid convention: frequency
spacing is `width / points`, and time interval is `1 / width`. These are API
sampling assumptions. The cited DICOM modules define ordering and metadata, but
do not establish which bin is the transmitter frequency or an absolute time
origin. Every helper therefore requires a finite caller-supplied
`referenceIndex`, which can be fractional or outside the sampled grid.

```go
offsetHz, err := data.ColumnAxis.FrequencyOffsetHz(pointColumn, referenceIndex)
ppm, err := data.ColumnAxis.PPM(pointColumn, referenceIndex)
```

Frequency offset is `(referenceIndex - index) × width / points`. For PPM, the
caller must identify `referenceIndex` with the transmitter frequency; the helper
then adds `offsetHz / transmitterMHz` to the chemical shift reference. Time
offset is `(index - referenceIndex) / width`. Missing calibration, wrong domain
or non-finite calculated coordinates return `ErrAxisUnavailable`. No center bin,
endpoint convention or Fourier shift is inferred.

## Validation

Synthetic tests exercise asymmetric multi-frame voxel/spectral grids, all four
representations, little/big endian and typed OF, exact IEEE bits, axis VM/order
and explicit references, malformed metadata and counts, overflow and limits,
deferred split words, replay errors/truncation/closed sources, cancellation and
ownership. The external GDCM fixture is opt-in and is read in place:

```sh
DICOMGO_SPECTROSCOPY_GDCM=/Users/user/GDCM/Testing/Data/MR_Spectroscopy_SIEMENS_OF.dcm \
    go test ./spectroscopy -run TestGDCMSpectroscopyExternal -v
```

The test does not copy or publish the fixture and logs only signal/grid
qualification fields. This reader is not a full MR Spectroscopy IOD validator,
and fixture success does not qualify clinical interpretation or PACS workflows.


Normative references consulted: DICOM PS3.3 2026d
[C.8.14.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.14.4.html),
[C.8.14.1.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.14.html)
and [PS3.5 6.2, OF](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.html).
