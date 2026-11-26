# United Imaging grid images


## Supported layout

The source must use classic MR Image Storage and have at least two Image Type
components, with `VFRAME` last. The private creator `Image Private Header` must
reserve an unambiguous block in group 0065. The block may be anywhere in 10–FF;
the implementation uses the existing creator-scoped Object lookup, not fixed
tags such as (0065,1050). It reads these logical offsets:

| Offset | Required meaning |
| --- | --- |
| 50, DS/VM1 | Positive integer slice count N |
| 14, DS/VM3 | Finite unit slice normal parallel or antiparallel to IOP row × column |
| 51, SQ | Exactly N frame items, in row-major tile order |

An implicit-VR private DS represented as bounded raw UN is interpreted locally
only after creator resolution. An already materialized UN sequence is accepted
at offset 51. Public attributes retain their declared VR requirements. This is
not generic UN recovery and does not change the source Object's VRs. Deferred,
unavailable or undecoded sequence payloads are errors.

The grid has `ceil(sqrt(N))` columns and `ceil(N/gridColumns)` rows. Rows and
Columns must divide this grid exactly, giving positive tile dimensions. The
last row may contain unused tiles, which are ignored. The pixel image must be
one native or decodable frame of 8- or 16-bit monochrome samples. Signedness,
Bits Stored, MONOCHROME1/2 and rescale metadata are retained. Enhanced functional
groups are not accepted in this classic layout.
Grid-relative overlay attributes are rejected because copying them unchanged
would attach the wrong geometry to the cropped slices.

Common Image Orientation (Patient) and positive row/column Pixel Spacing are
required. Row/column direction cosines must be unit and orthogonal within
1e-4. Every private frame item must carry a finite Image Position (Patient);
an optional item IOP or Pixel Spacing must agree with the common values. Repeated
frame geometry attributes are rejected. Output IPP comes from that exact item,
and output IOP uses its explicit value or the source's common IOP. No mosaic
corner correction, assumed slice origin or endpoint interpolation is applied.

Positions must be distinct and monotonic when projected onto row × column,
in either direction. The private normal does not reverse tile/pixel order.
Spacing Between Slices and Slice Thickness, when present, must be positive.
Each adjacent position step must agree with advertised Spacing Between Slices
within 1e-4 absolute/relative tolerance. When that attribute is absent, no
uniform through-plane spacing is invented; explicit irregular positions are
preserved. The original grid's public IPP is not used as a substitute.

The meaning of IPP, IOP and row/column Pixel Spacing, including unit orthogonal
direction cosines, follows [DICOM PS3.3 C.7.6.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.2.html).
Creator scope and dynamic block reservation follow [DICOM PS3.5 7.8](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_7.8.html).
These standards define geometry and private scope, not the vendor grid layout.

## Identity, limits and ownership

```go
outputs, report, err := multiframe.SplitUIHGrid(ctx, file, multiframe.UIHGridOptions{
    SeriesInstanceUID: derivedSeriesUID,
    SOPInstanceUIDForSlice: func(tile int) (string, error) {
        return deriveSliceUID(tile), nil
    },
    DecoderRegistry: registry,
    Limits: multiframe.DefaultLimits(),
})
```

The caller supplies a valid new Series Instance UID and unique new SOP Instance
UIDs, distinct from the source. The source Study UID and MR SOP Class remain.
Known Series/SOP UID collisions across those roles are rejected; the caller
owns global UID uniqueness beyond these source/output identities.
Image Type becomes DERIVED/SECONDARY with VFRAME removed. Source Image Sequence
references the source MR instance, and Derivation Description identifies the
split. Private attributes are removed because they describe the original grid.
The output File Meta is rebuilt to match its SOP identity and transfer syntax.

Source files and buffers are borrowed for the duration of the call; every
output owns copied metadata and pixels. Outputs neither share mutable buffers
with the source nor with one another. On error, including cancellation or a UID
callback failure, outputs are nil and the report is zero. The caller owns
successful files and closes them when finished. Object private/text lookup may
populate its caches; callers synchronize concurrent use of the same Object.

The implementation reuses `TranscodeFile` and its codec/reader limits, context
cancellation and timeout. Compressed sources require an explicit decoder
registry; missing codecs remain errors. Zero limit fields use finite
`multiframe.DefaultLimits`; negative signed limits are invalid. MaxFrames bounds
N before geometry arrays; MaxPixels bounds the source grid; MaxElements and
MaxDepth bound geometry/metadata structures. Geometry array payloads are also
bounded by MaxOutputBytes before allocation. A separate preflight counts all
repeated output structural copies, including empty SQ items, and their clone
payload bytes before per-output metadata or pixels are allocated. Aggregate
encoded Part 10 output is measured against MaxOutputBytes. These accounting
limits are not measurements of RSS.

The study loader uses a separate deterministic UID namespace for each source
grid and derives stable slice UIDs. On-disk metadata imports reopen recognized
grids with context and finite reader limits, then detach the outputs before
closing the source. Invalid recognized geometry or split errors are reported
without publishing the grid as a volume. Ordinary non-grid files retain their
existing loading path. The existing GPU-required application rendering gate is
unchanged.

## Evidence scope

GDCM `SplitGridFilter` was read as a clean-room behavioral reference for creator
offsets, rectangular tile layout and per-item IPP, with no code copied or
translated. Synthetic fixtures qualify pixels, per-slice geometry, dynamic
private blocks, identities, ownership, malformed metadata, finite limits,
compressed input, file-path loading and spatial volume construction. This does
not claim clinical vendor-corpus qualification, native Windows execution,
GPU throughput, FPS, physical input latency or a performance benchmark.
