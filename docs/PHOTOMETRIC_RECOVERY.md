# Native Photometric Interpretation recovery

`DecompressOptions.Recovery.RepairPhotometricInterpretation` is an explicit
compatibility profile for native, uncompressed Pixel Data from a known affected
producer. Strict decoding and the existing `DefaultImagePixelModule` profile
retain their behavior. The new profile does not run implicitly in the tolerant
codec profile, native frame extraction, or encoding.

```go
report := &pixeldata.DecompressReport{}
recovered, syntax, err := pixeldata.DecompressDataSet(dataset, sourceSyntax,
    pixeldata.DecompressOptions{
        Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true},
        Report: report,
    })
```

The input dataset and Pixel Data are borrowed read-only. The successful output
carries every corrected attribute explicitly, and `report.Diagnostics` names
each correction. The normalization does not reinterpret or rewrite native pixel
values; subsequent decompression applies its existing target byte-order policy.
Repeated recovery of the normalized output does not apply the repair again.

## Contract and diagnostics

| Declaration | Used interpretation or attribute | Diagnostic |
| --- | --- | --- |
| Missing/empty Photometric Interpretation and declared Samples per Pixel 1, 3, or 4 | MONOCHROME2, RGB, or retired ARGB, respectively | `photometric-from-samples` |
| Unrecognized nonempty Photometric Interpretation and declared Samples per Pixel 1, 3, or 4 | Same producer-selected convention | `invalid-photometric-replaced` |
| Recognized Photometric Interpretation with absent, zero, or inconsistent Samples per Pixel | One for monochrome/palette, three for RGB/YBR/HSV/XYB, four for ARGB/CMYK | `samples-per-pixel-from-photometric` |
| Planar Configuration 1 with a monochrome/palette interpretation | Zero | `ignored-planar-configuration` |
| Inferred ARGB with Planar Configuration 1 | Assumed interleaved ARGB, zero | `ignored-planar-configuration` |
| Color interpretation with absent Planar Configuration | Assumed interleaved, zero | `default-planar-configuration` |

Known Photometric Interpretation is never replaced with another color model.
An explicit valid planar RGB, YBR_FULL, ARGB, or CMYK declaration is preserved.
ARGB's historic planar form is valid; its preservation deliberately avoids
GDCM's blanket treatment of every sample count other than three as interleaved.
No additional RGB/YBR conversion or inference from pixel component values occurs.

Byte count is checked before applying a correction: the exact native size is
required, allowing one zero padding byte only when the expected length is odd.
Native 4:2:2 describes three components but stores two samples per pixel; its
width must be even and its configuration interleaved. This consistency check is
necessary, but **cannot identify the actual color model or component order**.
RGB, YBR, and HSV may have identical byte counts, as may ARGB and CMYK. Enabling
this profile explicitly selects the stated convention; callers must know it
matches the producer. Unrecognized private photometric terms are also replaced
under this explicit convention, so this profile must not be enabled for an
unknown private color encoding.

The profile refuses missing PI and sample count together, unsupported sample
counts, malformed US/CS multiplicities, PI values over 18 bytes (16 plus padding),
Planar Configuration outside 0/1, inconsistent byte counts for a corrected
layout, color samples
outside unsigned 8/16-bit containers, and compressed input. Native YBR_ICT,
YBR_RCT, YBR_PARTIAL_420, and XYB are refused; codec reconciliation remains its
separate existing profile. Recognizing HSV or CMYK for sample-count metadata does
not implement their display conversion. Signed monochrome remains supported by
existing decompression and display behavior.

Separate [explicit legacy pixel repairs](LEGACY_PIXEL_REPAIRS.md) add optional
native ARGB/CMYK/HSV normalization for known producer color conventions. This
metadata-repair option does not implicitly select that conversion.

Existing finite `DecompressLimits` apply, including input bytes, native bytes,
pixels, and frames. Metadata normalization failures return no recovered dataset
and do not add the rejected corrections to the report. Existing decompression
replaces the report after successful frame decoding; use a fresh report for each
attempt. Native data whose metadata needs no correction retains the existing
block-padding tolerance. The metadata profile does not decode compressed
codestreams, seek deferred payloads, or add a codec dependency.

## Retired ARGB display

`display.RenderColor` and `RenderColor16` support unsigned 8/16-bit ARGB, including
explicit interleaved or planar component layout. This implements the historic
DICOM ARGB semantics: alpha zero uses source RGB; a positive alpha value indexes
the RGB palette lookup tables and overrides the source RGB color. **Alpha is not
opacity**; displayed output is opaque. A positive alpha without complete RGB
palettes returns `display.ErrMissingPalette`. Zero-alpha data can be displayed
without palettes. Existing LUT index clipping and palette depth rules apply.

The frame API accepts borrowed, read-only palettes through `frame.WithPalette`
and `NativeFrame.Palette`/`EncapsulatedFrame.Palette`. `frame.ExtractFrames` reads
RGB palettes automatically for ARGB datasets. Other photometric models retain
existing extraction behavior. Direct `frame.RenderColor` can render zero-alpha
ARGB; positive alpha needs the palette-aware frame or display API. ARGB source
samples are scaled to 16-bit color; 8-bit output takes the high byte, preserving
existing palette depth behavior. This does not add ICC color management.

## Reference and validation limits

The metadata convention was compared with the local GDCM PixmapReader behavior;
implementation uses DICOM attribute contracts and independently written synthetic
fixtures. Current [PS3.3 Image Pixel Module](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.3.html)
defines sample counts and marks ARGB retired. The historic palette-override and
ARGB planar semantics are recorded in [DICOM CP-259 final text](https://dicom.nema.org/medical/dicom/final/cp259_ft.pdf).

The fixtures compare decoded bytes, effective metadata, individual display pixel
colors, diagnosis order, input immutability, and repeat-decode behavior. They
cover RGB/ARGB/mono inference, known PI sample correction, monochrome/palette
planar correction, valid planar ARGB, palette overrides, both byte orders,
8/16-bit stored samples, zero padding, typed refusals, and resource limits. They
qualify the library contracts; they are not PACS interoperability, a live desktop
session, performance measurement, or clinical image validation.
