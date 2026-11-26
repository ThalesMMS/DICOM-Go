# Public icon generation

`iconimage.Generate(ctx, file, options)` creates an owned `core.Element` for
Icon Image Sequence (0088,0200), containing exactly one native Image Pixel
item. It borrows source values; keep the file alive and unchanged until the
call returns. Every pixel buffer, LUT and descriptor in the result is new.
Separate calls do not reuse result storage.

```go
icon, err := iconimage.Generate(ctx, file, iconimage.GenerateOptions{
    Width: 64, Height: 64,
    Window: iconimage.WindowAutomatic,
    IgnorePadding: true,
    ByteOrder: destination.ValueByteOrder(),
})
if err != nil {
    return err
}
destination.Put(icon)
```

`core.Element` does not contain byte-order metadata. `ByteOrder == nil` means
the source dataset's `ValueByteOrder()`, including when the selected pixels
are decoded to Little Endian internally. Set the option to the destination's
order when inserting into another object. Both Little and Big Endian are
supported, including word ordering of packed palette data.

## Output and representation

Width and height independently default to 64 and each accepts 1..1024. The
source's physical aspect is fitted into that canvas with black letterboxing.
The existing `PixelSpacingAt` resolver supplies spacing for the selected
frame, including functional groups; Pixel Aspect Ratio is the fallback,
followed by square source pixels. Malformed Pixel Spacing or Pixel Aspect
Ratio is an error. Output Pixel Aspect Ratio is always 1:1.

Grayscale produces MONOCHROME2. RGB, supported YBR representations and
PALETTE COLOR produce PALETTE COLOR by default; `Monochrome` reduces color
to luminance instead. Color rendering and palette reading use the existing
display primitives. The scaled color image is mapped by `image/draw` to Go's
fixed 256-color Plan9 palette, an approximation without an adaptive quantizer
or dithering. The output has three descriptors `[256, 0, 8]` and three OW
LUT Data elements, encoded by `display.EncodeLUTData`, two entries per word.

Both outputs use one unsigned sample per pixel, Bits Allocated/Stored 8,
High Bit 7, native Pixel Data OB and no Planar Configuration. Item tags are
unique and increasing. These follow [PS3.3 F.7](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.7.html),
the [Image Pixel palette attributes](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.3.html)
and [PS3.5 7.5.1 item ordering](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_7.5.html).
The public API does not produce truecolor RGB icons: F.7 permits grayscale
and palette icons. The 128x128 MONOCHROME2 requirement in
[PS3.11 B.3.3.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part11/sect_B.3.3.2.html)
is specific to the XA1K application profile.

Sources use valid 8- or 16-bit containers, Bits Stored/High Bit and signed
grayscale samples. Supported source photometrics are MONOCHROME1/2, RGB, PALETTE COLOR,
YBR_FULL, YBR_FULL_422, YBR_PARTIAL_422 and decoded full-resolution
YBR_PARTIAL_420. Color layouts and palette LUTs must be supported by
`display.RenderColor`. Packed 1-bit sources, 32-bit sources, YBR_ICT/RCT
and other photometric interpretations are refused explicitly before decode. The generator does not
apply overlays, shutters or a separate presentation state.

## Grayscale window and padding

| Option | Behavior |
| --- | --- |
| `WindowFromSource` (zero) | First frame-specific VOI LUT/window from `display.VOIAt`; automatic min/max if neither exists. |
| `WindowAutomatic` | Ignore source VOI; scan every usable modality value. |
| `WindowMinMax` | Map finite increasing `Min` and `Max` to 0 and 255 using LINEAR_EXACT. |

The stored sample reader handles container byte order, sign and High Bit.
`display.ModalityLUTAt` then applies the selected frame's Modality LUT or
Rescale Slope/Intercept. Explicit endpoints are in this **modality domain**,
including fractional rescaled values. Automatic min/max examines every
usable transformed sample; a nonmonotone LUT can place its maximum between
the stored minimum and maximum. VOI and Modality LUT errors are propagated.
Window options other than the default require a monochrome source.

Padding is compared in the **stored domain**, before modality conversion.
`IgnorePadding` reads Pixel Padding Value and its optional inclusive Range
Limit using `PixelPaddingFromObject`; `Padding` overrides them with a caller
provided stored value/range and implies exclusion. The default includes
padding. Excluded samples are painted black after grayscale presentation;
this behavior also applies with source or explicit windows. Padding options
do not apply to color indices.

An automatic constant frame maps usable samples to 128 before presentation.
MONOCHROME1 is inverted once after VOI and encoded as MONOCHROME2, so its
automatic constant value is 127. Letterboxes and excluded padding stay
black. A frame with no usable samples returns `ErrNoUsablePixels`.

## Frames, deferred data, limits and cancellation

`FrameIndex` is a pointer to a zero-based index, allowing frame zero to be
selected explicitly. Nil uses a valid one-based Representative Frame Number;
otherwise it selects `NumberOfFrames / 3` (zero for a single frame).

The existing `FrameReader` indexes Pixel Data and reads/decodes only that
frame. It supplies effective decoded color layout and Little Endian samples;
the source frame's VOI, Modality LUT, padding and spacing remain attached to
their original frame index. An explicit codec `Registry` is respected; nil
selects the built-in registry. Transfer syntax resolution stays with
`FrameReader`, including UID-only syntax values and File Meta resolution.
The generator creates a private borrowed metadata facade for display accessors.

For deferred Pixel Data, `Source` must be the same `io.ReaderAt` input used
to read the file. It must stay open and unchanged until return. The file's
recorded value locations are preserved; the source Pixel Data remains
deferred. Generate closes neither the file nor Source. Missing Source for
deferred pixels is `pixeldata.ErrFrameLayout`.

`DefaultGenerateLimits()` provides finite admission limits, also used for
zero fields of `Limits`:

| Limit | Default |
| --- | ---: |
| Number of source frames | 1024 |
| Source pixels across all frames (excluding samples) | 16,777,216 |
| Predicted native source Pixel Data bytes | 67,108,864 |
| Source Pixel Data input bytes | 67,108,864 |
| Selected compressed frame expansion ratio | 1024 |
| `MaxFragments` for the frame index | 100,000 |

Dimensions, sample format, total source frames/pixels/native bytes, input
length and requested frame index are checked before frame indexing and
decode. Materialized encapsulated input counts BOT plus fragment payload;
deferred input conservatively counts the encoded Pixel Data value, including
Item headers. Deferred fragment count is checked by FrameReader while it
indexes the source. Selected-frame decode applies FrameReader's admission
and the returned metadata is checked before source-sized rendering. These
are resource admissions, not a measured whole-process RSS guarantee.

Negative limits, invalid dimensions, frame indices and min/max return
`ErrInvalidOptions`; unsupported source representations return
`ErrUnsupportedSource` or the relevant typed pixel/display error. Resource
admission errors support `errors.Is(err, pixeldata.ErrDecompressResourceLimit)`
and describe the exceeded limit. Partial files are refused with
`object.ErrPartialFile`. No partial element is returned on error.

A nil context means Background. Context is checked at entry, around reader
stages and `Source.ReadAt` calls, and during statistics, grayscale mapping,
resize and palette rows. Decode preserves the registry's `ContextCodec`
dispatch and the decode context options. An underlying `ReadAt` or a legacy
codec call already in progress finishes before its cancellation check;
bounded display color rendering is checked before and after the primitive.

## DICOMDIR integration and validation

DICOMDIR delegates to Generate with its existing 64/default or 128/profile
dimensions, monochrome output, declared padding exclusion and Little Endian
destination order. It passes the existing `DefaultDecompressLimits` to retain
compressed-source admission: 100,000 frames, 268,435,456 total source pixels,
536,870,912 native/input bytes and expansion ratio 1024. Unlike the former
native path, native sources now receive these finite bounds too. Source
failure still aborts unless `BlankIconOnFailure`
is explicitly requested; that option keeps its black image and diagnostic.
Cancellation is propagated even with the blank fallback enabled.

Focused tests cover modality/VOI LUTs, nonmonotone modality range, fractional
and negative rescale, stored signed padding/range/High Bit, constants,
MONOCHROME1, physical aspect, frame selection and per-frame transforms.
RGB and palette output are read and serialized as Part 10 in both byte orders,
with ordered tags asserted before serialization. A controlled multiframe
codec proves one selected-frame decode and borrowed source immutability;
native and compressed deferred fixtures prove Source use and admission before
ReadAt. DICOMDIR tests compare the public output with its written/read sequence
for Little Endian, Big Endian and JPEG Lossless sources, plus failure fallback
and cancellation.

