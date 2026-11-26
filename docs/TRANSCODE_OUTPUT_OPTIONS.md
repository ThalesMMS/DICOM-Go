# Transcode output options

`pixeldata.TranscodeOptions` configures top-level output layout and identity in
`TranscodeDataSet`, `TranscodeCoreDataSet`, `TranscodeFile` and `TranscodePath`.
Zero values retain the previous behavior: native copies keep their layout,
decoders supply native output layout, equivalent encapsulated copies retain
their stored Items, new encodes use one Fragment per frame with automatic
offset tables, and new lossy generations receive new SOP Instance UIDs.

| API | `dcmconv` | Behavior |
|---|---|---|
| `MaxFragmentBytes` | `-fragment-bytes N` | Maximum encapsulated Item payload bytes, including even padding; zero keeps the default. Positive values must be even, at least 2, and at most 4294967294. |
| `OffsetTable` (`object.OffsetTablePolicy`) | `-offset-table auto\|basic\|extended\|empty` | Automatic BOT/EOT, required populated BOT, required EOT, or empty BOT without EOT. |
| `PlanarOutput` | `-planar-output default\|iod\|interleaved\|planar` | Layout of native color output; explicit policies reject encapsulated targets. |
| `SecondaryCapture` | `-secondary-capture` | Convert a compatible classic single-frame image to Secondary Capture with a new SOP Instance UID. |
| `UIDPolicy` | `-uid-policy default\|always\|preserve` | Preserve the previous identity policy, always create a new identity, or refuse operations requiring new identity. |
| `CompressIcons` (existing) | `-compress-icons` | Encode icons with the lossless target codec; default icons stay native. |

## Fragments and tables

The Item carrying the Basic Offset Table is always present. `OffsetTableEmpty`
writes it with zero length and removes old Extended Offset Table attributes.
`OffsetTableBasic` writes one 32-bit offset for each frame and rejects overflow.
Offsets point to the first Item Tag of each frame, relative to the first Item
after the BOT, and include Item headers and even padding. Only a frame's final
Fragment may need padding; intermediate Fragment boundaries add no bytes to
the codestream. A frame never shares a Fragment with another frame.

For caps of at least 4 bytes, a terminal payload of one/two bytes is avoided
by moving two bytes from the penultimate Fragment to the last. The preceding
Item stays even, the cap still includes final padding, and Item counts and
offsets stay identical. This keeps a JPEG-family end marker and optional old
tail padding together for readers inspecting terminal Items. A cap of 2 bytes
has no room for that adjustment. Readers may also require the JPEG header in
the first Fragment; such peers require a sufficiently large cap. Normative
validity of arbitrary small fragments does not guarantee their support by
every codec/version.

`OffsetTableExtended` requires one Fragment per frame and writes an empty BOT,
Extended Offset Table and Extended Offset Table Lengths. The lengths exclude
tail padding. Automatic tables use a populated BOT while offsets fit 32 bits,
then EOT when every frame occupies one Fragment. Automatic overflow with split
frames is refused; select an empty BOT explicitly for such a representation.
EOT with split frames is refused. RLE, Deflated Image Frame Compression,
Encapsulated Uncompressed, HTJ2K and JPEG XL require one Fragment per frame: a
cap smaller than a frame's padded length is refused, rather than splitting that
frame.

An explicit nonzero fragment cap or non-default table policy repacks an
equivalent-syntax copy by indexing existing codestreams. This needs no codec,
does not create another lossy generation, and retains the old identity unless
the caller requests `TranscodeUIDAlways`. The automatic zero policy alone
continues to preserve the existing equivalent copy. Old BOT/EOT/Total Length
attributes are removed or recomputed when repacking.

`object.WriteFileFrames` exposes the same `MaxFragmentBytes` policy and an
optional `MaxFragments` cap (zero leaves that generic writer uncapped). It
inserts Item headers while `FrameSource.WriteFrameTo` supplies chunks, without
building another complete frame. Transcode always supplies its finite
`TranscodeLimits.MaxFragments` bound. Explicit encapsulation options reject
native targets.

## Native sample layout

`PlanarOutputInterleaved` selects Planar Configuration 0;
`PlanarOutputPlanar` selects 1. These policies copy complete sample bytes of
full-resolution RGB or YBR_FULL with Bits Allocated 8 or 16, including native
Big Endian output. They preserve stored values and introduce no color-space
conversion or resampling. Single-sample images are not reordered, and the
policy introduces no Planar Configuration attribute for them. Subsampled and
other color representations are refused explicitly.

`PlanarOutputIOD` selects 1 for retired Hardcopy Color Image Storage
(`1.2.840.10008.5.1.1.30`) and YBR_FULL under the 1996 Ultrasound Image and
Ultrasound Multi-frame Image Storage UIDs (`...6.1`, `...3.1`). Other admitted
native color images use 0. Plane order for native ultrasound YBR_FULL is a
compatibility choice: current PS3.3 C.8.5.6.1.16 allows either 0 or 1 for native
output, while specifying 1 for RLE. The opt-in policy does not change codec
metadata under an encapsulated target. `Rewrite.PlanarConfiguration` remains
available for the existing whole-object rewrite workflow; `PlanarOutput` is
the final native-output policy when both are supplied.

The native streaming route reorders individual frames, including its serial
decode-on-write route and bounded parallel spool route. Its conservative
in-flight charge reserves one additional native frame per admitted task for
this step. Lossless comparisons normalize admitted planar RGB/YBR_FULL to
pixel order before comparing sample bytes; tolerances remain unchanged.

## Identity and Secondary Capture

`TranscodeUIDDefault` retains copy/lossless/decompression identity and the
existing mandatory new UID for a new lossy generation. `TranscodeUIDAlways`
creates a new UID even on native/equivalent-copy fast paths, records a Source
Image reference when the source has valid SOP identity, and synchronizes File
Meta. It does not generate another UID after a lossy encoder already did so.
`TranscodeUIDPreserve` refuses a new lossy encode or Secondary Capture
conversion. Decompressing or repacking an existing lossy image preserves its
identity and lossy history under the default/preserve policies.

Secondary Capture supports catalogued classic single-frame Image Storage
IODs with valid source SOP Class/Instance and Study/Series UIDs. Enhanced,
multi-frame, functional-group, unknown/private and non-image IODs are refused.
Admitted pixels are monochrome or palette with Bits Allocated 8/16, or unsigned
8-bit RGB/YBR_FULL/YBR_FULL_422 color. The source must already have valid pixel
metadata; palette channels must have matching descriptors and supported
8/16-bit entries. Missing or invalid palettes are refused. This is a bounded conversion of an
existing image; it is not a general IOD validator or multi-frame SC builder.

The output uses Secondary Capture Image Storage (`...1.1.7`), Conversion Type
WSD, Image Type DERIVED/SECONDARY, a new SOP Instance UID, a reference to the
original source, and empty missing Type 2 patient/study/image attributes.
Study/Series identity and existing values are retained. Number of Frames and
Frame Increment Pointer are removed. File Meta SOP Class/Instance UIDs match
the output. Unsupported inputs fail before path publication. SC and main-image
UID/layout policies do not reinterpret an independently described icon.

## Bounds and verification

Existing finite transcode limits, cancellation, staged structural readback,
lossy-error verification and atomic publication still apply. Fragment counts
are checked before splitting or writing their payloads. Same-syntax streaming
repack retains at most one assembled frame and checks its raw byte length
against `Pipeline.MaxInFlightBytes` before reading it (zero: 256 MiB); no decode
workspace is charged on that route. Whole-object APIs retain their documented
whole-object allocation model. Managed frame charges are not process RSS caps.

Invalid policies and unsupported layout/SC/UID combinations wrap
`ErrTranscodeUnsupported`; exceeded transcode fragment/frame/byte budgets wrap
`ErrTranscodeResourceLimit`. Generic frame-writer input/layout errors wrap
`object.ErrFrameSource`. Decoder/encoder registries and availability errors
retain their existing explicit contract. A failed path operation leaves the
previous destination authoritative and cleans private staged outputs.

## References and qualification

Normative layout rules: [PS3.5 A.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.4.html),
[RLE A.4.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.4.2.html),
[JPEG 2000 and HTJ2K A.4.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.4.4.html),
[Encapsulated Uncompressed A.4.11](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.4.11.html),
[JPEG XL A.4.12](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.4.12.html),
[Deflated Frame A.4.13](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.4.13.html)
and [PS3.3 C.7.6.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.3.html).
SC references: [PS3.3 A.8](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.8.html),
[C.8.6](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.6.html)
and [C.8.6.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.6.2.html).
Planar references: [US C.8.5.6](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.5.6.html)
and [retired Hardcopy Color, Supplement 24](https://dicom.nema.org/medical/dicom/Final/sup24_f3.pdf).

Tests cover independent offset/sample oracles, materialized/serial/parallel
equality, Big Endian sample order, same-syntax repacking without registries,
identity/meta/predecessor consistency, icon isolation, invalid options, finite
fragment/in-flight admission and refusal without publication. External reader
qualification is recorded separately; it is fixture acceptance, not a claim of
complete IOD conformance, live-PACS interoperability or native Windows testing.

The local qualification used DCMTK `dcm2img` 3.7.0+ DEV (arm64 Darwin, IJG 6b
and CharLS 1.0) and GDCM 3.3.0 `gdcminfo --md5sum`/`gdcmconv --raw`.
DCMTK used `+Fa +op`, so every frame was rendered and compared to its native
source control. GDCM native stored samples were compared separately using an
independent sample/plane/byte-order oracle; successful raw outputs were also
rendered through DCMTK. Dictionary/resource paths were explicitly configured.

| Synthetic output | DCMTK | GDCM |
|---|---|---|
| JPEG Baseline, 128×128, cap 1024, populated/empty BOT, 24 Fragments | Accepted; maximum sample error 1 with bound 8 | Accepted; maximum stored-sample error 1 with bound 8 |
| JPEG-LS single-frame, cap 32, empty BOT, 6 Fragments | Exact pixels | Exact stored samples |
| JPEG-LS three frames, EOT, one Fragment per frame | All three exact | All three exact |
| JPEG-LS three frames, cap 32, populated/empty BOT, 18 Fragments | All three exact | Decompression refused |
| Equivalent JPEG-LS repack, three frames, cap 16, empty BOT, 36 Fragments | All three exact; joined codestreams preserved | Decompression refused |
| RLE RGB, empty BOT, one Fragment | Exact pixels | Exact stored samples |
| Native RGB planar/interleaved and US YBR_FULL IOD policy | Exact rendered pixels | Exact stored samples |
| Native new UID and single-frame SC monochrome 16-bit/RGB/palette | Accepted; exact rendered pixels | Accepted; exact stored samples |
| JPEG-LS with existing CompressIcons enabled | Main image exact | Main stored samples exact |

JPEG Baseline with a cap of 64 split the initial header and was refused by
both readers; this GDCM build terminated with a codec exception on that input.
Unsplit JPEG Baseline and three-frame JPEG-LS controls decoded correctly in
both readers, demonstrating codec availability. GDCM's own `--split` requests
for the small controls were refused and did not provide another qualified
fragmented-writer control. The results above qualify these specific shapes,
caps and tool versions; they do not establish support for every arbitrary
fragment boundary. Independent unit tests cover the complete boundary sweep
of lengths 0 through `2*cap+3` for caps 2, 4, 6, 16 and 32, including terminal
padding, Fragment count and offsets.
