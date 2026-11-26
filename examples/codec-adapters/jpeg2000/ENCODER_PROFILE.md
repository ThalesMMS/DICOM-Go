# OpenJPEG Part 1 lossless and lossy encoders

This optional nested module implements `pixeldata.FrameEncoder` for
lossless `1.2.840.10008.1.2.4.90` and lossy `1.2.840.10008.1.2.4.91`
transfer syntaxes. Enable `jpeg2000_openjpeg` or `codecfull` and
explicitly register an encoder:

```go
registry := pixeldata.NewMemoryEncoderRegistry()
err := jpeg2000.RegisterOpenJPEGLosslessEncoder(ctx, registry,
    jpeg2000.OpenJPEGEncoderOptions{Executable: "/path/to/opj_compress"})
// Check err before passing registry as TranscodeOptions.EncoderRegistry.
```

`NewOpenJPEGLosslessEncoder` is also available for direct use. Constructor
preflight checks the actual `opj_compress` product and OpenJPEG 2.5.4 version.
Discovery uses the explicit option, `DICOM_GO_OPENJPEG_COMPRESS`, then PATH.
Missing runtime or an untagged build returns `ErrOpenJPEGEncoderUnavailable`.
Version tokens must match exactly; prefixes such as 2.5.40 or 2.5.4.1 are
rejected by the shared encoder/decoder preflight.
Decoder registration does not enable or require this encoder. Base builds do
not acquire this module or a native dependency; no global encoder registry is
modified. `.91` has its own lossy encoder (below); Part 2 and JPEG XL encode
directions remain absent here. The base module's dependency-free pure-Go
encoder (`pixeldata/jpeg2000`) covers `.90`, `.91` and HTJ2K `.201`-`.203`
without OpenJPEG; see
[JPEG 2000 and HTJ2K encoder](../../../docs/JPEG2000_ENCODER.md).

## Sample profile and pipeline

- Unsigned MONOCHROME1, MONOCHROME2 or RGB; one or three components.
- 8 or 16 allocated bits, **8 through 16 stored bits**, no more than allocation,
  and `HighBit = BitsStored - 1`. Nonzero unused bits are rejected.
- Canonical little-endian, interleaved input; RGB requires Planar Configuration
  0. Signed, palette, YBR, sub-8-bit precision and other layouts fail explicitly.
- Single or multiple frames via the existing transcoder. The adapter returns
  one raw codestream per frame; the existing pipeline owns fragmentation, BOT/
  EOT, metadata/identity policy and complete-file publication.

The adapter supplies a binary PGM/PPM raster to OpenJPEG. PNM's 16-bit raster
uses big-endian storage; the adapter changes byte packing only, checks every
sample against stored precision, and never scales values. An 8-bit stored
sample in a 16-bit allocation remains the same value. `-r 1`, the default
reversible 5/3 transform, and explicit `-mct 0` retain exact samples and RGB
photometry. No irreversible transform or color conversion is selected. The
result must be a complete SOC/EOC codestream and pass the existing JPEG 2000
metadata parser's geometry, precision, component and Part 1 checks.

The binary PNM route is deliberate: OpenJPEG 2.5.4's RAW reader has a bitwise
width/height/component/precision validation error that rejects certain valid
odd combinations. Its PNM reader also raises precision below eight bits, so
this adapter **rejects sub-8-bit input** instead of misreporting precision.
Neither runtime patching nor a second JPEG 2000 parser is introduced. The exact
upstream implementation inspected was [OpenJPEG 2.5.4, commit
6c4a29b](https://github.com/uclouvain/openjpeg/blob/6c4a29b00211eb0430fa0e5e890f1ce5c80f409f/src/bin/jp2/convert.c).

## Lossy `.91` encoder

`NewOpenJPEGLossyEncoder` / `RegisterOpenJPEGLossyEncoder(ctx, registry,
opts, coding)` register `1.2.840.10008.1.2.4.91` with the same runtime
preflight, sample profile, PNM route, limits, cancellation and output checks
as the lossless encoder. `OpenJPEGLossyOptions` selects the coding, like
`opj_compress` (and `gdcmconv -K -r -q -t -n --irreversible`):

- `Rates`: one compression ratio per quality layer, decreasing (`-r`); or
  `Quality`: one target PSNR in dB per layer, increasing (`-q`); exactly one.
  OpenJPEG counts a ratio against the stored precision, so a 12-bit image in
  16-bit containers is compressed further than the ratio says against its
  native bytes.
- `TileWidth`/`TileHeight` (`-t`), `Resolutions` (`-n`, zero picks up to six,
  every tile side must hold 2^(n-1) samples) and `Progression` (`-p`, LRCP by
  default).
- The irreversible 9/7 wavelet (`-I`) unless `Reversible` asks for the 5/3
  one, whose codestream the rates then truncate.
- RGB goes through the component transform, and the frame's Photometric
  Interpretation becomes `YBR_ICT` (`YBR_RCT` when reversible), unless
  `KeepRGB` keeps it RGB (`-mct 0`).

The encoder declares the Lossy Image Compression Method `ISO_15444_1`; the
transcoder requires `AllowLossy`, sets Lossy Image Compression to `01` and
appends the ratio and method to their history, as for lossy JPEG.


| Image | Coding | Ratio | PSNR |
| --- | --- | ---: | ---: |
| 12-bit mono 256x192 | 80:1 | 105.6:1 | 29.6 dB |
| 12-bit mono 256x192 | 40:1 | 57.6:1 | 35.7 dB |
| 12-bit mono 256x192 | 20:1 | 27.0:1 | 43.1 dB |
| 12-bit mono 256x192 | 10:1 | 13.4:1 | 52.4 dB |
| 12-bit mono 256x192 | 5:1 | 6.7:1 | 66.2 dB |
| 12-bit mono 256x192 | layers 40:1, 10:1; 64x64 tiles; 4 resolutions; RPCL | 13.4:1 | 44.9 dB |
| 12-bit mono 256x192 | PSNR 45 dB | 23.4:1 | 44.9 dB |
| 12-bit mono 256x192 | reversible 5/3 truncated at 20:1 | 27.2:1 | 38.2 dB |
| 8-bit RGB 160x120 (YBR_ICT) | 80:1 | 79.0:1 | 34.5 dB |
| 8-bit RGB 160x120 (YBR_ICT) | 40:1 | 39.6:1 | 44.6 dB |
| 8-bit RGB 160x120 (YBR_ICT) | 20:1 | 19.9:1 | 53.3 dB |
| 8-bit RGB 160x120 (YBR_ICT) | 10:1 and 5:1 | 16.1:1 | 55.2 dB |
| 8-bit RGB 160x120 (YBR_ICT) | layers 40:1, 10:1; 64x64 tiles; 4 resolutions; RPCL | 11.8:1 | 52.5 dB |
| 8-bit RGB 160x120 (YBR_ICT) | PSNR 45 dB | 40.9:1 | 44.2 dB |
| 8-bit RGB 160x120 (YBR_RCT) | reversible 5/3 truncated at 20:1 | 20.0:1 | 45.2 dB |
| 8-bit RGB 160x120 (RGB kept) | 20:1 | 19.9:1 | 44.7 dB |

The smooth synthetic colour image reaches the encoder's full quality near 16:1,
so the 10:1 and 5:1 requests stop there. The figures describe these images
only; they are not a diagnostic quality claim. A JPEG Extended 12-bit lossy
encoder (the optional sibling of this item) is not provided.

## Resources and failure behavior

| Resource | Default | Maximum configurable value |
| --- | --- | --- |
| Native frame bytes | 16 MiB | 64 MiB |
| Encoded output bytes accepted | 32 MiB | 128 MiB |
| Time per call, including queue wait | 30 seconds | 2 minutes |
| Active processes per shared encoder | 1 | 4 |
| OpenJPEG threads per process | 1 | Fixed |
| Captured diagnostics | 16 KiB | Fixed |

The native allocation budget is bounded indirectly by validated geometry/frame
bytes, one tile and thread count; it is not a process RSS quota. Output size is
checked before reading into memory. The trusted, pinned runtime may write its
bounded image output before that check; there is no operating-system disk quota.
Calls use separate private temporary directories and fixed filenames, with
cleanup after success, native error or cancellation. Input slices are borrowed
read-only; returned bytes are owned. Keep the runtime and temporary namespace
under local access controls. Backend output and paths are omitted from errors.

Cancellation terminates and waits for the single native subprocess; it does not
pretend that an in-process OpenJPEG C call can be interrupted. Waiting for the
per-instance concurrency gate also observes the deadline. Resource, metadata,
runtime and invalid-output errors remain classifiable through typed/sentinel
errors. The transcoder receives no encoded result on failure and preserves its
source. No file publication is performed by the encoder itself.

## Independent qualification

Run from this nested module:

```sh
go test ./...
go test -race -tags jpeg2000_openjpeg -run TestOpenJPEGEncoder -count=1 ./...
DICOMGO_J2K_ENCODER_INTEROP=1 DICOM_GO_OPENJPEG_COMPRESS=/path/to/opj_compress DICOMGO_FFMPEG=/path/to/ffmpeg go test -race -tags jpeg2000_openjpeg -run TestOpenJPEGLosslessEncoderIndependentFFmpeg -count=1 -v
```

Opt-in means missing tools fail the selected interop profile; they are never
reported as successful qualification. The default suite skips that external
profile. Fault tests build a local synthetic helper and require no OpenJPEG.

The shared `codecfixture.JPEG2000LosslessEncoderCases` corpus contains 21 cases
and two frames per case: every precision 8..16, mono/RGB, extrema and varying
samples, odd geometry, 1x1/1x31 images, 129x131 RGB, and stored/allocation width
differences. Each case runs native-to-.90 through the real transcoder, checks
metadata and source preservation, assembles the emitted frames, then compares
**all 141974 samples across 42 frames** with exact policy.

The verifier explicitly selects FFmpeg's **native `jpeg2000` decoder**, not its
`libopenjpeg` wrapper. FFmpeg's raw display raster is left-aligned in the chosen
8/16-bit output format; the shared sample comparator accounts for this declared
layout when comparing stored samples. OpenJPEG is the encoder implementation
and FFmpeg native is the independent decoder implementation.


| Component | Platform/version | SHA-256 of executable |
| --- | --- | --- |
| Official OpenJPEG encoder | Windows/amd64, 2.5.4 | `f09fa59016677a6dd42b244d40ff01cd17f1fabab7b7730c7c42c25fcd8176df` |
| Source-built OpenJPEG encoder | Linux/amd64, 2.5.4, pinned commit above | `c0d9700ee42196f675ab574a4868fcea1f10e50702ad4b6a04d447226574a707` |
| Native FFmpeg verifier | Linux/amd64, Debian `7:7.1.5-0+deb13u1` | `e8a8d46f5225f3062cec7c07fb145d58ae73c603cb740dcd5bad34bfb54e455a` |

The Windows encoder was independently checked through the Linux verifier in
WSL. To repeat that test from PowerShell, set
`$env:DICOMGO_J2K_ENCODER_INTEROP='1'`,
`$env:DICOM_GO_OPENJPEG_COMPRESS` to the Windows executable and
`$env:DICOMGO_FFMPEG_WSL='Debian'`. The test translates only its synthetic fixture
paths using `wslpath`; production code has no WSL dependency. macOS encoding
has not been qualified by this increment.


OpenJPEG is BSD-2-Clause; see [its existing runtime/packaging
profile](OPENJPEG_PROFILE.md). The source-built verifier package used here is
FFmpeg's Debian GPL-enabled build; it is a test tool and is not linked, shipped
or added to the library dependency graph. No upstream code or clinical fixture
was copied. Qualification establishes this sample/metadata subset, not every
IOD or a clinical retention/diagnostic guarantee.
