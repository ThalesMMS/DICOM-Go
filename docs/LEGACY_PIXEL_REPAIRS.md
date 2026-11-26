# Explicit legacy pixel repairs

`pixeldata.DecodeRecovery` provides separate choices for known producer conventions.
Defaults remain strict; none is enabled by `DetectableDecodeRecovery`. Pixel
values and byte counts cannot reliably identify reversed words, byte-plane order
or wrong planar metadata. Default decoding follows declarations; callers must
diagnose unknown conventions instead of guessing from plausible colors.

```go
report := &pixeldata.DecompressReport{}
normalized, syntax, err := pixeldata.DecompressDataSetContext(ctx, dataset,
    sourceSyntax, pixeldata.DecompressOptions{
        Recovery: pixeldata.DecodeRecovery{
            NativePlanarLayout: pixeldata.NativePlanarWholeImage,
        },
        Report: report,
    })
```

Output owns native pixels and explicitly carries effective metadata. Input is
read-only. Applied choices appear in `report.Diagnostics` or
`TranscodeReport.DecodeDiagnostics`; failures publish no partial dataset/report.
Reversal and whole-image choices must not be applied again to normalized output.

## Choices and limits

| Choice | Effect | Applied diagnostic |
| --- | --- | --- |
| `NativePlanarInterleaved` | Override native Planar Configuration to zero | `native-planar-layout-overridden` |
| `NativePlanarByFrame` | Override native Planar Configuration to one; each frame contains its component planes | `native-planar-layout-overridden` |
| `NativePlanarWholeImage` | All frames of component one, then all frames of component two, etc.; reorder to per-frame planes and Planar Configuration one | `native-whole-image-planar-reordered` |
| `NativeOWByteSwap` | Reverse every byte pair of native OW, including its padding pair | `native-ow-byte-swap` |
| `RLEReversedByteSegments` | LSB-first byte segments within each component instead of MSB-first | `rle-reversed-byte-segments` |
| `WrongPaletteAttributeTags` | Complete `(0028,1111-1113)/(0028,1211-1213)` descriptors/data become `(0028,1101-1103)/(0028,1201-1203)` | `wrong-palette-attribute-tags` |
| `ConvertRetiredColorModels` | Native ARGB/CMYK/HSV become interleaved RGB | `retired-argb-to-rgb`, `retired-cmyk-additive-to-rgb`, `retired-hsv-full-range-to-rgb` |

Planar choices accept unsigned native 8/16-bit RGB, YBR_FULL, ARGB, CMYK or HSV
with matching sample counts, valid stored-bit metadata and complete geometry/
payload (plus its one-byte padding). Simple overrides already matching the
declaration need no diagnostic. Whole-image selection always reports reordering.
Unknown enum values fail. Word swapping accepts native OW with 8/16 allocated
bits and even encoded length; OB, compressed input and wider containers fail.
Swapping occurs before source-to-little-endian conversion. These assumptions
also alter conforming input when wrongly selected and cannot be detected.

RLE reversal preserves channel order and header validation. Above eight allocated
bits it changes byte significance in full-frame, region and caller-buffer paths.
Eight-bit samples need no reversal/diagnostic. Existing sample, segment-count,
geometry, resource and stream validation applies; encoders remain standard.

Palette repair requires all six aliases, PALETTE COLOR or ARGB, valid tables and
no canonical/segmented conflict. Historical large palettes can legitimately use
these alias tags, so mere presence is not proof of a mistake. Before allocation,
each descriptor is capped at six bytes, each linear channel at 131072 bytes, and
each segmented channel at 524304 bytes. Existing depth/index clipping applies.
Finite input/native-byte/frame/pixel admission precedes sample work. Copy,
reorder and color loops observe cancellation. Incomplete pixel prefixes cannot
be combined with these choices.

## Retired color conventions

Conversion requires native unsigned full-range 8/8/7 or 16/16/15 samples,
matching sample counts and explicit Planar Configuration zero/one. Output keeps
precision and carries RGB, three samples and Planar Configuration zero. Obsolete
palettes, ICC Profile and Color Space declarations are removed from converted
output. These are explicit conventions without ICC color management.

- ARGB preserves [existing display semantics](PHOTOMETRIC_RECOVERY.md#retired-argb-display):
  alpha zero uses source RGB; positive alpha indexes the palette and overrides
  RGB. Alpha is not opacity. Palette depth scales independently to output
  precision. Strict ARGB display, including mixed-depth palettes, remains intact.
- CMYK uses additive ink subtraction: `R=max(0,M-C-K)` and likewise for magenta
  and yellow, with `M=2^BitsStored-1`, without selecting a printing profile.
- HSV uses binary hue bins: `H*6/(M+1)` spans the six sectors of one turn,
  saturation is `S/M`, value retains precision, and RGB is truncated to integers.
  At full saturation HSV8 hue 85 gives `(1,255,0)`, hue 170 gives `(0,3,255)`.

Without conversion, native datasets retain their model. Default color display
refuses CMYK/HSV and missing/conflicting palettes; it already supports ARGB.

## Display and transcode

Render normalized output with existing frame/display APIs. The same choices in
`TranscodeOptions.Recovery` force pixel decoding for native-to-native and
same-syntax output, bypassing native clones, compressed pass-through and
codestream-only conversion. Standard output then decodes strictly.

Lossless path verification compares the **explicitly normalized source** with
strict output decoding. It neither compares the original malformed interpretation
nor repairs normalized output again. `TranscodeVerificationPixelExact` refers to
this requested source; selected `NativeRewrite` also applies to that comparison.
The RLE encoder accepts interleaved RGB. For planar recovery that produces Planar
Configuration one, explicitly select its existing output rewrite when targeting
RLE; native output can retain per-frame planes:

```go
interleaved := uint16(0)
options := pixeldata.TranscodeOptions{
    Recovery: pixeldata.DecodeRecovery{
        NativePlanarLayout: pixeldata.NativePlanarWholeImage,
    },
    Rewrite: &pixeldata.NativeRewrite{PlanarConfiguration: &interleaved},
    DecoderRegistry: decoders,
    EncoderRegistry: encoders,
}
```

## Fixtures and independent evidence

[Synthetic fixtures](../pixeldata/legacy_pixel_repairs_test.go) cover every choice
at 8/16 bits, both planar declarations, multi-frame planes, odd OW padding,
palettes/alias conflicts, stored samples, actual display colors, metadata,
input/report atomicity, cancellation/limits, native/same-syntax/encoded dataset
transcodes and Part 10 path verification. RLE also covers 32/64-bit monochrome,
regions and caller-buffer guard bytes.


- Positive-alpha ARGB16 with **8-bit palettes**, both layouts: DCMTK emits
  unscaled entries in RGB16 and black PNG pixels for these small entries. The
  library preserves its existing depth-scaled palette contract. These two cases
  are not qualified as matching the oracle.
- HSV-grid16: stored RGB16 and independent display of original HSV/normalized
  RGB match exactly. Reduced PNG8 differs because the library rounds normalized
  RGB and DCMTK truncates. The receipt states that scope without claiming
  identical reduced library/PNG display.

Run from the module in PowerShell:

```powershell
go test ./pixeldata ./pixeldata/rle ./pixeldata/frame ./pixeldata/display -count=1
go test -race ./pixeldata -run '^TestLegacyPixelRepairs' -count=1
$env:PATH = 'C:\msys64\ucrt64\bin;' + $env:PATH
$env:DICOMGO_LEGACY_PIXEL_INTEROP = '1'
$env:DICOMGO_LEGACY_PIXEL_EVIDENCE_DIR = 'C:\Users\user\AppData\Local\Temp\issue1315-legacy-pixel-qualification'
go test ./pixeldata -run '^TestLegacyPixelRepairsDCMTK$' -count=1 -v
```

Without opt-in/runtime the independent test skips explicitly. Planar/OW choices
have synthetic validation; no installed dcm4che or historic DCMTK `+wp` runtime
qualified them. Current DCMTK `+Mp` palette and `+br` RLE paths are exercised.
These fixtures establish library behavior without clinical datasets, live PACS
or native viewer claims. Public contracts and independently derived conversions
are used; vendor implementation code is not imported/copied.


Windows 11 Pro, amd64, Go 1.26.4, CGO enabled and MSYS2 UCRT64 GCC were used.
The current four complete pixel packages passed with
`go test -p 2 ./pixeldata ./pixeldata/rle ./pixeldata/frame ./pixeldata/display -count=1`.
After review corrected the same-syntax icon guard, the new constant-JPEG
regression rejected a newly lossy icon encoder before it could run, while the
control without repairs preserved its original encapsulated bytes. The defect
was reproduced before the fix with two encoder calls and an incorrectly
published dataset. The complete icon selector passed; its historical GDCM
fixture case skipped because `DICOMGO_GDCMDATA_ROOT` was unset.


```sh
go test -p 1 ./internal/viewer/study -run '^(TestDecodeRecoveryDecodesOverrunningRLESegment|TestConformingSliceHasNoRecoveryLabel|TestLoadNonConformantFileRequiresOptIn)$' -count=1
```


Library build, documentation and codec policy/adapter/conformance/manifest
checks passed. Twin documentation, architecture, release compliance and all
three Windows builds passed. These are library/consumer and software UI checks;
there was no clinical/PACS or native macOS/GPU qualification, and no performance
claim.

References: [PS3.5 G.2 byte segments](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_g.2.html),
[CP-259 retired color definitions](https://dicom.nema.org/medical/dicom/final/cp259_ft.pdf),
[PS3.3 Image Pixel Module](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.3.html).
