# Pixel Transfer Syntax Transcoding

`pixeldata` provides an explicit, bounded still-image transcoding pipeline. It
does not register encoders globally and it never treats decoder availability as
evidence that an encoder exists.

## Registries and ownership

- `MemoryEncoderRegistry` is separate from the legacy decoder `Registry`.
  Registration freezes capabilities, rejects duplicate Transfer Syntax UIDs,
  and has no package-level default.
- `TranscodeOptions.DecoderRegistry` is required when the source is a compressed
  still-image syntax. By default, `TranscodePath` also requires the target
  decoder to check newly encoded lossless or lossy output before publication.
  `EncoderRegistry` is required when the target is encoded.
- Input `object.File` and `object.Object` values are borrowed and are never
  mutated. Returned values are detached from source buffers and deferred value
  providers. An unavailable deferred value fails explicitly.
- `TranscodeCoreDataSet` is the `core.DataSet` adapter. Because duplicate Data
  Elements have ambiguous last-wins semantics in `object.Object`, it rejects
  duplicates explicitly instead of silently collapsing them.
- `TranscodePath` owns every opened handle and temporary file. It writes a
  private `0600` file in the destination directory, syncs it by default and reads it back
  through the same open handle under finite limits, verifies the output syntax
  and pixel representation, revalidates source and destination identities, and
  only then renames that same temporary entry atomically.
- Linux and macOS use filesystem compare-and-swap primitives when replacing an
  existing destination. Windows also supports replacement: `TranscodePath`
  revalidates the destination snapshot immediately before publication, then
  atomically renames the validated temporary handle with replace-existing
  semantics and verifies the published destination identity. Publication has
  an old-or-new contract: a failure before the atomic rename leaves the previous
  destination authoritative; after the rename succeeds, the new destination
  remains authoritative even if a later verification, close, or durability
  step reports an error, and the operation does not roll it back. Other
  platforms without an equivalent primitive expose the in-memory APIs but
  return `ErrTranscodeUnsupported` from `TranscodePath`.

## Filesystem traversal and restricted Unix environments

`TranscodePath` accepts explicit paths outside the current working directory;
it is not an API that confines operations to a caller-selected subtree.
It resolves existing parent-directory aliases with `EvalSymlinks` once, then
opens the resulting canonical path component by component from a descriptor
for `/`, using `O_NOFOLLOW` throughout. Final source/destination symlinks are
rejected. A symlink introduced into a canonical ancestor is also rejected.
The caller must keep the canonical directory namespace stable during the
operation and use trusted directories. Precommit reopens and compares source
and parent identities; publication and cleanup remain anchored to the opened
parent descriptor. This does not promise a pathname-level transaction against
an adversary renaming ancestors after the last revalidation.


On Linux, an `EACCES` from that initial root open now retries the **same `/`
anchor** with `O_PATH|O_DIRECTORY|O_CLOEXEC|O_NOFOLLOW`. An `O_PATH` descriptor
can serve as `openat`'s directory anchor without directory read permission;
subsequent traversal still enforces search permission. See the Linux
[open(2) specification](https://man7.org/linux/man-pages/man2/open.2.html).
The retry is limited to `EACCES`; it does not retry `EPERM`, resource failures,
or operations relative to `.`. Final directory handles remain readable for
publication and `fsync`; their errors are not bypassed. Other Unix platforms
retain the original root-opening contract.

If a sandbox still rejects traversal (including `O_PATH` or ancestor/final
directory reads), provision the directory permissions/operations required by
this contract, or use `TranscodeFile` with caller-owned I/O and publication.
Do not treat lexical path-prefix checks as an equivalent replacement.


```sh
CGO_ENABLED=0 go test -c ./pixeldata -o /tmp/pixeldata-sandbox.test
sudo env DICOM_GO_TEST_CHROOT=1 /tmp/pixeldata-sandbox.test \
  -test.run='Transcode' -test.v
```

## Byte order

Raw values in an `object.Object` are always in its `ValueByteOrder`. When the
syntax being written has the other byte order, the Part 10/raw writers
(`WriteFile`, `WriteFileFrames`, `WriteDataSet`, `WriteDataSetWithValidation`)
and DIMSE `SendDataSet`, which writes through `WriteDataSet`, reverse each
numeric value by its width while writing, in memory or deferred and at any
sequence depth; the source object is not changed, and a value that is not a
whole number of units is refused, unless
`WriteFileOptions.KeepPartialByteOrderUnits` reverses its whole units and
keeps the trailing partial unit (STOW-RS re-encoding sets it). Native frames supplied by a `FrameSource`
are already in the byte order of the syntax being written and pass unchanged.
`TranscodeDataSet`/`DecompressDataSet` convert the same way and set the
result's `ValueByteOrder` to the target. Conversion follows these rules:

| Value | Width reversed |
|---|---|
| AT, OW, SS, US | 2 bytes (PS3.5 7.3) |
| FL, OF, OL, SL, UL, Float Pixel Data (OF) | 4 bytes |
| FD, OD, OV, SV, UV, Double Float Pixel Data (OD) | 8 bytes |
| Native OW Pixel Data with Bits Allocated 32 or 64 | the sample width, 4 or 8 bytes |
| OB, UN and text | not reversed |

PS3.5 defines OW Pixel Data order for 16-bit words and is silent about wider
samples under the retired Explicit VR Big Endian syntax. dicom-go orders each
32/64-bit sample as one big-endian integer, which is what pydicom 3.0.2 and
amr v1.6.0 read and write; word swapping would transpose halves (1249000 would
read as 250085395). The width comes from Bits Allocated in the same dataset or
sequence item, so an 8/16-bit Icon Image Sequence item never inherits a 32-bit
parent's width. Values whose length is not a multiple of their width are refused
with `ErrPixelDataSizeMismatch`; malformed Bits Allocated returns
`ErrInvalidMetadata`; datasets with deferred values return
`ErrTranscodeUnsupported`. None of these errors publish a partial output.
Evidence: `TestWideSampleBigEndianUsesSampleWidthOnEveryPath`,
`TestFloatPixelDataSwapsByVRWidth`, `TestWideSampleEndianRefusesInvalidLengthsWithoutOutput`,
the opt-in `TestPydicomWideSampleEndianInterop` and the validation module's
`TestAmrWideSampleBigEndianInterchange`.

DICOM JSON and Native DICOM Model XML carry the `InlineBinary` and bulk data
of OW, OF, OD, OL and OV values in Little Endian, by the same units: native OW
Pixel Data with Bits Allocated 32 or 64 by sample, other values by VR width.
A Big Endian object's values are reversed on export, and reading with a Big
Endian `UnmarshalOptions.TransferSyntax` (or `ByteOrder`) reverses them back.
`SourceByteOrderInlineBinary` in the options of `dicomjson` and `dicomxml`
keeps the object's value byte order instead.

## Icon Image Sequence

The file, data-set and path APIs transcode Icon Image Sequence (0088,0200)
items independently from the main image. A native target decompresses icons;
an encapsulated target keeps them native by default, as PS3.5 A.4 permits.
`TranscodeOptions.CompressIcons` selects the same lossless target codec for
each icon. It requires that codec's encoder and decoder, complete icon
metadata and exactly one frame. Main-image rewrites do not apply to icons.
Other sequences containing nested Pixel Data retain their explicit unsupported
error for encapsulated conversions.

`TranscodeReport.Icons` records each tag/item path, action, byte counts and
whether metadata was incomplete. An already-native icon with missing metadata
can be preserved byte for byte; requesting its compression fails explicitly.
This preserves the malformed `simpleImageWithIcon.dcm` fixture without
inventing Samples per Pixel. Its compression can explicitly select the
existing `Recovery.DefaultImagePixelModule` policy; applied repairs appear
in each icon's `DecodeDiagnostics`. Pixel/frame limits count readable image
metadata before and after recovery; incomplete native icons remain bounded
by input/output byte limits. An icon failure prevents path publication.

## Supported conversions

- Native Implicit/Explicit VR Little Endian, Explicit VR Big Endian, and
  Deflated Explicit VR Little Endian can be converted among one another without
  changing pixel sample values. See [byte order](#byte-order) for the rules.
- Encapsulated Uncompressed can be decoded to native and produced with one
  fragment per frame.
- RLE Lossless can be decoded through an explicit decoder registry and encoded
  through the pure-Go `pixeldata/rle.Encoder`. The encoder accepts samples
  allocated in multiples of 8 bits up to 15 segments per frame, signed or
  unsigned monochrome, and unsigned palette/interleaved RGB or YBR_FULL,
  including multiple frames.
- JPEG Baseline Process 1 encoding through `pixeldata/jpeg` requires explicit
  quality (1 through 100) and `TranscodeOptions.AllowLossy`. `NewEncoder` keeps
  the Go standard-library monochrome/4:2:0 encoder. `NewEncoderWithOptions`
  offers optimized Huffman tables and 4:2:2 or 4:4:4 color sampling. Native
  input is unsigned 8-bit monochrome or interleaved RGB; all YCbCr output is
  declared `YBR_FULL_422`, with actual sampling in SOF0. More chroma samples
  can increase output size, and Huffman optimization adds CPU and coefficient
  scratch. See [JPEG Baseline encoder](JPEG_BASELINE_ENCODER.md).
- JPEG Lossless Process 14 (`.57`) and SV1 (`.70`) are encoded by the pure-Go
  `pixeldata/jpeglossless` encoder (`RegisterEncoder`; `NewProcess14Encoder`
  for predictors 2-7). It accepts 8 or 16 allocated bits with 2-16 stored
  bits, signed or unsigned monochrome, and unsigned interleaved RGB or
  YBR_FULL. Each frame gets optimal Huffman tables.
- JPEG 2000 Part 1 (`.90`, `.91`) and HTJ2K (`.201`, `.202`, `.203`) are
  encoded by the pure-Go `pixeldata/jpeg2000` encoder
  (`RegisterEncoders` for the lossless syntaxes, `RegisterLossyEncoders`
  with a quantization step for the irreversible ones). It accepts 8 or 16
  allocated bits with 1-16 stored bits, signed or unsigned monochrome,
  unsigned palette (lossless) and interleaved RGB, optionally through the
  RCT/ICT (`YBR_RCT`/`YBR_ICT` output). Lossless verification decodes the
  output, so file and stream transcodes need a JPEG 2000 decoder in
  `DecoderRegistry`, such as the optional adapter's `RegisterCore` and
  `RegisterCoreHTJ2K`. See [JPEG 2000 and HTJ2K encoder](JPEG2000_ENCODER.md).
- JPEG XL Lossless (`.110`) and JPEG XL (`.112`) are encoded by the pure-Go
  `pixeldata/jpegxl` encoders: `RegisterLosslessEncoder` (1-16 stored bits
  in 8 or 16 allocated bits, signed or unsigned monochrome and interleaved
  RGB; signed samples are stored with an offset-binary level shift) and
  `RegisterLossyEncoder` with an explicit quality (8-bit monochrome and RGB,
  lossy authorization, method `ISO_18181_1`). `jpegxl.Register` adds the
  decoder that lossless verification needs.
- A `TranscodeOptions.CodestreamTranscoder` converts encoded frames between
  two encapsulated syntaxes without decoding pixels when it supports the pair
  (`CanTranscodeCodestream`). `jpegxl.Recompressor` recompresses JPEG
  Baseline (`.50`) and Extended (`.51`) frames to JPEG XL JPEG Recompression
  (`.111`) and reconstructs `.111` frames to the identical JPEG, so neither
  direction is lossy; JPEGs that cannot be restored exactly (progressive,
  12-bit) fail with `jpegxl.ErrNotReversible` at stage `codestream`.
  `ForceReencode` disables the codestream route, and `TranscodePath` still
  verifies the output by decoding both syntaxes.
- An equivalent source and target Transfer Syntax uses a detached
  byte-preserving Pixel Data fast path unless `ForceReencode` is selected.
- Other encoder families are extension points. Missing adapters return typed
  availability errors. Video, JPIP, Float Pixel Data and Double Float Pixel
  Data are never silently treated as ordinary integer still-image frames.
- Deflated Image Frame Compression (`1.2.840.10008.1.2.8.1`) is an ordinary
  lossless per-frame target and source: `deflateframe.RegisterEncoder` deflates
  each native frame into one fragment (raw RFC 1951, NULL-padded to even
  length) without touching the Image Pixel attributes, and the builtin decoder
  inflates each fragment to exactly the native frame size, refusing any stream
  that would expand further (`deflateframe.ErrExpansionLimit`). It is not the
  data-set deflate of Deflated Explicit VR Little Endian.

RLE emits the PS3.5 64-byte header, byte planes from most significant to least
significant, PackBits reset for each row, and one fragment per frame. Basic
Offset Table entries point to Item tags and include prior item headers plus even
padding. The pipeline removes stale Extended Offset Table fields whenever the
representation changes and emits EOT/EOT Lengths if frame offsets do not fit in
32 bits.

Decompression with `DecompressOptions.Recovery` writes every Image Pixel
Module value it assumed (`DefaultImagePixelModule`) or took from the JPEG
or JPEG 2000 codestream (`ReconcileWithCodestream`) into the output data set, and
`DecompressOptions.Report` lists each correction, so a rewritten object is
self-consistent. See [CONFORMANCE.md](CONFORMANCE.md) for the rules.

## Rewriting the stored representation

`RewriteNative(ctx, dataset, NativeRewrite)` changes how native pixel data is
stored, not only how it is displayed, and updates the attributes that describe
it; the source object is not modified. `TranscodeOptions.Rewrite` runs it
between decoding and encoding, so representations an encoder refuses (the
OpenJPEG encoder takes only interleaved MONOCHROME or RGB) can be transcoded;
`TranscodeReport.Rewrite` lists the applied steps.

- `PaletteToRGB`: PALETTE COLOR indices (signed or unsigned, 8 or 16 bits)
  go through the palette, linear or segmented, into RGB at the palette's
  entry depth; the palette, Palette Color LUT UID, Smallest/Largest Image
  Pixel Value and Pixel Padding (index values) are removed.
- `LinearizeSegmentedPalette`: keeps PALETTE COLOR and replaces Segmented
  Palette Color LUT Data with the equivalent linear data.
- `YBRFullToRGB`: unsigned YBR_FULL to RGB with the inverse of the PS3.3
  C.7.6.3.1.2 equations, rounded and clamped, in either planar layout.
- `Monochrome1ToMonochrome2`: stored values v become min+max-v of the Bits
  Stored range; Window Center moves to k-c+1 for LINEAR (k-c for LINEAR_EXACT
  and SIGMOID, with k = slope*(min+max) + 2*intercept), Pixel Padding and
  Smallest/Largest Image Pixel Value are inverted (the last two swap), Pixel
  Intensity Relationship Sign is negated and Presentation LUT Shape INVERSE
  becomes IDENTITY, so the image displays the same. A Modality or VOI LUT
  Sequence or functional groups are refused (`ErrNativeRewrite`).
- `PlanarConfiguration`: 0 (interleaved) or 1 (by plane), a pure reordering.

Steps that change pixel values are appended to the Derivation Description;
the SOP Instance UID is kept, as the image content is the same. Bits Stored
must fill the low bits (High Bit = Bits Stored - 1) of 8- or 16-bit samples.

Evidence: every step is compared with the display pipeline on synthetic
images (exact for palettes and planar, within one step for YBR_FULL and for
MONOCHROME1 inverted, with a narrow window that checks the LINEAR offset),
and on the gdcmData files `LEADTOOLS_FLOWERS-8-PAL-Uncompressed`,
`NM-PAL-16-PixRep1`, `gdcm-US-ALOKA-16`, `US-RGB-8-epicard` and
`IllegalGroup2ImplicitTS` (exact). Planar reordering round-trips byte for
byte. `TestTranscodeRewriteReachesRestrictedEncoder` transcodes a palette to
JPEG Lossless, which refuses palettes, and
`TestOpenJPEGEncodesRewrittenRepresentations` (opt-in, OpenJPEG adapter)
encodes palette, planar YBR_FULL and planar 16-bit RGB to JPEG 2000 lossless,
decoded identically by `opj_decompress` and the pure-Go core.

## Lossy policy

Lossy encoders require `AllowLossy`. A successful lossy conversion appends the
compression ratio and method history, sets Lossy Image Compression to `01`,
marks Image Type as derived, and assigns a new SOP Instance UID. It records
the step as DCMTK and GDCM do (PS3.3 C.7.6.1.1.5), through package
`derivation`: Derivation Code 113040 (Lossy Compression, added once), a
Derivation Description naming the method and ratio (appended after `; ` to an
earlier one), and a Source Image reference to the predecessor instance with
Purpose of Reference 121320 (Uncompressed predecessor), when the source has
SOP Class and SOP Instance UIDs. Classic images carry them at the top level;
a multi-frame functional group object carries them in the Derivation Image
Functional Group (Shared, or each Per-Frame item when the object keeps that
group per frame, referencing its frame). dicom3tools `dciodvfy` (snapshot
20260901) reports no new finding on gdcmData `IllegalGroup2ImplicitTS` (CR)
and `EnhancedWithIPPPerFrameIOPShared` (Enhanced X-Ray 3D) after a lossy
`.91` transcode compared with the sources. Encoder output
metadata changes are applied only when every frame reports the same validated
Photometric Interpretation and Planar Configuration. A real transform must
also be declared in `EncoderCapabilities.OutputPhotometricInterpretations` or
`OutputPlanarConfigurations`; otherwise the encoder output is rejected.

Lossy encoders, in the default module or in optional adapters, are registered
explicitly, publish directional manifest evidence, and satisfy the same
limits, cancellation, metadata, and transactional contracts.

## Limits and cancellation

Zero-valued `TranscodeOptions` use finite defaults for frames, pixels, input and
output bytes, fragments, structural nodes (elements, string components and
sequence items), and nesting depth. `DecompressDataSetContext`
and `DecompressFileContext` add finite frame/pixel/native/input/expansion limits
while preserving the legacy wrappers. Internal codecs may implement the
additive `ContextCodec` interface; legacy codecs are checked before and after
their call.

Errors and reports contain fixed stages and field names, not paths, patient
values, pixel bytes, or backend-controlled panic text.

## Streaming large files

`TranscodePath` transcodes a little-endian source frame by frame when its
frames can be located without decoding the whole value: native Pixel Data,
one Item per frame (with an empty Basic Offset Table or one pointing at those
Items), or JPEG, JPEG-LS and JPEG 2000 codestreams laid out in any way the
[shared frame plan](ENCAPSULATED_FRAME_ASSEMBLY.md) accepts. The source is
read with its Pixel Data deferred; each frame is read, decoded, encoded and
verified on its own (a lossless frame must decode back to the source
samples, a lossy one must decode), encoded frames wait in a private spool
file beside the destination, and the output is written with
`object.WriteFileFrames` and then read back structurally before the atomic
publication. A syntax change of native data and an equivalent-syntax copy
stream the value straight from the source. Output metadata comes from the
same functions as the in-memory route, and tests compare both routes byte for
byte across native, RLE, JPEG-LS, JPEG Lossless, JPEG Baseline (lossy, with
its new SOP Instance UID fixed), split-Item JPEG-LS and Encapsulated
Uncompressed conversions. `RecoverUN`, `Rewrite`, `Recovery`, embedded
overlays, big-endian or deflated syntaxes, float Pixel Data and other Item
layouts take the in-memory route.

The limits keep bounding the whole input and output, so a multi-GiB file needs
larger `MaxInputBytes`/`MaxOutputBytes`/`MaxPixels`. With serial codec calls,
managed pixel buffers remain proportional to a frame; an explicit parallel
pipeline retains a bounded window instead. A 3.9 GiB native file of 2000 frames of 2 MiB
(`DICOMGO_LARGE_TRANSCODE=1 go test ./pixeldata -run MultiGigabyte`) was
transcoded to RLE in 27 s with a sampled peak heap of 42 MB, and to Implicit
VR Little Endian in 10 s with 0.3 MB; the default suite checks that 12 MiB of
Pixel Data transcode within 4 MiB of heap.

`object.WriteFileFrames(w, file, frames, opts)` is the public writer behind
it: it writes a data set as `WriteFileWithOptions` does, pulling Pixel Data
from a `FrameSource` one frame at a time (native frames concatenated,
encapsulated frames one Item each after a Basic Offset Table, or an empty one
with the Extended Offset Table, by `OffsetTablePolicy`), with byte-identical
output to `WriteFile` on the same frames.

### Frame windows, verification and durability

`TranscodeOptions.Pipeline` applies to the streaming `TranscodePath` route.
`Workers=0` or `1` preserves serial calls, including compatibility with legacy
decoders that do not promise reentrancy. `Workers>1` explicitly requests
parallel decode/encode/verify; the caller must ensure its codecs and registries
support concurrent calls. The existing shared CPU budget limits helper tasks.
Each window finishes and joins all helpers before ordered spool writes; the
next window waits for those writes. Failure or cancellation cancels and joins
helpers before temporary-file cleanup. Codec cancellation remains cooperative.
Serial native-target decoding writes one frame at a time directly to the private
staged output, without an additional complete decoded spool.

Zero window limits choose `min(workers,32)` frames and 256 MiB of conservative
buffer charge. `MaxEncodedFrameBytes=0` derives `4*native-frame-bytes + 64 KiB`,
clipped to the total output limit. The charge includes input and conservative
decode, encode and verification buffers. It is not an RSS cap: private codec
workspace, excess returned capacity, metadata and the runtime are outside it.
An oversized encoded frame is refused when its codec call returns, before
verification or publication; the API cannot bound that codec's internal
allocation. Disk spool bytes are separately bounded by `MaxOutputBytes`.
Whole-object fallback retains the existing global limits and reports
`Streaming=false`; frame-window limits do not apply to it.

`SkipVerification=false` preserves the additional pixel check: lossless output
must match decoded source samples, and lossy output must decode. Setting it to
`true` explicitly omits only this additional top-level check; staged structural
readback, syntax, encoder metadata, bounds and atomic publication remain
mandatory. Recovery/rewrite and encoded icons keep their own checks. Source
compressed data still requires an explicit decoder registry.

`Durability=TranscodeDurable` is the zero/default policy and syncs the staged
file and destination directory where supported. Windows' existing directory
sync helper is a no-op and is reported as `DirectorySynced=false`.
`TranscodeAtomic` explicitly omits both syncs while retaining atomic replacement;
it makes no persistence promise through a process, OS or power failure. A
directory-sync error after replacement leaves the new output published, as
under the existing publication contract.

`TranscodeReport` records the effective verification, durability, actual syncs,
streaming route, requested worker ceiling and peak logical frame/byte reserves.
Worker phase times are sums of task durations, not wall time or pure codec
kernel time. Serial decode occurs inside output writing, so those phases
overlap and must not be added as independent wall-time components.


## Reading single frames and regions

`pixeldata.FrameReader` (`OpenFrameReader`, or `NewFrameReader` over a file
read with `DeferPixelData` and its seekable source) indexes Pixel Data
without loading it, with the same frame location rules as streaming
transcoding, and reads one frame at a time: `RawFrame` returns it as stored,
`Frame` decoded as `DecompressDataSetContext` would, and `Region` a
rectangle of it with metadata of the region's size. A native region reads
only its rows (plane by plane for planar data, byte-swapped for big endian);
an encapsulated region is decoded by the codec when it implements
`FrameRegionDecoder`, and otherwise the frame is decoded whole and cropped.
The RLE codec decodes each segment only to the region's last row and keeps
only its columns; the JPEG 2000 core adapter (`j2kcore.DecodeOptions.Region`)
decodes only the tiles the region overlaps. For monochrome JPEG-LS, the
pure-Go Lossless and explicit Near-Lossless codecs keep three predictor rows
and emit only the requested output pixels. They consume the rest of the
entropy stream to check truncation and padding, so zooming saves output
memory while doing prediction for every row. Bits Allocated below 8, which
packs frames across bytes, is refused.

Tests compare every frame and several regions with the full in-memory
decode for native (mono, interleaved and planar RGB, big endian), RLE,
JPEG-LS and JPEG 2000 files, and the RLE and JPEG 2000 region decoders with
the cropped full decode (runs crossing rows, subsampling, reduced
resolution). Region of 256 x 256 in a 4096 x 4096 16-bit frame
(`go test ./pixeldata -run '^$' -bench FrameReaderRegion`): native 0.48 ms and
128 KiB against 7.4 ms and 34 MB to decode and crop; RLE 0.96 ms and 3 MB
against 32 ms and 170 MB. One tile of the tiled JPEG 2000 corpus codestream
decodes in 33 µs against 694 µs for the image
(`go test ./examples/codec-adapters/jpeg2000 -run '^$' -bench CoreRegion`,
from that module).

`go test ./pixeldata/jpegls -run '^$' -bench BenchmarkJPEGLSRegionalDecode -benchmem`
compares a 96 × 96 bottom-right crop from a 2048 × 2048 16-bit frame with
direct decoding into an allocated or reusable destination. The benchmark
includes entropy decoding and reports the peak size of active raster buffers.

### Partial decoding

`FrameReader.DecodePartial` takes one `PartialDecodeRequest`: a frame, a
region in full-resolution pixels (the zero rectangle is the frame), a
`ReductionLevel` n for 1/2^n across and down (up to `MaxReductionLevel`, 16)
and a `QualityLayers` count (0 for all). The output is the reduced grid's
pixels whose 2^n x 2^n cells overlap the region (`ReducedRegion`: Min
rounded down, Max rounded up), so a reduced region is the crop of the
reduced frame and reduced tiles meet without seams. A codec that implements
`PartialFrameDecoder` serves a supported request directly. The JPEG 2000 core
adapter supports tiles, resolution levels and quality layers. The pure-Go
JPEG-LS codecs support monochrome regions at full resolution. Otherwise the reader falls back: native frames
read only the rows and columns of the region's cells, a `FrameRegionDecoder`
(RLE) decodes them, other codecs decode the whole frame and crop it; a
reduction is then the mean, rounded half up, of each cell's stored values
(BitsStored at HighBit, sign extended; cells clipped at the frame's edge),
and a layer count is ignored. `PartialDecodeResult` says which: `Path`,
`Fallback`, `AveragedReduction` and `QualityLayers` (0 when every layer was
decoded). `NoFallback` refuses a fallback with
`ErrPartialDecodeUnsupported`. `PartialDecodeCapabilities` reports, per
frame, whether regions, how many reduction levels and how many quality
layers the codec serves itself. A region outside the frame is
`ErrInvalidRegion`, a negative or too deep reduction (or one beyond a
natively reducing codestream's decomposition levels) `ErrInvalidReduction`,
a negative layer count (or more than the codestream has)
`ErrInvalidQualityLayers`. Beyond what the codec decodes, memory holds the
region's cells at full resolution and the output. JPIP-referenced Pixel Data
has no local codestream and still needs an external JPIP client
(`ErrJPIPRetrievalRequired`).

The JPEG 2000 reduction is the codestream's own low band, not a cell mean:
the two differ by the half cell between a sample and a cell's centre (on a
ramp of 1.5 levels a pixel, 3 levels at 1/2 and 11 at 1/16, checked
against the fallback in `TestFrameReaderDecodePartialWithCoreCodec`).
Reductions, regions and layers of the core codec are checked bit for bit
against the crop of the decode they are part of, and layers against
`opj_decompress -l`. A 1024x1024 region at 1/4 of a 4096x4096 16-bit frame
(`go test ./pixeldata -run '^$' -bench FrameReaderDecodePartial`) takes
3.8 ms and 2.2 MB native and 7.1 ms and 7.2 MB RLE, against 5.1 ms and
34 MB, and 17 ms and 136 MB, to decode the frame and reduce it; a 1/8
thumbnail of a 2048x2048 JPEG 2000 frame with five levels 4.5 ms and 4 MB
against 389 ms and 189 MB (`-bench CorePartialDecode` in the JPEG 2000
adapter module).

### Per-frame codec routing

A `CodecRouter` (`NewCodecRouter`, then `Register` in any `Registry`) is the
codec of one transfer syntax built from several backends, each a named
`Codec`. It finds the frames among the Items as the codecs do (marker scan
for JPEG, JPEG-LS and JPEG 2000; one Item per frame otherwise) and decodes
each frame on its own, in a one-frame copy of the data set, with the first
backend whose `Accept` takes it (nil accepts every frame): a codestream
feature only one backend implements can send those frames to it and the
rest elsewhere. When a backend fails on a frame the router tries the next
that accepts it, unless `Fallback` says no (a function returning false
forces the first backend); cancellation never falls back. `Observe`
receives a `FrameCodecDecision` per frame (backend, backends passed over
with a fixed reason, fallback, admitted bytes), `Stats` counts them, and a
fallback adds the `codec-backend-fallback` diagnostic. Frames of one object
must decode to the same size and photometric interpretation
(`ErrCodecBackendsDisagree`). `FrameReader` uses a registered router for
single frames, regions and partial decodes alike; a region or partial
decode goes to the backend that would decode the frame when it implements
`FrameRegionDecoder` or `PartialFrameDecoder`, and otherwise falls back to
the whole frame through the router.

`ShadowComparison` decodes one frame in every `SampleEvery` a second time
with an oracle backend and compares the samples (exactly, or within
`MaxAbsoluteError`). The output is always the routed backend's. Each
`ShadowReport` names the backends, frame, geometry, outcome (matched,
mismatched, failed) with a fixed reason code, the number of samples that
differ, the largest difference and the first differing index; it never
carries sample values, attribute values or codec error text, so it can be
logged without PHI.

`MemoryAdmission` is a weighted semaphore over bytes, granted in arrival
order, which any number of routers and goroutines can share. The router
acquires each frame's estimated working set (`FrameBytes`; by default the
encoded and the decoded frame, doubled for a shadowed frame) before
decoding it and releases it after, so with `Workers` frames decoded at once
the frames in flight hold at most the capacity; a single frame larger than
the capacity runs alone. The estimate is a plan, not a measurement of
resident memory, and the decoded frames a whole-object decode returns are
the caller's. On 64 frames of 128x128x16 bits decoded by eight workers
(`go test ./pixeldata -run '^$' -bench CodecRouterMultiFrameAdmission`),
256 KiB of frames were in flight unbounded, 64 KiB under an admission of
two frames and 128 KiB under four.

## Verification

```sh
go test ./pixeldata ./pixeldata/rle -count=1
go test -race ./pixeldata ./pixeldata/rle -count=1
go test ./pixeldata/rle -bench Encoder -benchmem
go test ./pixeldata/jpeglossless -run '^$' -bench BenchmarkEncode -benchmem
```

The encoder benchmarks report elapsed time, bytes and allocations per
operation, and a sampled peak heap delta. The first recorded qualification run
is in [`PIXEL_TRANSCODING_BENCHMARKS.md`](PIXEL_TRANSCODING_BENCHMARKS.md).

The optional pydicom reader gate is documented in
[`INTEROP_MATRIX.md`](INTEROP_MATRIX.md). The capability manifest distinguishes
`decode` and `encode`; legacy v2 JSON without a direction remains decode-only.
