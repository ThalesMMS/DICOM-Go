# JPEG 2000 / HTJ2K optional adapter

This nested module provides the supported opt-in JPEG 2000 / HTJ2K pixel data
adapter for `dicom-go`. It stays outside the base module so applications that do
not decode JPEG 2000 do not inherit the JPEG 2000 dependency.

The adapter uses `github.com/mrjoshuak/go-jpeg2000` as a pure Go dependency. No
CGO, native library, or adapter-specific build tag is required by this module.
Applications may still place registration behind their own build tag when they
want default builds to avoid the optional dependency path.

The explicit OpenJPEG `.90` encoder, the lossy `.91` encoder with its rates,
PSNR layers, tiles, resolutions and wavelet choice, and their qualification
are documented in [ENCODER_PROFILE.md](ENCODER_PROFILE.md). Decoder
registration does not register an encoder or require `opj_compress`.

For caller-owned persistent OpenJPEG/OpenJPH pipe workers instead of launching
a CLI for each frame, see [NATIVE_HELPER.md](NATIVE_HELPER.md). This explicit
profile includes lifecycle limits, cancellation, fixture qualification and
paired codec-frame-cost evidence; existing default registrations are unchanged.

Supported decoder boundaries:

- registers JPEG 2000, JPEG 2000 Part 2, and HTJ2K still-image transfer syntax
  UIDs explicitly;
- excludes JPIP and streaming transfer syntaxes;
- validates common DICOM pixel metadata and codestream metadata before decode;
- supports one fragment per frame, plus multi-fragment assembly for a single
  frame;
- returns interleaved native frame bytes for supported 8-bit and 16-bit unsigned
  monochrome/RGB data;
- returns typed errors for unsupported metadata, unsupported fragment layouts,
  size mismatches, and malformed codestreams;
- implements `pixeldata.ContextCodec`. Cancelation and deadlines are returned
  as `context.Canceled` / `context.DeadlineExceeded` rather than image
  corruption. External `opj_decompress` and `ojph_expand` processes honor the
  tighter of the caller deadline and the codec timeout, kill their process group
  on Unix, wait for collection, and delete temporary files. The pure-Go
  `j2k.Decode` backend checks the context before and after decode; an in-flight
  native frame is not guaranteed to abort mid-codestream.

The profile decision and current benchmark evidence are documented in
[PRODUCTION_PROFILE.md](./PRODUCTION_PROFILE.md). In short, this module is the
supported dependency-free opt-in JPEG 2000 / HTJ2K adapter for the controlled
fixture-backed profile, but it is not enough by itself for the clinical
`codecfull` profile. That qualified profile combines this adapter boundary with
OpenJPEG for JPEG 2000 Part 1/2 and OpenJPH for HTJ2K, and validates external
clinical fixtures against independent references.

The OpenJPEG fallback profile is documented in
[OPENJPEG_PROFILE.md](./OPENJPEG_PROFILE.md). Build with `-tags
jpeg2000_openjpeg` or call `RegisterOpenJPEG` / `RegisterOpenJPEGDefault` to use
that profile. It registers OpenJPEG for JPEG 2000 Part 1 transfer syntaxes,
keeps JPEG 2000 Part 2 and HTJ2K on the pure-Go fallback path, requires
OpenJPEG's `opj_decompress` executable, and keeps the default `jpeg2000`
pure-Go path unchanged.

## Core backend (JPEG 2000 Part 1, pure Go)

`NewCore(CoreOptions)` / `RegisterCore(registry, options)` select a second
pure-Go backend adapted from the amrshadid/go-dicom v1.6.0 decoder (see
[`internal/j2kcore/PROVENANCE.md`](internal/j2kcore/PROVENANCE.md)). It registers
only `.90` and `.91` in the caller's registry; Part 2 stays with the backends
above. The default path uses this backend for everything but Part 2:
`Register` installs it for `.90`, `.91`, `.201`, `.202` and `.203`, and `New`
hands it every codestream whose CAP marker does not declare Part 2.
go-jpeg2000 only decodes its own output: OpenJPEG-compatible Part 1 streams
came out flat and OpenJPH HTJ2K misdecoded. A transform flag (SGcod MCT) on
a stream with fewer than three components is ignored, as OpenJPEG does.

- Frames follow the Basic/Extended Offset Tables or, without them, the
  codestream structure (`encapsulated.JPEG2000`: SOC, lengths, `Psot`, EOC);
  an Item is never assumed to be a frame, and each frame's framing is validated.
- Samples are read as numbers, never through `image.Image`: 8/16-bit
  containers, 1 or 3 components (inverse RCT/ICT gives RGB), signed samples
  stored as two's complement, no scaling, rescale or VOI.
- PS3.5 8.2.4 (2026d): the codestream controls decompression. A precision other
  than BitsStored is accepted only if it fits BitsAllocated, and every decoded
  sample must fit BitsStored/PixelRepresentation (`ErrSampleOutOfRange`). A
  signed codestream with PixelRepresentation 0 is refused; an unsigned one with
  PixelRepresentation 1 is refused unless `ReinterpretUnsignedAsSigned`
  explicitly asks for two's complement of BitsStored (`ErrSignednessMismatch`).
- A JP2 file is refused (`ErrJP2Wrapped`) unless `AllowJP2Wrapper` opts into
  that recovery. Every progression order and POC, user-defined precincts,
  every code-block style option, PPM/PPT packed headers and subsampled
  components decode, and so do regions of interest (RGN, Maxshift, ISO/IEC
  15444-1 Annex H) in the main or a tile-part header: coefficients from 2^s
  up are the region's and are scaled back down, smaller ones are background
  (testdata/rgn: `opj_compress -ROI` fixtures and synthetic Maxshift
  codestreams that mix region and background). RGN styles other than
  Maxshift, RGN on HT code-blocks, HT code-blocks outside
  `RegisterCoreHTJ2K`'s cleanup-pass profile and precision above 16 bits are
  refused by name (`ErrUnsupportedCodestreamFeature`).
- `DecodeCoreReduced(ctx, codestream, reduce, options)` decodes at 1/2^reduce
  of the size by dropping the finest resolution levels, for thumbnails and
  previews (a 512x512 CT frame: 33 ms in full, 1 ms at 1/8); asking for more
  levels than a tile-component has is `ErrInvalidReduction`.
- The core codec (Part 1 and the HTJ2K syntaxes alike) implements
  `pixeldata.FrameRegionDecoder`: a region of a frame
  (`pixeldata.FrameReader.Region`) decodes only the tiles it overlaps, so a
  region of a tiled image costs those tiles (one tile of the tiled corpus
  codestream: 33 µs against 694 µs for the image). Other backends return
  `pixeldata.ErrRegionUnsupported`, and the reader decodes and crops.
- The core codec also implements `pixeldata.PartialFrameDecoder`, for
  `pixeldata.FrameReader.DecodePartial`: one request combines a region, a
  reduction (the finest resolution levels dropped, ISO/IEC 15444-1 Annex F)
  and the first quality layers (the coding passes of later layers dropped,
  B.10). The packets the request does not need are not decoded, and reading
  stops after the last one it needs; a region still costs whole tiles.
  `PartialDecodeCapabilities` reports the fewest decomposition levels of any
  tile-component and the layer count. A reduction beyond those levels is
  `pixeldata.ErrInvalidReduction` (and `ErrInvalidReduction`), more layers
  than the codestream has `pixeldata.ErrInvalidQualityLayers`; an image
  origin that is not a multiple of 2^n on the output grid (SIZ offsets) is
  not reduced natively (`pixeldata.ErrPartialDecodeUnsupported`, so the
  reader averages). A 1/8 thumbnail of a 2048x2048 12-bit frame with five
  levels decodes in 4.5 ms and 4 MB against 389 ms and 189 MB to decode the
  whole frame and average it (`go test -run '^$' -bench CorePartialDecode`).
  Quality-layer and layer-plus-reduction decodes equal committed
  `opj_decompress -l`/`-r` output (`testdata/layers`). Other backends report
  no capabilities and return `pixeldata.ErrPartialDecodeUnsupported`.
  `Register` uses the core backend for HTJ2K only, so `RegisterCore` is what
  gives `.90/.91` partial decoding.
- `CoreLimits` bound input bytes, components, tiles, resolutions, output
  samples, per-tile work and code-blocks before allocation (`ErrResourceLimit`).
- Cancellation is checked at every tile, component, packet, code-block and
  wavelet level; an error or cancellation publishes no frame and the borrowed
  codestream is never retained or modified.

`RegisterCoreHTJ2K(registry, options)` registers the same backend for HTJ2K
(`.201`, `.202`, `.203`, ISO/IEC 15444-15): the CAP marker of Part 15 and HT
code-blocks (code-block style 0x40) coded by one HT cleanup pass, which is how
the base module's pure-Go encoder (`pixeldata/jpeg2000`) and OpenJPH write
lossless and non-truncated lossy streams. The block decoder is shared with
that encoder (`internal/htj2k` of the base module). HT refinement (SigProp,
MagRef) and placeholder passes, mixed HT/Part 1 code-blocks and Part 1 style
options on HT code-blocks are refused by name. It is separate from
`RegisterCore` so that a registry that keeps another HTJ2K backend is
unchanged. The OpenJPH-encoded pydicom fixtures `HTJ2KLossless_08_RGB` and
`HTJ2K_08_RGB` decode to their reference points; the pure-Go encoder's output
decodes exactly (`core_purego_encoder_test.go`).

The backend stays opt-in. Its qualification corpus, oracles, benchmarks and
the profile decision are in
[PRODUCTION_PROFILE.md](PRODUCTION_PROFILE.md#core-backend-decision-934).


Registration is explicit:

```go
import jpeg2000codec "github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000"

if err := jpeg2000codec.RegisterDefault(); err != nil {
	panic(err)
}
```

Run the adapter tests directly:

```sh
go test ./...
go test -bench BenchmarkDecodeJPEG2000Profile -benchmem ./...
go test -tags jpeg2000_openjpeg ./...
```
