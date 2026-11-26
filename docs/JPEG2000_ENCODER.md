# JPEG 2000 and HTJ2K encoder

The separate [Part 2 collection encoder](JPEG2000_PART2_ENCODER.md) applies
an explicitly selected Annex J transform across monochrome frames.

`pixeldata/jpeg2000` is a pure-Go encoder for JPEG 2000 Part 1 (ISO/IEC
15444-1) and High-Throughput JPEG 2000 (ISO/IEC 15444-15, HTJ2K). It is part of
the base module, has no dependency and needs no build tag or external runtime:

| Transfer syntax | Wavelet | Block coder | Progression | Lossy Image Compression Method |
|---|---|---|---|---|
| JPEG 2000 Lossless Only (`1.2.840.10008.1.2.4.90`) | reversible 5/3 | Part 1 MQ (EBCOT) | LRCP | — |
| JPEG 2000 (`1.2.840.10008.1.2.4.91`) | irreversible 9/7 | Part 1 MQ (EBCOT) | LRCP | `ISO_15444_1` |
| HTJ2K Lossless (`1.2.840.10008.1.2.4.201`) | reversible 5/3 | HT cleanup pass | LRCP | — |
| HTJ2K Lossless RPCL (`1.2.840.10008.1.2.4.202`) | reversible 5/3 | HT cleanup pass | RPCL, with a TLM marker segment | — |
| HTJ2K (`1.2.840.10008.1.2.4.203`) | irreversible 9/7 | HT cleanup pass | LRCP | `ISO_15444_15` |

The encoder is written from the ISO/IEC 15444-1 and 15444-15 descriptions.
No OpenJPEG, OpenJPH, Kakadu or GDCM code is used; OpenJPEG and OpenJPH are
opt-in decoding oracles. The one exception is data: the HT cleanup pass needs
the normative CxtVLC codebook of 15444-15, whose codewords were taken from
the decoder lookup tables of go-jpeg2000 v1.3.0 (Apache-2.0, derived from
OpenJPEG). The provenance is recorded in
[THIRD_PARTY_PROVENANCE.md](THIRD_PARTY_PROVENANCE.md).

## Registration

Nothing is registered by package initialization. Callers add the encoders
they want to their own `pixeldata.EncoderRegistry`:

```go
encoders := pixeldata.NewMemoryEncoderRegistry()
if err := jpeg2000.RegisterEncoders(encoders); err != nil { // .90, .201, .202
	return err
}
if err := jpeg2000.RegisterLossyEncoders(encoders, jpeg2000.LossyOptions{
	StepSize: 2, AllowLossy: true, // .91 and .203
}); err != nil {
	return err
}
```

`jpeg2000.NewEncoder(jpeg2000.Options{...})` builds one encoder for direct
use. The options are:

- `ColorTransform` applies the reversible (RCT) or irreversible (ICT)
  component transform to RGB. The encoded frame then reports Photometric
  Interpretation `YBR_RCT` or `YBR_ICT` with Planar Configuration 0, which the
  transcoder applies to the output. Without it colour stays `RGB`.
- `Levels` sets the number of decomposition levels. Zero chooses up to five,
  fewer for a frame too small to hold them.
- `StepSize` is the irreversible quantization step in stored-sample units,
  required for the lossy syntaxes, from just above 0 to 65536. Each subband's
  step is `StepSize` divided by the L2 norm of its synthesis basis, so every
  quantization index contributes about the same error to the image. The
  reconstruction error is roughly `StepSize/√12` RMS on detailed content.
- `AllowLossy` must be set for `.91` and `.203`. The transcoder separately
  requires `TranscodeOptions.AllowLossy`, then records Lossy Image Compression
  `01`, the ratio and the method in the output.

The transcoder's lossless verification decodes the output, so it needs a
decoder for the target syntax. The pure-Go core backend of the optional
[JPEG 2000 adapter](../examples/codec-adapters/jpeg2000/README.md) decodes
every codestream this encoder writes: `RegisterCore` for `.90`/`.91` and
`RegisterCoreHTJ2K` for `.201`–`.203`. OpenJPEG and OpenJPH adapters decode
them too.

## Accepted input

| Bits Allocated | Bits Stored | Samples per Pixel | Pixel Representation | Photometric Interpretation |
|---:|---:|---:|---:|---|
| 8 or 16 | 1 to Bits Allocated, High Bit = Bits Stored − 1 | 1 | 0 or 1 | `MONOCHROME1`, `MONOCHROME2`, `PALETTE COLOR` (lossless and unsigned only) |
| 8 or 16 | 1 to Bits Allocated, High Bit = Bits Stored − 1 | 3 | 0 | `RGB`, interleaved (Planar Configuration 0) |

Bits above Bits Stored are ignored; signed samples are sign-extended from
Bits Stored. Every other combination fails before encoding with
`pixeldata.ErrUnsupportedEncoderMetadata`. Each frame becomes one bare
codestream (no JP2 box); the transcoder owns fragmentation and offset tables.

## Codestream

- One tile covering the frame, with image and tile origin 0, and no
  subsampling. The SIZ precision and signedness are Bits Stored and Pixel
  Representation, and unsigned samples are DC level shifted.
- 64×64 code-blocks, maximal precincts and a single quality layer. There are
  no SOP or EPH markers.
- Reversible syntaxes use the 5/3 wavelet without quantization (QCD style 0).
  Each subband's exponent is the precision plus its gain, plus one under RCT.
  There are at least two guard bits, raised if a coefficient needs more
  bit-planes.
- Irreversible syntaxes use the 9/7 wavelet with expounded scalar quantization
  (QCD style 2). Every quantization index is coded. There is no rate control,
  layer truncation or PCRD: quality and size follow from `StepSize`.
- Part 1 code-blocks code every bit-plane with the significance, refinement
  and cleanup passes (D.3) through the MQ coder (Annex C). All passes form
  one codeword segment with no code-block style option.
- HTJ2K code-blocks are coded completely by one HT cleanup pass at bit-plane
  zero, with MEL, VLC and MagSgn streams and the zero bit-plane count
  Mb − 1. SIZ Rsiz has the Part 15 bit set, CAP declares Part 15 (`Ccap15`
  with the reversible flag and MAGB), and COD has code-block style 0x40.
  SigProp and MagRef refinement passes are never written.
- `.202` uses the RPCL progression and adds a TLM marker segment. With one
  layer and one precinct per resolution, its packets come in the same order
  as LRCP.

Code-blocks are coded in parallel across `GOMAXPROCS` goroutines. The output
depends only on the input: the same frame always gives the same bytes.

## Validation

`go test ./pixeldata/jpeg2000 ./internal/htj2k` runs in `make check` and
needs no external tool. It covers:

- option, metadata and registration errors, capabilities and cancellation;
- the exact inverse of the 5/3 transform and RCT, the 9/7 round trip and
  its subband norms;
- packet-header primitives (pass counts, tag trees, bit stuffing) and the
  marker segments of each syntax;
- deterministic output under concurrent use;
- an HT block-coder round trip over sizes from 1×1 to 1024×4 and magnitudes
  up to 2^30, an exhaustive check of the VLC codebook, MEL round trips and
  `FuzzDecodeCleanup`.

The optional JPEG 2000 adapter module decodes the encoder's output with its
core backend in `make check` (`examples/codec-adapters/jpeg2000`,
`core_purego_encoder_test.go`):

- The 21 cases of `codecfixture.JPEG2000LosslessEncoderCases` go through the
  real transcoder to each lossless syntax, with and without RCT. That is 192
  frames and 821544 samples, all bit-exact.
- The repository's fixtures are re-encoded to each lossless syntax and decode
  to the same samples as before. They are the native `693_UNCR`,
  `emri_small`, `US1_UNCI` and `MR_small`, the JPEG 2000 `693_J2KR` and
  `emri_small_jpeg_2k_lossless`, and the OpenJPH-encoded
  `HTJ2KLossless_08_RGB` and `HTJ2K_08_RGB`.
- The lossy syntaxes record their compression method and stay within their
  error bounds.

Opt-in independent decoders (`DICOMGO_J2K_ORACLE=1`; executables from
`DICOM_GO_OPENJPEG_DECOMPRESS` and `DICOM_GO_OPENJPH_EXPAND`, otherwise
`PATH`; a missing one fails the test):


Compression and PSNR (peak 2^BitsStored − 1) of the lossy syntaxes on
synthetic band-limited frames with mild noise, from the opt-in run:

| Frame | Syntax | StepSize | Ratio | PSNR |
|---|---|---:|---:|---:|
| 12-bit mono 256×192 | `.91` | 2 | 2.7:1 | 75.3 dB |
| 12-bit mono 256×192 | `.91` | 8 | 4.5:1 | 62.3 dB |
| 12-bit mono 256×192 | `.91` | 32 | 15.4:1 | 48.8 dB |
| 12-bit mono 256×192 | `.203` | 32 | 14.0:1 | 48.8 dB |
| 8-bit RGB 160×120, ICT | `.91` | 2 | 15.8:1 | 45.7 dB |
| 8-bit RGB 160×120, ICT | `.91` | 8 | 49.1:1 | 42.7 dB |
| 8-bit RGB 160×120, ICT | `.203` | 8 | 38.9:1 | 42.7 dB |
| 8-bit RGB 160×120, ICT | `.203` | 32 | 71.1:1 | 35.1 dB |

The figures describe these synthetic frames only and are not a diagnostic
quality claim. `BenchmarkEncode512Mono12` measures each syntax on a 512×512
12-bit frame. On an Apple M4 Pro, HTJ2K lossless takes about 8 ms and Part 1
lossless about 19 ms.

## Not implemented

- Rate control and quality layers: there is no target ratio or PSNR, one
  layer only, and no truncation of a reversible stream.
- Tiles, user-defined precincts, code-block sizes other than 64×64,
  progression orders other than LRCP and RPCL, POC, PPM/PPT and SOP/EPH.
- Part 1 code-block style options (bypass, termination, causal contexts) and
  region of interest.
- HT refinement passes (SigProp and MagRef), multiple HT sets and mixed HT
  and Part 1 code-blocks.
- JP2/JPH wrappers, more than 16
  stored bits, subsampled components and YBR inputs.
