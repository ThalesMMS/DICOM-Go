# Still-image codec capability matrix

This document is the canonical human-readable codec inventory. The executable
source of truth is `pixeldata/codecprofile.CodecFullManifest`; local checks verify this
document against that manifest. Native uncompressed transfer syntaxes do not
need a pixel codec and are outside this table. Video and JPIP data-bin transfer
syntaxes are also outside the still-image codec registry.

`D` means decode and `E` means encode. A capability is available only when the
named profile is selected and every runtime required by that profile passes
preflight.

| Transfer syntax | D | E | Profile and implementation | Dependency / license | Qualified platforms |
|---|:---:|:---:|---|---|---|
| Encapsulated Uncompressed Explicit VR Little Endian (`1.2.840.10008.1.2.1.98`) | yes | yes | default; in-tree frame assembly | none; project license | Go-supported platforms |
| JPEG Baseline Process 1 (`1.2.840.10008.1.2.4.50`) | yes (SOF0, 8-bit) | yes (SOF0, 8-bit; explicit sampling/Huffman options) | default Go standard-library JPEG; opt-in local encoder | Go standard library / BSD-3-Clause; local encoder / project license | Go-supported platforms |
| JPEG Extended Process 2/4 (`1.2.840.10008.1.2.4.51`) | yes (Process 2 SOF1 8-bit; Process 4 SOF1 12-bit monochrome and color) | yes (explicit pure-Go SOF1: strict mono8/12; opt-in RGB8/RGB12/YBR_FULL12 4:4:4 compatibility; historical reduced mono9-11; lossy authorization) | default; Go standard library for 8-bit, in-tree pure-Go decoder for 12-bit and in-tree pure-Go SOF1 encoder | Go standard library / BSD-3-Clause and project license | Go-supported platforms |
| JPEG Spectral Selection Process 6 & 8, retired (`1.2.840.10008.1.2.4.53`) | yes (8-bit SOF2; qualified unsigned mono/RGB12 4:4:4) | no | default; Go standard-library 8-bit and in-tree pure-Go 12-bit | Go standard library / BSD-3-Clause and project license | Go-supported platforms |
| JPEG Full Progression Process 10 & 12, retired (`1.2.840.10008.1.2.4.55`) | yes (8-bit SOF2; qualified unsigned mono/RGB12 4:4:4) | no | default; Go standard-library 8-bit and in-tree pure-Go 12-bit | Go standard library / BSD-3-Clause and project license | Go-supported platforms |
| JPEG Lossless Process 14 (`1.2.840.10008.1.2.4.57`) | yes | yes (explicit pure-Go encoder, predictors 1-7) | default; pure-Go decoder and encoder | none; project license | Go-supported platforms |
| JPEG Lossless Process 14 SV1 (`1.2.840.10008.1.2.4.70`) | yes | yes (explicit pure-Go encoder) | default; pure-Go decoder and encoder | none; project license | Go-supported platforms |
| RLE Lossless (`1.2.840.10008.1.2.5`) | yes | yes | default; pure-Go codec | none; project license | Go-supported platforms |
| Deflated Image Frame Compression (`1.2.840.10008.1.2.8.1`) | yes (1-bit and 8-64-bit samples; decoded size bounded by the frame size) | yes (explicit pure-Go encoder, one raw DEFLATE stream per frame) | default; pure-Go codec (`pixeldata/deflateframe`) on Go `compress/flate` | Go standard library / BSD-3-Clause and project license | Go-supported platforms |
| JPEG-LS Lossless (`1.2.840.10008.1.2.4.80`) | yes | yes | default pure-Go decoder; explicit pure-Go encoder | none; project license | Go-supported platforms |
| JPEG-LS Near-Lossless (`1.2.840.10008.1.2.4.81`) | yes (qualified unsigned profiles; signed monochrome as unsigned codes) | yes (explicit pure-Go NEAR and lossy authorization, unsigned ILV=0, RGB ILV=1/2) | default: pure-Go decoder; `codecfull` / `jpegls_charls`: CharLS dynamic adapter | base: none; optional CharLS 2.4.2 / BSD-3-Clause and github.com/ebitengine/purego v0.10.1 / Apache-2.0 | codecfull decode: Windows/amd64, macOS/arm64; new encode oracle: Windows/amd64 |
| JPEG 2000 Part 1 Lossless (`1.2.840.10008.1.2.4.90`) | yes | yes (explicit pure-Go encoder, 1-16 stored bits, signed or unsigned, optional RCT; explicit optional OpenJPEG unsigned 8..16-bit encoder) | default: explicit pure-Go encoder (`pixeldata/jpeg2000`); `codecfull`: OpenJPEG; `jpeg2000_openjpeg`: explicit encoder; optional `jpeg2000`: pure Go decode, plus the opt-in `RegisterCore` Part 1 backend (every Part 1 coding option, Maxshift RGN included, corpus-qualified against OpenJPEG, [profile decision](../examples/codec-adapters/jpeg2000/PRODUCTION_PROFILE.md#core-backend-decision-934)) | OpenJPEG 2.5.4 / BSD-2-Clause; optional go-jpeg2000 / Apache-2.0; core adapted from amrshadid/go-dicom v1.6.0 / MIT; pure-Go encoder: project license | decode codecfull: Windows/amd64, macOS/arm64; OpenJPEG encode: Windows/amd64, Linux/amd64; pure-Go encode: Go-supported platforms, oracle-qualified on macOS/arm64; pure-Go decode: Go-supported platforms; core qualification: Windows/amd64 |
| JPEG 2000 Part 1 Lossy (`1.2.840.10008.1.2.4.91`) | yes | yes (explicit pure-Go 9/7 encoder with a quantization step and lossy authorization; explicit optional OpenJPEG encoder: rates or PSNR layers, tiles, resolutions, 9/7 or truncated 5/3) | default: explicit pure-Go encoder (`pixeldata/jpeg2000`); `codecfull`: OpenJPEG; `jpeg2000_openjpeg`: explicit lossy encoder; optional `jpeg2000`: pure Go, plus the opt-in `RegisterCore` Part 1 backend (every Part 1 coding option, Maxshift RGN included, corpus-qualified against OpenJPEG, [profile decision](../examples/codec-adapters/jpeg2000/PRODUCTION_PROFILE.md#core-backend-decision-934)) | OpenJPEG 2.5.4 / BSD-2-Clause; optional go-jpeg2000 / Apache-2.0; core adapted from amrshadid/go-dicom v1.6.0 / MIT; pure-Go encoder: project license | codecfull: Windows/amd64, macOS/arm64; pure-Go profile: Go-supported platforms; pure-Go encode oracle: macOS/arm64; core qualification: Windows/amd64 |
| JPEG 2000 Part 2 Lossless (`1.2.840.10008.1.2.4.92`) | yes, bounded profiles | opt-in bounded collection | `codecfull`: OpenJPEG; optional `jpeg2000`: raw-profile decode; explicit pure-Go collection encoder and `RegisterCorePart2` decoder; [scope](JPEG2000_PART2_ENCODER.md) | OpenJPEG 2.5.4 / BSD-2-Clause; optional go-jpeg2000 / Apache-2.0; no encoder dependency | Windows/amd64 exact original Part 10 decoding by independent pydicom + Python Annex J + OpenJPEG pipeline; off-the-shelf full Annex J interoperability pending |
| JPEG 2000 Part 2 Lossy (`1.2.840.10008.1.2.4.93`) | yes, bounded profiles | opt-in bounded collection | `codecfull`: OpenJPEG; optional `jpeg2000`: raw-profile decode; explicit pure-Go collection encoder and `RegisterCorePart2` decoder; [scope](JPEG2000_PART2_ENCODER.md) | OpenJPEG 2.5.4 / BSD-2-Clause; optional go-jpeg2000 / Apache-2.0; no encoder dependency | Windows/amd64 original Part 10 independent composed-decoder error bound, using unrounded spatial coefficients; off-the-shelf full Annex J interoperability pending |
| HTJ2K Lossless (`1.2.840.10008.1.2.4.201`) | yes | yes (explicit pure-Go encoder, reversible, HT cleanup pass) | default: explicit pure-Go encoder (`pixeldata/jpeg2000`); `codecfull`: OpenJPH; optional `jpeg2000`: pure Go, plus the opt-in `RegisterCoreHTJ2K` backend for HT cleanup-pass code-blocks | OpenJPH 0.31.0 / BSD-2-Clause; optional go-jpeg2000 / Apache-2.0; pure-Go encoder: project license with the CxtVLC codebook from go-jpeg2000 (Apache-2.0) | codecfull: Windows/amd64, macOS/arm64; pure-Go profile: Go-supported platforms; pure-Go encode oracle (OpenJPEG 2.5.4, OpenJPH 0.32.0): macOS/arm64 |
| HTJ2K Lossless RPCL (`1.2.840.10008.1.2.4.202`) | yes | yes (explicit pure-Go encoder, reversible, RPCL with TLM, HT cleanup pass) | default: explicit pure-Go encoder (`pixeldata/jpeg2000`); `codecfull`: OpenJPH; optional `jpeg2000`: pure Go, plus the opt-in `RegisterCoreHTJ2K` backend for HT cleanup-pass code-blocks | OpenJPH 0.31.0 / BSD-2-Clause; optional go-jpeg2000 / Apache-2.0; pure-Go encoder: project license with the CxtVLC codebook from go-jpeg2000 (Apache-2.0) | codecfull: Windows/amd64, macOS/arm64; pure-Go profile: Go-supported platforms; pure-Go encode oracle (OpenJPEG 2.5.4, OpenJPH 0.32.0): macOS/arm64 |
| HTJ2K Lossy (`1.2.840.10008.1.2.4.203`) | yes | yes (explicit pure-Go encoder, irreversible 9/7 with a quantization step and lossy authorization, HT cleanup pass) | default: explicit pure-Go encoder (`pixeldata/jpeg2000`); `codecfull`: OpenJPH; optional `jpeg2000`: pure Go, plus the opt-in `RegisterCoreHTJ2K` backend for HT cleanup-pass code-blocks | OpenJPH 0.31.0 / BSD-2-Clause; optional go-jpeg2000 / Apache-2.0; pure-Go encoder: project license with the CxtVLC codebook from go-jpeg2000 (Apache-2.0) | codecfull: Windows/amd64, macOS/arm64; pure-Go profile: Go-supported platforms; pure-Go encode oracle (OpenJPEG 2.5.4, OpenJPH 0.32.0): macOS/arm64 |
| JPEG XL Lossless (`1.2.840.10008.1.2.4.110`) | yes (pure Go: modular frames exactly, 1-16 bits, signed monochrome via offset-binary level shift) | yes (explicit pure-Go encoder: 1-16 stored bits, signed or unsigned monochrome and RGB) | default: explicit pure-Go codec (`pixeldata/jpegxl`); `codecfull` / optional `jpegxl_djxl`; command adapter | pure Go: project license (embedded Brotli dictionary, MIT); libjxl djxl 0.11.2 / BSD-3-Clause with patent grant | pure Go: Go-supported platforms, oracle-qualified (libjxl 0.12.0) on macOS/arm64; codecfull: Windows/amd64, macOS/arm64 |
| JPEG XL JPEG Recompression (`1.2.840.10008.1.2.4.111`) | yes (pure Go: JPEG reconstruction, sequential and progressive, then the JPEG decoder) | yes (pure-Go codestream recompression from `.50`/`.51` and reconstruction back, `jpegxl.Recompressor`; sequential 8-bit Huffman JPEG) | default: explicit pure-Go codec (`pixeldata/jpegxl`); `codecfull` / optional `jpegxl_djxl`; command adapter | pure Go: project license (embedded Brotli dictionary, MIT); libjxl djxl 0.11.2 / BSD-3-Clause with patent grant | pure Go: Go-supported platforms, oracle-qualified (libjxl 0.12.0) on macOS/arm64; codecfull: Windows/amd64, macOS/arm64 |
| JPEG XL Lossy (`1.2.840.10008.1.2.4.112`) | yes (pure Go: modular, JPEG-compatible VarDCT and bounded 8-bit sRGB XYB DCT8 frames; wider XYB profiles need the adapter) | yes (explicit pure-Go 8-bit encoder at a quality, lossy authorization) | default: explicit pure-Go codec (`pixeldata/jpegxl`); `codecfull` / optional `jpegxl_djxl`; command adapter | pure Go: project license (embedded Brotli dictionary, MIT); libjxl djxl 0.11.2 / BSD-3-Clause with patent grant | pure Go: Go-supported platforms; XYB DCT8 oracle-qualified with libjxl 0.11.2 on Windows/amd64; earlier modular/JPEG oracle-qualified with libjxl 0.12.0 on macOS/arm64; codecfull: Windows/amd64, macOS/arm64 |

## Profiles and fail-closed behavior

- `default` is dependency-free at runtime. `pixeldata/builtin.NewRegistry`
  registers only the built-in decoders, including the qualified JPEG-LS
  Lossless subset; encoders are selected explicitly by transcode APIs. Missing
  compressed decoders return `pixeldata.ErrCodecNotFound`.
- Optional tags (`jpeg2000`, `jpeg2000_openjpeg`, `jpegls_charls`, and
  `jpegxl_djxl`) enable only their named adapters. A tag never proves that its
  external runtime is installed.
- `codecfull` is the release profile. Startup preflights CharLS, OpenJPEG,
  OpenJPH, and `djxl`, including qualified versions and packaging evidence. Any
  missing or incompatible runtime aborts registration; partial capability is
  never advertised.
- Choosing among registered backends is explicit and per frame: a
  `pixeldata.CodecRouter`, registered for one syntax like any codec, decodes
  each frame with the first of its backends that accepts it, falls back to the
  next on failure only as its policy allows (reported per frame, and as the
  `codec-backend-fallback` diagnostic), can compare sampled frames with an
  oracle backend without changing the output, and can hold each frame's
  working set in a shared `MemoryAdmission`. A router never adds a backend the
  selected profile did not register; see
  [Pixel transcoding](PIXEL_TRANSCODING.md#per-frame-codec-routing).
- `codec-capabilities.json` is generated from the schema-v2 manifest and ships
  with codecfull packages. Consumers must use its `directions`, dependencies,
  evidence, and readiness fields instead of inferring support from build tags.

JPEG Baseline is the 8-bit Process 1/SOF0 path. Its encoder preserves the
standard-library default; explicit options add optimized Huffman tables and
4:2:2/4:4:4 sampling. See [JPEG Baseline encoder](JPEG_BASELINE_ENCODER.md)
for API, independent decoding evidence and size/CPU/memory tradeoffs.

JPEG-LS HP1/HP2/HP3 is a separately qualified **nonstandard legacy
compatibility** profile, disabled by default. Explicit
`DecodeRecovery.JPEGLSHPColorTransforms` enables APP8 `mrfx` only for lossless
unsigned RGB8/RGB16, full-range precision and ILV=1/2. The pure-Go decoder and
optional CharLS lossless adapter report the transform and backend used.
Near-Lossless, other precisions, signed samples, unknown transforms and
incompatible headers remain refused. Standard encoders do not emit HP markers;
this compatibility does not establish universal DICOM JPEG-LS conformance.
See [HP profile and independent evidence](JPEGLS_HP_COMPATIBILITY.md).

Known producer planar/OW conventions, reversed RLE byte segments, wrong palette
tags and retired ARGB/CMYK/HSV conversions use separate explicit
[legacy pixel repair choices](LEGACY_PIXEL_REPAIRS.md). They retain strict defaults
and diagnose changes; they do not add a codec backend or claim that ambiguous
sample conventions can be detected automatically.

JPEG Extended keeps 8-bit
Process 2/SOF1 decoding on the Go standard library and uses the in-tree pure-Go
Process 4/SOF1 path for 12-bit codestreams with monochrome or three-component
color samples. Process 4 supports BitsAllocated=16, BitsStored=9–12, and
HighBit=BitsStored-1. MONOCHROME1 and MONOCHROME2 support unsigned or signed
samples; RGB, YBR_FULL, and YBR_FULL_422 support unsigned samples with any
codestream sampling factors. Output is interleaved little-endian, with color
interpretation determined by the JPEG color policy (RGB by default). Frames
may span multiple Items through the
[shared bounded assembly](ENCAPSULATED_FRAME_ASSEMBLY.md); see
[Pixel Data Codecs](CONFORMANCE.md#pixel-data-codecs) for the full decode
subset.

For a standard monochrome Process 4 scan, IDCT emits clipped block rows into
the packed little-endian raster. Color and recovered scan parameters use
padded component planes. Compare small and odd-sized large monochrome
frames with `go test ./pixeldata/jpeg -run '^$' -bench BenchmarkJPEGExtendedDecodeBuffers -benchmem`.
The request limit charges these rasters together with the compressed input
and retained frames. Color admission includes the padded and upsampled
planes before allocating them.

JPEG Extended encoding is an explicitly registered pure-Go SOF1 encoder.
The options constructor (`jpeg.RegisterExtendedEncoderWithOptions`) defaults to
unsigned mono8/12 with absent PlanarConfiguration. RGB8/RGB12/YBR_FULL12 needs
`AllowColorCompatibility`, interleaved 4:4:4, and emits the declared
`jpeg-extended-color` profile: PS3.5 Table 8.2.1-1 does not list color for `.51`.
The historical constructor retains mono9-12; mono9-11 reports
`jpeg-extended-reduced-precision`. All require explicit quality and lossy
authorization, finite resource budgets, cancellation and consistent frame
profiles. Stored YBR error verification needs `JPEGColorNever`. See
[JPEG Extended encoder](JPEG_EXTENDED_ENCODER.md) for contracts and current
Windows/amd64 DCMTK/GDCM qualification, including installed GDCM CLI color12
limitations and the independent public-API oracle.

The optional JPEG 2000 adapter's `Register` and `RegisterCore` accept unsigned
`PALETTE COLOR` (8 or 16 allocated bits) under lossless-only `.90`, preserving
indices and LUTs. Other registered JPEG 2000 syntaxes do not gain that palette
profile. This enables lossless Icon Image Sequence transcoding; synthetic
round trips and opt-in gdcmData Kodak/Siemens icon fixtures exercise it.

The core backend behind `Register`, `RegisterCore` and `New` also decodes
`YBR_FULL_422` frames, which PS3.5 does not list for JPEG 2000 but some
writers produce, as GDCM does. The codestream must hold three unsigned 8-bit
components without a component transform: component 0 at full resolution and
components 1 and 2 at XRsiz=2, YRsiz=1, so an odd width has ceil(Columns/2)
chroma columns. Chroma is repeated over the two columns each sample covers and
converted to interleaved RGB with the PS3.3 C.7.6.3.1.2 equations.
Decompression then writes `RGB` and Lossy Image Compression `01`. Other
sampling layouts, depths, and backends keep refusing `YBR_FULL_422`. The
shared fixture and its independent OpenJPEG-based oracle are in
`pixeldata/codecfixture/testdata/codecs/jpeg2000-ybr422`.

The retired JPEG Spectral Selection (`.53`) and Full Progression (`.55`)
syntaxes decode their 8-bit Huffman progressive (SOF2) frames, grayscale and
color, through the Go standard library, with the JPEG Baseline metadata rules
and color policy. Every frame must be SOF2; under `.53` the successive
approximation scans of a full-progression stream are decoded too, as libjpeg
does. The strict in-tree 12-bit profile supports unsigned mono/RGB 4:4:4,
BA16/BS12/HB11, spectral-only `.53` and full-progression `.55`, with complete
scan coverage, bounded request work and cancellation. Sequential and arithmetic
frames, and 12-bit sampling/color/precision outside that subset, are refused.
See [12-bit progressive profile and independent corpus](JPEG_PROGRESSIVE12.md).
Samples are within 2 levels of the independent decoder references.

JPEG Lossless Process 14 supports one- and three-component SOF3 streams at 8
or 16 allocated bits. SOF3 precision must equal BitsStored and HighBit must be
BitsStored-1: 2-8 bits when 8 bits are allocated or 2-16 bits when 16 bits are
allocated; one-bit samples are outside the builtin subset. Three-component
objects are limited to unsigned RGB or YBR_FULL, full-resolution 1x1 component
sampling, PixelRepresentation=0, and PlanarConfiguration=0; decoded native
samples are interleaved. The codestream may use one interleaved
three-component scan or three single-component scans in arbitrary component
order. Predictors 1-7 and point transforms from zero through precision-1 are
accepted per scan; the SV1 transfer syntax requires predictor 1 in every scan.
Component identifiers, dimensions, sampling, scan coverage, and tables are
validated before decoded frames are published. The shared assembly supports multiple Items per frame using BOT or
unambiguous codestream boundaries; EOT remains restricted to one Item per frame.

`jpeglossless.RegisterEncoder` registers an explicit pure-Go encoder for SV1
(`.70`) and for Process 14 (`.57`, predictor 1); `NewProcess14Encoder` selects
predictors 2-7. It writes one interleaved Huffman scan per frame with
per-frame optimal tables (T.81 K.2, 16-bit code limit), point transform 0, and
accepts 8 or 16 allocated bits with 2-16 stored bits, signed or unsigned
MONOCHROME1/MONOCHROME2, and unsigned interleaved RGB or YBR_FULL. `dcmdjpeg`
decodes every predictor bit-exactly (`DICOMGO_DCMTK_INTEGRATION=1`), and the
output is within 0.1% of `dcmcjpeg +e1` in size.

The builtin JPEG-LS Lossless decoder supports NEAR=0 and ILV=0, plus unsigned
RGB ILV=1/2 with 2-16 stored bits in 8/16-bit containers, PlanarConfiguration=0,
matching precision and MAXVAL=2^precision-1. Default parameters and LSE ID=1
are accepted; see [the qualified profiles and full corpus](JPEGLS_INTERLEAVE.md).
Restart markers, NEAR>0, and color transforms remain outside this subset. DICOM
frame assembly is shared with the other JPEG codecs and optional CharLS adapter,
with populated Basic Offset Tables,
normative Extended Offset Table/Lengths pairs, and empty Basic Offset Tables
only when complete SOI/terminal-EOI boundaries make every frame unambiguous.
Independent pydicom/GDCM fixtures are compared bit-exactly; the pure-Go encoder
is additionally checked by a CharLS interoperability gate. With
`EncoderOptions.Interleave` the encoder writes RGB frames line (ILV=1) or
sample (ILV=2) interleaved, lossless or near-lossless; its scans are byte for
byte CharLS 2.4.2's for every interleaved stream of the codecfull corpora.

The builtin `.81` decoder separately qualifies unsigned MONOCHROME1/2 ILV=0
and RGB ILV=0/1/2, with explicit NEAR and source bounds; signed monochrome
decodes to its unsigned codes, which NEAR bounds (not the signed values at
the code midpoint). The `.80` codec remains
strictly lossless. See [Near-Lossless evidence and exclusions](JPEGLS_NEAR_LOSSLESS.md).
The separate [explicit Near-Lossless encoder](JPEGLS_NEAR_LOSSLESS_ENCODER.md)
uses the caller's NEAR bound and lossy policy; it is qualified for unsigned
MONOCHROME1/2 and RGB ILV=0 with all samples independently decoded by CharLS,
and for RGB ILV=1/2 by the entropy-coded bytes CharLS writes for the same
samples.

The explicit pure-Go JPEG 2000 and HTJ2K encoder (`jpeg2000.RegisterEncoders`
for `.90`, `.201` and `.202`; `jpeg2000.RegisterLossyEncoders` for `.91` and
`.203`) writes one tile, one quality layer and 64x64 code-blocks. Part 1
code-blocks use the MQ coder and HTJ2K ones a single HT cleanup pass. It
accepts 1-16 stored bits, signed or unsigned monochrome and RGB with an
optional RCT/ICT. OpenJPEG and OpenJPH decode its lossless output bit-exactly.
See [JPEG 2000 and HTJ2K encoder](JPEG2000_ENCODER.md) for the scope, oracle
evidence and the PSNR per quantization step.

## Reproducible checks

Run from `dicom-go`:

```sh
make codec-policy-check          # dependency and license guardrails
make codec-optional-check        # dependency-free optional-module tests
make codec-conformance-check     # synthetic codec corpus
make codec-manifest-check        # schema, evidence and readiness
make codecfull-check             # all runtimes; requires codecfull environment
go run ./cmd/dicom-codec-manifest -require-ready
```

`make check` includes every check that does not require native runtimes. See
[`CODECFULL_PROFILE.md`](CODECFULL_PROFILE.md) for pinned runtime acquisition,
packaging, platform gates, evidence, and performance reports.

The [full-sample corpus contract](PIXEL_CORPUS.md) describes fixture metadata,
hashes, independent reconstruction, per-component/spatial metrics and explicit
base/optional exclusions. Corpus presence alone does not qualify a decoder.
