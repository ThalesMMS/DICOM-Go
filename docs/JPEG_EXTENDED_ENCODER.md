# JPEG Extended SOF1 encoder

`pixeldata/jpeg` has an explicitly registered, pure-Go JPEG Extended encoder
for `1.2.840.10008.1.2.4.51`. It writes sequential Huffman DCT SOF1 streams at
8 or 12-bit precision, derived from [ITU-T T.81](https://www.w3.org/Graphics/JPEG/itu-t81.pdf).
It reuses the project's scalar DCT and Huffman routines. No IJG, DCMTK or
GDCM implementation source is copied into it; installed DCMTK/GDCM are
independent test oracles only.

[PS3.5 Table 8.2.1-1](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_8.2.html)
(2026d) lists mono8/12 for `.51`, and lists color for `.50`.
The options constructor therefore defaults to mono8/12. Color `.51` output
is an explicitly selected compatibility profile, not a claim of DICOM color
conformance. [PS3.5 A.4.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.4.html)
requires SOF1 for `.51`, including its 8-bit encoding; this encoder never
substitutes SOF0.

## Profiles and registration

All inputs are unsigned with HighBit=BitsStored-1. Mono inputs have absent
PlanarConfiguration; color inputs require present PlanarConfiguration=0.
All frames have the same metadata and compatibility profile.

| Input | Allocated/stored bits | Selection | Reported compatibility profile |
|---|---|---|---|
| MONOCHROME1/2 | 8/8 or 16/12 | strict options default | empty |
| RGB | 8/8 or 16/12 | `AllowColorCompatibility` | `jpeg-extended-color` |
| YBR_FULL | 16/12 | `AllowColorCompatibility` | `jpeg-extended-color` |
| MONOCHROME1/2 | 16/9-11 | `AllowLegacyReducedPrecision`, or historical constructor | `jpeg-extended-reduced-precision` |

An empty profile means no listed compatibility profile was used; it is not a
general DICOM conformance certificate. `EncoderCapabilities.CompatibilityProfiles`
declares possible output profiles. `EncodedFrame.CompatibilityProfile` identifies
the profile actually used. Registration copies and validates those declarations;
encoding refuses undeclared output profiles. Both transcoder paths refuse
inconsistent profiles between frames and return owned
`TranscodeReport.EncodeCompatibilityProfiles` slices. Icon reports expose their
own profile slice.

```go
encoders := pixeldata.NewMemoryEncoderRegistry()
err := jpeg.RegisterExtendedEncoderWithOptions(encoders, jpeg.ExtendedEncoderOptions{
    Quality: 90, // strict mono8/12
})
```

Color needs `AllowColorCompatibility: true` in that options value.
`jpeg.NewExtendedEncoderWithOptions` provides the same strict direct API.
`jpeg.NewExtendedEncoder(quality)` and `jpeg.RegisterExtendedEncoder` retain the
historical mono9-12 API and additionally accept mono8. Mono9-11 is labeled as
compatibility: its stream precision is 12 and stored values retain their scale;
low quality can reconstruct samples above the reduced BitsStored range, which
the Go decoder clamps. The strict constructor refuses this reduced profile.
Neither constructor registers globally or in `builtin.NewRegistry`.

Registration does not authorize loss. Transcoding requires
`TranscodeOptions.AllowLossy=true`, records Lossy Image Compression `01`,
method `ISO_10918_1` and native-byte/encoded-byte ratio, and reports `Lossy=true`.

## Components, sampling and coding

Color components are supplied and preserved directly; the encoder does not
convert RGB to YBR. It emits three 1x1 components (4:4:4), one interleaved scan,
and independent DC predictors. RGB component IDs are R/G/B; YBR IDs are 1/2/3.
PhotometricInterpretation remains RGB or YBR_FULL and PlanarConfiguration stays
0. No JFIF or Adobe marker introduces an implicit color transform.

The scalar float64 FDCT follows T.81 A.3.3. Edge blocks replicate the final
row/column. Every component uses the luminance example quantization table
(Table K.1), scaled by quality 1-100: 5000/quality below 50, otherwise
200-2*quality. This applies directly to all RGB/YBR components; there is no
separate chrominance table. Precision8 uses 8-bit DQT values; precision12 can
use 16-bit DQT values. Quality100 quantizes by 1. Optimized shared DC/AC
Huffman tables are computed per frame, with at most 16-bit codes and a reserved
all-ones code. Level shifts are 128 or 2048. Input bits above HighBit are ignored.

Signed data, reduced color, YBR_FULL8, YBR_FULL_422, planar1, restart intervals,
custom tables, subsampling, progressive and hierarchical encoding are refused
or unavailable. Invalid metadata returns `ErrUnsupportedEncoderMetadata` before
allocation; wrong frame lengths return `ErrPixelDataSizeMismatch`.

## Bounds, cancellation and verification

Zero byte limits choose finite defaults: 256 MiB `MaxWorkingBytes` and 128 MiB
`MaxEncodedFrameBytes`. Negative limits return `ErrInvalidExtendedEncoderLimits`.
For B=ceil(columns/8)*ceil(rows/8)*components, admission reserves:

- output capacity = B*512+4096 bytes;
- working capacity = input frame bytes+B*128+output capacity+65536 bytes.

The output bound covers headers and worst-case 12-bit entropy with byte stuffing;
working capacity covers borrowed input, int16 coefficients, output capacity and
a fixed table allowance. This is a conservative allocation bound, not process
RSS or a compression-size prediction. A compressible large frame can still be
refused. Limits are checked with uint64 arithmetic and host-int bounds before
allocation. Exceeding a limit returns `ErrExtendedEncoderResourceLimit` without
partial output. Output storage is preallocated to its admitted bound.

Cancellation is checked before/after bounded coefficient and output allocations,
during DCT at each row and every 128 MCUs, and during entropy every 1024 blocks.
Allocation itself is bounded but cannot be interrupted inside Go's allocator.
A canceled call returns no frame; borrowed pixels are never modified.

For YBR `TranscodeOptions.MaxPixelError`, choose stored-component comparison
explicitly with `pixeldata.WithJPEGColorConversion(ctx, pixeldata.JPEGColorNever)`.
`JPEGColorDefaultGuess` converts decoded YBR to RGB, so verification refuses it
with `ErrTranscodeComparisonUnsupported` rather than comparing different color
spaces. Both memory and streaming paths test this refusal; an existing output
file is preserved. This error bound is on stored Y/Cb/Cr samples, not a perceptual
RGB metric. Lossy quality is not a guarantee for arbitrary images.

## Independent qualification and reproduction


`dcmdjpeg +cn +px -bs +te` validates Part10 native output metadata and stored
samples for all 30 cases. `gdcmconv -w --planar-configuration 0` validates 18
mono8/mono12/RGB8 cases. Installed `gdcmconv` refuses RGB12/YBR_FULL12 with
`Unsupported JPEG data precision 12`, including when the planar flag is omitted.
An own small adapter using public `gdcm::JPEGCodec` APIs decodes the remaining
12 cases. It checks JPEG dimensions/precision/component count and raw stored
samples; PI/planar are explicit input hints, so Part10 PI/planar verification for
these cases comes from DCMTK. The API emits an initial unsupported-precision
message while finding its successful 12-bit decoder; success requires exit0,
exact raw length, valid header and sample comparisons.

The largest difference from the Go decoder was 1 level for either oracle.
Quality100 stayed within 1 source level for every component. PSNR uses
peak=2^BitsStored-1 and is measured against the source per component. Enforced
floors are 24/27/30/36/42/56 dB for 8-bit profiles and 38/44/49/56/60/78 dB for
12-bit profiles, in quality order above. These are fixture qualification floors.

From the library root, build the adapter against installed public GDCM headers
and libraries (this compiles only the own wrapper, not a vendor runtime):

```powershell
$bin = 'C:\msys64\ucrt64\bin'
$evidence = Join-Path $env:TEMP 'issue1457-jpeg-extended-qualification'
New-Item -ItemType Directory -Force -Path $evidence | Out-Null
& "$bin\g++.exe" -std=c++17 -O1 '-IC:\msys64\ucrt64\include\gdcm-3.2' `
  '-LC:\msys64\ucrt64\lib' pixeldata/jpeg/testdata/gdcm_jpeg_oracle.cpp `
  -lgdcmMSFF -lgdcmDSED -lgdcmCommon -o "$evidence\gdcm-jpeg-oracle.exe"
$env:PATH = "$bin;" + $env:PATH
$env:DICOMGO_JPEG_EXTENDED_INTEROP = '1'
$env:DICOMGO_GDCM_JPEG_ORACLE = "$evidence\gdcm-jpeg-oracle.exe"
$env:DICOMGO_JPEG_EXTENDED_EVIDENCE_DIR = $evidence
$env:GOMAXPROCS = '2'
go test -p 1 ./pixeldata/jpeg -run '^TestExtendedEncoderNewProfilesDCMTKAndGDCM$' -count=1 -v
```

The test skips by default; opting in requires all tools and produces a receipt
only marked successful after every case passes. Local default tests cover
profiles, strict refusal, extreme samples/sizes, budget boundaries, cancellation,
concurrent historical encoder use, ownership and multiframe memory/streaming
transcoding. The own adapter was qualified here on little-endian Windows/amd64;
other native oracle platforms were not exercised.

## Integration checks


## Historical mono qualification


| Bits Stored | Quality | Recorded ratio | Codestream bytes | PSNR `dcmdjpeg` (dB) | PSNR pure Go (dB) | max \|Go − `dcmdjpeg`\| | `dcmdjpeg` samples > 2^BS − 1 | `dcmcjpeg +ee` bytes / PSNR (dB) |
|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 12 | 25 | 10.29 | 5831 | 46.9 | 46.9 | 1 | 0 | 5860 / 46.9 |
| 12 | 50 | 7.36 | 8145 | 50.6 | 50.6 | 1 | 0 | 8176 / 50.6 |
| 12 | 75 | 5.11 | 11737 | 53.7 | 53.7 | 1 | 0 | 11788 / 53.7 |
| 12 | 90 | 3.39 | 17683 | 60.5 | 60.5 | 1 | 0 | 17726 / 60.4 |
| 12 | 95 | 2.75 | 21809 | 66.4 | 66.4 | 1 | 0 | 21850 / 66.4 |
| 12 | 100 | 2.06 | 29081 | 82.8 | 83.1 | 1 | 0 | 29148 / 82.2 |
| 10 | 25 | 20.38 | 2943 | 38.0 | 38.0 | 1 | 5 | — |
| 10 | 50 | 14.67 | 4088 | 42.3 | 42.3 | 1 | 0 | — |
| 10 | 75 | 10.37 | 5784 | 46.8 | 46.8 | 1 | 0 | — |
| 10 | 90 | 6.49 | 9239 | 51.6 | 51.6 | 1 | 0 | — |
| 10 | 95 | 4.49 | 13346 | 54.9 | 54.9 | 1 | 0 | — |
| 10 | 100 | 2.81 | 21379 | 70.7 | 71.0 | 1 | 0 | — |

The quantization step does not depend on Bits Stored. The same quality
therefore gives about 6 dB less PSNR for each stored bit below 12, and a
higher compression ratio. At 12 bits, size and PSNR match DCMTK's own
encoder at every quality.
