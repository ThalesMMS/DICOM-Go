# img2dcm

`img2dcm [flags] input [input ...] output.dcm` imports stored samples into a new DICOM image, or encapsulates an existing compressed image/video payload. It uses `encapimage.FromImages`, the shared common-module builder and `clifile.WriteAtomic` for atomic publication, with File Meta rebuild and context-aware writes. The default module dependencies and codec registry are unchanged.

```sh
img2dcm own.png own.dcm
img2dcm a.pgm b.pgm multi.dcm
img2dcm -format raw -rows 64 -columns 96 -bits-allocated 16 -bits-stored 12 -endian big -offset 128 own.raw own.dcm
img2dcm -series-from existing.dcm own.bmp derived.dcm
img2dcm -j2k-lossless own.j2k own.dcm
img2dcm -region-code CODE -region-scheme SCHEME -region-meaning 'Actual imaged region' own.mp4 video.dcm
```

## Input profiles

| Input | Accepted profile | Pixel handling |
| --- | --- | --- |
| PNG | Gray/RGB/palette, 8/16 bits, opaque alpha only | Standard PNG decoder; palette expands to RGB; interleaved stored samples |
| BMP | V3 DIB 40, BI_RGB 8-bit palette/24-bit BGR/32-bit BGRX, top-down or bottom-up | Orientation and palette preserved numerically; BGRX unused byte is discarded |
| PNM | P2/P3/P5/P6, MAXVAL 255 or 65535 | ASCII values or big-endian binary samples; no implicit MAXVAL rescaling |
| PGX | `PG ML`/`PG LM`, signed/unsigned mono, precision 1–16 | Exact samples; canonical sign extension required for signed values |
| Raw | Explicit rows/columns, 8/16 Bits Allocated, precision 1–Bits Allocated, 1/3 interleaved samples, endian/offset/frame count | Remaining bytes must match exactly; no guessing, packed/planar conversion or rescaling |
| JPEG | Existing supported baseline/extended/lossless; retired progressive opt-in | Existing JPEG header/sanitizer, coding payload unchanged; APP/COM removal defaults shared with jpg2dcm |
| JLS | Existing qualified 1/3-component JPEG-LS framing/header parser, default MAXVAL | NEAR selects lossless `.80` or near-lossless `.81`; no re-encoding |
| J2K | Raw Part 1 codestream, identical unit component grids, zero image/tile origins, up to 4096 tiles, one tile-part per tile, no tile coding overrides, no explicit precincts | Headers and framing checked, original payload retained; JP2/Part 2/HTJ2K/ROI/POC/PPM/PPT profiles refuse |
| Video | Single-track AVC High profile through Level 4.1 classic MP4, 8-bit 4:2:0 progressive; MPEG-2 Main profile Program Stream in the admitted DICOM Main/High Level subset | Original container retained as one padded fragment, empty BOT; no video decoding, re-encoding or remuxing |

Format detection uses content. Raw needs `-format raw`. Multiple input files must agree in dimensions, precision, sign, sample count, allocated width, photometric interpretation, compression history, transfer syntax and ICC profile. They may use different native containers with that same sample layout. JPEG marker removal counts and trailing-byte totals cover all frames.

For J2K, reversible COD and unquantized QCD alone do not prove a full lossless source: layer truncation may already have discarded coding passes. `.90` therefore requires the caller's `-j2k-lossless` declaration and the reversible header constraints. The report records a declaration, not an original-image comparison. Without declaration, reversible input uses `.91` and unknown/empty Lossy Image Compression; irreversible input records `01`. JPEG-LS NEAR is read from the source. Structural compressed-input acceptance does not guarantee entropy decoder acceptance for every codestream.

## IOD and metadata

One image defaults to Secondary Capture. Compatible unsigned multi-frame layouts use Multi-frame Grayscale Byte/Word or True Color SC. Signed multi-frame, RGB16 multi-frame, and 16-bit allocations with precision 8 in Multi-frame SC refuse because they need a different IOD representation. An explicit `-vlp` writes one unsigned 8-bit VL Photographic image; lossy RGB input that would need relabeling refuses. `-new-sc` explicitly selects the default SC family and refuses video. `-oph-photo` is not an alias for VL Photographic: Ophthalmic Photography is a different IOD and is unsupported.

Video uses Video Photographic Image Storage. It requires caller-supplied anatomic CodeValue/CodingSchemeDesignator/CodeMeaning, writes actual frame count and constant Frame Time, and derives syntax/color/precision from the accepted coding headers. Anatomy is never inferred. New imports default Burned In Annotation to conservatively `YES`; `-burned-in NO` requires the caller's explicit statement. The current IOD validator checks required attributes and structured values; metadata syntax is checked separately, excluding admitted Pixel Data/ICC payloads. Manual clinical/conditional value constraints reported by the validator are not independently proven.

`-study-from` inherits patient/study modules. `-series-from` also retains the study/series identity, series number/description/modality. Explicit option fields win, except conflicting study identity with an inherited series refuses. A source SOP UID is never reused as the new generated instance. `-instance-uid` may provide a different explicit UID. Source text is converted to UTF-8, and the caller-owned source remains open and immutable until construction finishes. The CLI retains the source descriptor through construction and refuses replacing an input/source path or hardlink.

## Bounds and cancellation

Defaults are combined input 256 MiB, admitted native sample span 512 MiB, 16 Mi pixels per frame and 1024 frames. `-max-input-bytes`, `-max-decoded-bytes`, `-max-pixels`, `-max-frames` set positive limits. Library zero selects defaults; negatives refuse. Input has a 1 GiB hard ceiling and frames 100000. Rows/columns must fit DICOM US. Multi-frame SC Page Number Vector additionally must fit the 65534-byte explicit IS length; large requested frame counts that exceed it refuse before allocation. Native length arithmetic, aggregate defined Pixel Data length (including padding), and host-int conversion are checked before final Pixel Data allocation; native aggregate admission also precedes retaining the next prepared frame. PNG also preflights conservative decoder pixel storage. These are admitted byte/work spans, not a peak RSS promise.

New importer JPEG segment retention caps at 4096 (the legacy `FromJPEG` API keeps its existing contract). PNM headers cap at 64 KiB and tokens 64 bytes; PGX headers at 256 bytes; MP4 box visits 4096, MPEG packets 4096, AVC NALs 131072, J2K tiles 4096. Container/frame metadata tables are admitted before allocating frame arrays. Source DICOM reads separately cap 256 MiB total/pixel bytes, 16 MiB per metadata element, 100000 elements, 4096 fragments and 32 nesting depth.

Context is checked before/after existing parser work, at PNG decoder reads, in source reads, row/frame/sample/header loops and framing validation. New JPEG entropy and MPEG start-code scans use at most 4 KiB searches between checkpoints. PNG inflate work over already buffered input, operating-system file calls and other older parser interiors are not promised immediately interruptible. Cancellation takes priority over a parser error observed in the same operation. Cancel/error returns no constructed file or published output; atomic writing uses the existing shared publication path. An existing target remains intact when construction or writing fails.

## Video exclusions and authority

Fragmented MP4, edits, rotations/scaling, multiple tracks/audio, avc3/in-band parameter changes, non-square SPS SAR/MP4 pasp, variable decode timing, unsupported profiles/bit depth/chroma/fields, MPEG-TS/ES and HEVC refuse in this import profile. MPEG-2 H-14 refuses. DICOM Main Level timing/dimension caps and High Level 16:9 geometry/timing restrictions are enforced. Encapsulation does not mean Twin or the library can decode/play video.

Coding/container metadata follows public [PS3.5 MPEG-2 Main Level](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_8.2.5.html), [High Level](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_8.2.6.html), and [AVC High Profile/Level 4.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_8.2.7.html). No upstream implementation is copied. Synthetic numeric fixtures and bounded independent decoder/container readback are correctness evidence, not clinical or arbitrary-codec interoperability acceptance.
