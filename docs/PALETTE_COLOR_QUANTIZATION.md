# RGB8 to PALETTE COLOR

`pixeldata.QuantizePaletteColor(ctx, source, options)` builds a new Image Pixel
Module with indexed Pixel Data and one global RGB palette. It accepts native,
unsigned RGB with Samples per Pixel 3, Bits Allocated/Stored/High Bit 8/8/7,
positive Rows/Columns and explicit Planar Configuration 0 or 1. Encapsulated,
YBR, signed, alpha and higher-depth inputs need conversion to this domain first.
OB bytes are independent of byte order; little-endian OW is also accepted.
Big-endian OW is rejected so word ordering cannot silently change RGB samples.

The returned `PaletteQuantization.ImagePixel` owns new values and contains only
pixel attributes, LUTs and Pixel Data. It shares no mutable source buffers and
does not copy SOP identity, study/patient attributes, spatial metadata, ICC
profiles, supplemental/enhanced palettes or functional groups. The source must
remain unchanged while the call reads it. A nil context means Background;
cancellation is checked before work, per histogram/mapping row, per median-cut
split and before constructing the result. Errors return no partial result.

```go
quantized, err := pixeldata.QuantizePaletteColor(ctx, rgbDataset,
    pixeldata.PaletteQuantizeOptions{
        MaxColors: 256,
        Dither:    true,
        MaxPixels: 16_000_000,
        MaxRMSE:   16,
    })
// quantized.ImagePixel is a module, not a complete SOP Instance or Part 10 file.
```

Zero options mean 256 colors, no dithering, at most 16 million total pixels and
no quality rejection. MaxColors accepts 1–256. MaxPixels applies to all frames
and is checked before extracting their views or allocating the histogram/output.
MaxRMSE accepts finite nonnegative values; zero disables rejection. A positive
limit returns `ErrPaletteQuantizeQuality` when the measured result exceeds it.
Other invalid options and unsupported domains wrap `ErrPaletteQuantize`;
metadata/extraction and context errors retain their existing identities.

## Algorithm and error

When all distinct RGB triples fit MaxColors, they are retained exactly. Otherwise
all pixels contribute to a fixed histogram of 32,768 bins (5 bits per channel),
which retains each bin's population and full-resolution channel sums. Weighted
median cut splits the box with greatest weighted RGB variance on its greatest
variance axis. A box's palette entry is its rounded population-weighted mean.
The histogram does not grow with input color diversity and is not sampled.

Palette entries are sorted lexicographically. Box/axis ties, bin sort ties and
nearest-color ties have fixed rules, so input repetition produces the same LUTs
and indices. Mapping selects the least squared RGB distance; a tie selects the
first palette entry. Optional dithering applies serpentine Floyd–Steinberg
diffusion with clamped corrected samples and weights 7/16, 3/16, 5/16, 1/16.
Error state resets at every frame. Dithering can reduce banding while increasing
pixelwise error; it has no promise of lowering RMSE.

`RMSE = sqrt(sum((sourceChannel - reconstructedChannel)^2) / (3 * totalPixels))`
and `MaxAbsoluteError` compare stored RGB8 samples with reconstructed palette
RGB8, after optional dithering. These are sample-domain metrics in units 0–255,
without gamma linearization, perceptual weighting or ICC conversion. There is
no universal quality guarantee for arbitrary inputs; callers can impose MaxRMSE.
The palette search is linear in its at-most-256 entries; no throughput or memory
benchmark claim is made here.

## DICOM representation and caller responsibilities

Output is unsigned 8-bit PALETTE COLOR (Samples per Pixel 1, 8/8/7, no Planar
Configuration), with identical US descriptors `[entryCount, 0, 16]` and OW RGB
LUT data. RGB8 entries are expanded to 16 bits by multiplying by 257 and encoded
with `display.EncodeLUTData`. Indices use OB. All frames reference the same LUT;
their indices are concatenated without individual padding, and only the complete
Pixel Data value is padded to even length. Output values are little endian.
These choices follow [PS3.3 C.7.6.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.3.html),
[PS3.5 §8](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/chapter_8.html)
and [PS3.5 A.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.2.html).

A pixel module alone does not establish IOD conformance. The caller must build a
new compatible SOP Instance with appropriate identity, derivation/source
references, other required modules and metadata consistent with the transformed
pixels. Do not merge this module into an arbitrary existing instance while
retaining contradictory image attributes. Quantization can discard colors; it
does not preserve clinical metadata or choose a lossy-compression provenance
policy for the caller.

The external fixture uses classic single-frame Secondary Capture
`1.2.840.10008.5.1.4.1.1.7`, which permits this representation.
[Multi-frame True Color SC](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.8.5.4.html)
requires RGB and Samples per Pixel 3 and cannot receive this output, even with
one frame. Multiframe/global-palette unit tests prove the pixel representation,
not compatibility with any chosen multiframe IOD. The initial API does not
generate a Color Palette Storage instance, which has different LUT constraints.

## Synthetic acceptance and reproduction

Focused tests cover exact palettes including 1/256 colors, planar inputs, a
global palette across odd-length frames, final padding, ownership, determinism,
frame-local dithering, cancellation during work, limits, unsupported domains,
Part 10 round-trip and strict VR/VM/file-meta validation. Reconstruction uses
the existing `display.PaletteFromObject` and `display.RenderColor` pipeline.
The `pixeldata/frame.RenderColor` adapter rejects PALETTE COLOR because its
Metadata lacks LUTs; the display pipeline receives those LUTs explicitly.

The opt-in DCMTK oracle writes synthetic classic SC files and renders them with
`dcm2img -W -M -O -S +Pid +op`. No LUT-repair flags, scaling or VOI/modality
transformations are enabled. DCMTK and dicom-go reconstructed RGB8 must match
exactly; source-to-result RMSE must meet the fixture's declared limit.

| Fixture | Colors | Dither | RGB8 RMSE | Maximum error | Limit |
|---|---:|---|---:|---:|---:|
| Three primary colors, 3×1 (odd Pixel Data) | 3 | No | 0 | 0 | 0 |
| RGB gradient, 128×128 | 256 | No | 4.172819 | 9 | 16 |
| RGB gradient, 128×128 | 256 | Yes | 5.275019 | 15 | 16 |

DCMTK 3.7.0+ DEV was built locally from the independent DCMTK checkout, only as
a renderer oracle. No DCMTK quantizer code was read or copied. The module could
not import `render.MedianCutPalette`/GIF dithering because render depends on
pixeldata; this implementation uses its own bounded pixel-domain algorithm.
These fixtures are not an external clinical corpus or full IOD-validator proof.


