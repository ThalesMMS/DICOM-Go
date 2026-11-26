# Geometric derivation of classic CT and MR images

`seriesderive.TransformImage` creates an independent DICOM instance by cropping,
resizing, flipping and rotating a classic CT or MR image. It retains the source
SOP Class, Study and Frame of Reference, assigns new Series and SOP Instance UIDs,
marks Image Type `DERIVED\SECONDARY`, and appends derivation codes, description
and an immediate Source Image reference through `derivation.Apply`. Existing
source references and derivation history remain available.

The admitted profile is deliberately explicit:

| Attribute | Supported domain |
| --- | --- |
| SOP Class | CT Image Storage or MR Image Storage, classic single frame |
| Transfer Syntax | Implicit VR Little Endian, Explicit VR Little Endian, Explicit VR Big Endian; retained in output |
| Pixels | Native 16-bit containers, one sample per pixel, MONOCHROME1 or MONOCHROME2, signed or unsigned |
| Stored bits | CT 12–16; MR 1–16; High Bit = Bits Stored − 1 |
| Image Plane | Top-level IPP VM 3, IOP VM 6, Pixel Spacing VM 2, finite values, strictly positive spacing; direction norm/dot deviation ≤ 1e-6 |
| Pixel Data | Exactly Rows × Columns × 2 bytes, including defined-length deferred native input |
| Crop | Integer rectangle entirely inside the source; no outside fill |
| Operations | Crop → resize → horizontal/vertical flips → clockwise 0/90/180/270° rotation |

Enhanced/multiframe objects, functional groups, frame increment pointers,
encapsulated/float/double/color/palette pixels, overlays (including attributes
describing embedded overlays), EOT and external pixel providers are refused.
This is not an arbitrary-SOP conversion API. Source files require consistent
File Meta information and must already satisfy the admitted CT/MR IOD checks.

```go
out, report, err := seriesderive.TransformImage(ctx, source, seriesderive.GeometricOptions{
    Crop: &seriesderive.CropRegion{Column: 10, Row: 20, Width: 256, Height: 128},
    Rows: 256, Columns: 512,
    Interpolation: seriesderive.Bilinear,
    FlipHorizontal: true,
    RotateClockwise: 90,
})
if err != nil {
    return err // out and report are both nil
}
defer out.Close()
_ = report
```

Rows and Columns specify the resized grid **before** rotation. Both zero retain
the crop dimensions. One zero derives that axis from the crop's physical aspect
ratio, rounding to the nearest positive integer; resulting physical pixel sizes
are approximately square, with the rounding discrepancy reflected in the exact
output spacing. Two positive axes allow anisotropic spacing. A one-pixel source,
crop or output axis is supported with positive spacing. The broader DICOM
singleton exception allowing zero spacing is outside this profile.

Resize preserves the physical **edges** of the cropped field of view. A resized
pixel centre at column `c` maps to source column
`crop.Column + (c + 0.5) * crop.Width / resizedColumns - 0.5`, with the corresponding
row formula. This convention puts a singleton output at the crop's centre and
assigns that axis the entire crop extent as its pixel spacing. It also makes the
centres of an upsampled border pixel extend beyond the first/last source centre;
sampling clamps to the crop's centres while the physical edge geometry remains
unchanged.

Nearest selects the larger source index on an exact half-pixel tie. Bilinear
uses the four centre samples, exact integer rational weights, and rounds the
stored result to the nearest integer with ties away from zero. Weights and
accumulation fit int64 for US dimensions and 16-bit samples. Neither method
performs antialiasing. There is no global signal/error bound: downsampling may
discard small structures, and interpolation is caller-selected processing.
Nearest preserves Pixel Padding Value/Range Limit and their sample semantics;
bilinear refuses a source with pixel padding rather than mixing padding into
valid intensity. Unused container bits are ignored when reading and cleared in
the derived pixels, using `pixeldata.SampleFormat.Reader`. Processing is in
stored units; modality rescale/LUT, VOI/window/LUT and MONOCHROME1 inversion are
not applied to the samples. Their value-domain metadata stays in the output.

The output inverse map is reported as `SourceFromOutput`. IPP is the patient
coordinate of its mapped first centre. Its column and row step vectors follow
the source mapping; their norms become column/row spacing and their normalized
directions become the first/second IOP triplets. Source direction rounding within
the admitted tolerance is folded into output spacing, preserving the source's
declared world-coordinate mapping. DS encoding introduces finite decimal
rounding; the synthetic oracle checks each resulting patient coordinate within
1e-8 mm. Flips and rotations can reverse the IOP normal, while preserving the
same physical plane and Frame of Reference.

The metadata policy updates Rows, Columns, IPP, IOP, Pixel Spacing, Image Type,
identities, provenance and File Meta. It removes source Icon Image Sequence,
Pixel Aspect Ratio, Spatial Resolution, retired Zoom Factor/Center, image/series/
plane extrema and retired Largest Monochrome Pixel Value. A present Patient
Orientation is emptied; the required IPP/IOP define direction. It retains Slice
Thickness, Spacing Between Slices and Slice Location as descriptions of the
unchanged source plane, and retains acquisition/reconstruction attributes such
as Reconstruction Diameter (which DICOM explicitly permits alongside a cropped
image). It retains calibration intent, sample-domain LUTs, rescale and lossy
compression history; an existing Lossy Image Compression `01` is never reset.
Private attributes and other unrelated metadata are copied, without interpreting
private spatial annotations or rewriting external objects that reference the
source. Applications remain responsible for those workflows.

New IDs default to random UUID-derived `core.NewUID` values. Supplied/generated
IDs must be valid and distinct between roles and from source SOP/Series/Study/
Frame of Reference/SOP Class or an existing source ancestor SOP. A consumer may
supply the same new Series UID across calls to form one derived series, with
different new SOP UIDs. The API does not append to foreign UIDs. An optional UID
generator runs only after admission and source validation; its failure/panic
publishes no result. Source provenance uses the image-processing purpose and
spatial-resampling code, and omits the optional projection-specific
`REORIENTED_ONLY` term.

Limits are normalized before decoding geometry/VM, cloning, allocating output
pixels or calling the UID generator. Defaults admit 64 Mi input/output pixels,
128 MiB Pixel Data, 256 MiB per encoded file, 100,000 elements/items/fragments and
64 sequence levels. Negative limits and depths above 256 are refused; zero
selects a finite default. Structural admission bounds traversal and values
before metadata decoding; both Part 10 clones enforce parser limits before value
allocation. Input and output pixel limits are independent. All operations are
copy-on-success: raw, sequence, private and pixel buffers in the successful
result are owned, and failures return no file or report. Keep the source open
and immutable for the duration of the call.

These are per-operation payload/structure limits, **not** an RSS or aggregate
batch budget. Peak memory includes the caller's source, strict-validator copies,
the first parsed clone, replacement pixels, the final parsed clone, metadata
overhead and garbage awaiting collection. No full encoded file buffer or decoded
source sample array is built. Callers must bound concurrent/batch work separately.
Context is checked at stage boundaries, in strict validation and between output
rows. Existing cloning and IOD routines and a supplied UID generator accept no
context; cancellation during them is observed after that step completes.

The original representation, its independently reparsed copy and final Part 10
output pass strict VR/VM and File Meta identity checks plus
`validation/iod.Validate(CheckValues: true)`. A source failure is refused before
UID generation. Successful reports expose the scoped IOD reports, including
informational/manual-condition findings; the library does not evaluate every
normative clinical/IOD condition. Synthetic complete CT/MR fixtures cover
oblique anisotropic geometry, signed/unsigned and low stored bits, all three
native syntaxes, all flip/rotation combinations, both interpolation methods,
singletons/aspect, padding, independent buffers, deferred input, refusal,
cancelled stages and concurrency. An independent scalar world-coordinate oracle
and arbitrary-precision rational sample oracle check the output and a separately
reopened Part 10 file. This is fixture qualification, not target-PACS or clinical
validation, and makes no performance claim.

The clean-room behavioral reference is the public DCMTK `dcmscale` manual
(`dcmimage/docs/dcmscale.man`): crop, size/aspect policy, interpolation and new
SOP identity. No DCMTK implementation was copied; this API defines its own
pixel-edge FOV and nearest/bilinear contract rather than claiming identical
interpolation algorithms.

Normative references: patient-space mapping and direction constraints in
[PS3.3 C.7.6.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.2.html),
spacing ordering in [PS3.3 10.7](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_10.7.html),
CT/MR pixel and acquisition attributes in
[PS3.3 C.8.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.2.html) and
[C.8.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.3.html),
provenance in [PS3.3 C.12.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.12.4.html),
stored samples in [PS3.5 chapter 8](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/chapter_8.html),
and entity identity in [PS3.5 chapter 9](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/chapter_9.html).

