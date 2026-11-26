# JPEG 2000 Part 2 component collections

The optional `pixeldata/jpeg2000.NewPart2Encoder` supports a bounded
Annex J component transform across monochrome DICOM frames. The base module
remains pure Go and registration is explicit.

| Output | Components | Stored precision | Transform |
|---|---|---|---|
| `.92` | 2–16 frames | 1–16 bits in 8/16 allocated bits, signed or unsigned | Reversible adjacent-frame differences, 5/3 spatial wavelet |
| `.93` | 2–16 frames | Eight bits in an 8-bit container, signed or unsigned | Irreversible decorrelation matrix, 9/7 spatial wavelet |

One collection contains at most 2,097,152 samples. Every frame has the same
Rows, Columns, precision and representation. Only `MONOCHROME1` and
`MONOCHROME2` are accepted. Planar Configuration must be absent.

```go
encoders := pixeldata.NewMemoryEncoderRegistry()
err := jpeg2000.RegisterPart2Encoder(encoders, jpeg2000.Options{
    TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID,
})
```

For lossy `.93`, select `AllowLossy: true` and a finite positive
`StepSize` when constructing the encoder. Transcoding separately requires
`TranscodeOptions.AllowLossy`. `MaxPixelError` verifies the final stored
samples across every frame, using an explicitly registered decoder.
The output records the lossy generation and creates a new SOP Instance UID.

Use `TranscodeDataSet`, `TranscodeFile` or `TranscodePath`. The registered
encoder supports `FrameCollectionEncoder`. Its single-frame method returns
a typed metadata error. The whole-object route is bounded by the transcode
limits and the collection cap. Every output contains one fragment with
an empty Basic Offset Table. Explicit refragmentation, nondefault offset
tables and compressed icons are rejected.

The nested JPEG 2000 adapter's `RegisterCorePart2` enables decoding of this
exact Annex J profile. The reader validates CBD, MCT, MCC and MCO before the
spatial decoder runs. A `FrameReader` can return frames and regions from the
collection, including deferred Part 10 files. It decodes the collection for
each request without caching decoded data. `RawFrame` and `RawFrameLength`
refuse the request because a frame has no independent encoded codestream.
Multiple fragments, more than one collection, other matrices, wavelet
component transforms and partial-resolution decoding remain outside this
profile.

`codecprofile.JPEG2000Part2EncoderCapability()` reports the encoder's
direction-specific coverage and bounded independent-decoder evidence.
Registration is separate from the current codecfull release profile.

## Bitstream

SIZ declares Rsiz 0x8100. COD declares SGcod 2. Coded components are signed
and have one more precision bit than the original frames. A single CBD
declares the reconstructed precision and sign. MCT array 1 holds either
reversible dependency coefficients or the irreversible lower-triangular
inverse matrix. MCT array 2 supplies the reconstructed DC offsets. MCC
contains one ordered collection, and MCO selects its single stage. The
stream uses only the Annex J extension and the existing Part 1 block coder.

## Validation on Windows

The default tests cover native round trips, unchanged frame order, stored
sample error, metadata, file serialization, cancellation, resource limits
and atomic destination preservation. The opt-in independent decoder reads
the original Part 10 with pydicom 3.0.2, parses Annex J in Python, decodes
spatial coefficients with OpenJPEG 2.5.4, and applies the marker-defined
inverse transform. Its reconstruction uses no production Go decoder,
marker parser, or inverse operator. The source pixels are read independently
with pydicom and NumPy 2.1.3.

For `.93`, the spatial coefficients must retain their fractional values
until after the component transform. Stock `opj_decompress` rounds those
values before writing PGX. The test-only build script adds a coefficient
export immediately before OpenJPEG's final level shift, rounding and
clamping. The hook serializes float32 values and leaves spatial decoding
and sample values unchanged. The Python stage applies the matrix and
performs the final rounding and clamping.

Build the extractor from the upstream OpenJPEG 2.5.4 source. The archive is
available from the [OpenJPEG release](https://github.com/uclouvain/openjpeg/releases/tag/v2.5.4).
The GitHub tag archive SHA-256 is
`a695fbe19c0165f295a8531b1e4e855cd94d0875d2f88ec4b61080677e27188a`.
The script checks the unmodified `tcd.c` SHA-256 before copying and patching
the source. It requires CMake, Ninja and a C compiler. The build directory
must be new.

```powershell
python -m pip install pydicom==3.0.2 numpy==2.1.3
python pixeldata/jpeg2000/testdata/build_part2_openjpeg.py `
  --source C:/reference/openjpeg-2.5.4 --build C:/reference/openjpeg-float
$env:DICOMGO_J2K_PART2_ORACLE = "1"
$env:DICOM_GO_OPENJPEG_FLOAT = "C:/reference/openjpeg-float/build/bin/opj_decompress.exe"
go test -p 2 ./pixeldata/jpeg2000 -run '^TestPart2' -count=1 -v
go -C examples/codec-adapters/jpeg2000 test -p 2 . -run '^TestPart2Independent' -count=1 -v
```

`DICOM_GO_OPENJPEG_DECOMPRESS`, `DICOM_GO_OPENJPEG_COMPRESS` and
`DICOM_GO_PYTHON` override the stock tool paths. Set
`DICOM_GO_PART2_EVIDENCE_DIR` to preserve the synthetic original files,
outputs and JSON receipts from the Part 10 tests. Each receipt records
SHA-256 values for the source, output, codestream, oracle and spatial
decoder, tool versions, sample count, maximum error, mean absolute error
and PSNR. The explicitly selected tests require these tools.

The Part 10 corpus covers five small reversible cases plus two
257×129×5-frame signed/unsigned 16-bit fields containing full-range
extrema and deterministic noise. All 334,698 reversible samples match
exactly. The four irreversible 8-bit cases contain 2,673 samples. With
`StepSize: 2`, their results are listed below. The corpus-specific bounds
do not establish universal bounds for that quantization step.

| Frames | Representation | Maximum error | Mean absolute error | PSNR (dB) | Asserted bound |
|---|---|---|---|---|---|
| 2 | unsigned | 2 | 0.449 | 50.97 | 6 |
| 4 | unsigned | 4 | 0.654 | 48.44 | 6 |
| 5 | signed | 4 | 0.776 | 47.25 | 6 |
| 16 | signed | 7 | 1.368 | 42.60 | 8 |

Independent controls start with `opj_compress` output and use alternate
array indices, dependency coefficients, divisors, offsets, component
permutations and a nonidentity irreversible matrix. The float control
detects premature integer conversion. Malformed marker and DICOM metadata
controls are rejected.

The installed OpenJPEG 2.5.4 command refuses the complete SGcod=2
codestream. The independent composed decoder covers the declared profile.
Interoperability with an off-the-shelf full Annex J decoder remains
unqualified, and the capability keeps its provisional status.

The normative encapsulation and transform restrictions are in
[DICOM PS3.5 A.4.4](https://dicom.nema.org/medical/DICOM/current/output/chtml/part05/sect_A.4.4.html).
The array and marker definitions are in
[ITU-T T.801](https://www.itu.int/rec/T-REC-T.801).
