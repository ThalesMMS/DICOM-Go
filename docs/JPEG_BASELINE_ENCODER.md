# JPEG Baseline encoder options and qualification

`pixeldata/jpeg.Encoder` encodes unsigned native 8-bit `MONOCHROME1`,
`MONOCHROME2`, and interleaved `RGB` frames as JPEG Baseline Process 1
(`1.2.840.10008.1.2.4.50`). Quality is explicit, from 1 through 100.
Lossy encoding remains opt-in at the transcoder level.

## API and preserved default

`NewEncoder(quality)` and `RegisterEncoder(registry, quality)` preserve the
previous `image/jpeg` path: monochrome or color 4:2:0, standard Huffman tables.
The resulting codestream is tested byte-for-byte against that previous path.
It does not allocate the new encoder's frame of quantized coefficients.

Use `NewEncoderWithOptions` or `RegisterEncoderWithOptions` for the new path:

```go
encoder, err := jpeg.NewEncoderWithOptions(jpeg.EncoderOptions{
    Quality:         90,
    Subsampling:     jpeg.Subsampling420,
    OptimizeHuffman: true,
})
```

`Subsampling420` is the zero value. `Subsampling422` and `Subsampling444`
select the local optimized encoder regardless of `OptimizeHuffman`; the
standard-library encoder cannot produce those factors. Monochrome always has
one 1x1 component, but selecting an optimized configuration also uses the
local encoder for monochrome. `OptimizesHuffman()` reports the effective
selection. `Capabilities().Backend` is `go-image-jpeg` for the legacy path and
`pure-go` for the new path.

The local path uses T.81 Table K.1/K.2 quantization with the same quality scale,
a separable FDCT, and per-frame Huffman tables using the existing local
Extended encoder's entropy primitives. Edge pixels are replicated before
chroma averaging, including odd dimensions. FDCT rounding can differ from
`image/jpeg`; optimizing does not promise identical reconstructed samples.
The encoder is immutable and supports concurrent frames, with private scratch
and cancellation checks during sampling/FDCT and entropy output.

## Sampling and DICOM color attributes

The claim that JPEG 4:2:0 labeled `YBR_FULL_422` is necessarily a DICOM bug is
incorrect. [PS3.3 C.7.6.3.1.2, YBR_FULL_422 note 3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.3.html)
explicitly discusses that use for encapsulated JPEG. The actual sampling is
carried by the codestream, not inferred from the label.

[PS3.5 8.2.1, Table 8.2.1-1](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_8.2.html)
permits `YBR_FULL_422` and `RGB` for color Baseline; it does not permit
`YBR_FULL` for that lossy transfer syntax. Therefore all three YCbCr sampling
modes return `YBR_FULL_422`, Planar Configuration 0. Monochrome keeps its
input Photometric Interpretation and omits Planar Configuration.

| Configuration | SOF0 precision | Y sampling | Cb/Cr sampling |
|---|---|---|---|
| Monochrome | 8 | 1x1 | absent |
| Color 4:2:0 | 8 | 2x2 | 1x1 |
| Color 4:2:2 | 8 | 2x1 | 1x1 |
| Color 4:4:4 | 8 | 1x1 | 1x1 |

General transfer-syntax support does not override IOD-specific color rules.
This API accepts canonical RGB input; it does not pass through native
subsampled YBR bytes or introduce an RGB-without-color-transform mode.

## Measured trade-offs


| Path | Median ms/frame | Allocated B/op | Allocs/op | Coefficient B/frame | JPEG B/frame |
|---|---:|---:|---:|---:|---:|
| Default monochrome | 0.811 | 295376 | 11 | 0 | 10784 |
| Direct old `image/jpeg`, monochrome | 0.841 | 295376 | 11 | 0 | 10784 |
| Optimized monochrome | 1.457 | 598313 | 9 | 524288 | 8096 |
| Default RGB 4:2:0 | 1.957 | 1114677 | 15 | 0 | 16987 |
| Direct old `image/jpeg`, RGB 4:2:0 | 1.913 | 1114677 | 15 | 0 | 16987 |
| Optimized RGB 4:2:0 | 3.023 | 992400 | 22 | 786432 | 14578 |
| Optimized RGB 4:2:2 | 3.787 | 1254609 | 23 | 1048576 | 16960 |
| Optimized RGB 4:4:4 | 5.416 | 1778900 | 23 | 1572864 | 19065 |

Choose optimization for its measured size/quality trade-off, not as a general
speed improvement. The default implementation is unchanged; timing variation
between its two benchmark wrappers is not evidence of a speed change.
4:2:2/4:4:4 retain more chroma than 4:2:0 and can produce larger files at the
same quality. An equal-or-smaller requirement across those sampling modes is
not an equivalent-fidelity comparison.

The size/quality test also covers 40 cases: 127x113, monochrome/RGB, constant,
smooth, checker and seeded noise, qualities 1/25/75/95/100. With the same 4:2:0
sampling and quality, optimized codestreams were 5.79% to 64.26% smaller than
the previous encoder. Source MAE is required to stay within `old MAE * 1.03 +
0.1` levels. This is a bounded synthetic corpus result, not a universal size
guarantee or clinical image-quality qualification.

## Local validation and independent decoders

Run from `dicom-go/`:

```sh
go test ./pixeldata/jpeg -run 'TestBaseline|TestJPEGEncoder' -count=1
go test -race ./pixeldata/jpeg -count=1
go test ./pixeldata/jpeg -run '^$' -bench '^BenchmarkBaselineEncoder$' -benchmem -benchtime=1s -count=3
DICOMGO_DCMTK_INTEGRATION=1 DICOMGO_DCMTK_BIN=/path/to/dcmtk/bin go test ./pixeldata/jpeg -run '^TestBaselineEncoderAgainstDCMTK$' -count=1 -v
DICOMGO_GDCM_INTEGRATION=1 DICOMGO_GDCM_BIN=/path/to/gdcm/bin go test ./pixeldata/jpeg -run '^TestBaselineEncoderAgainstGDCM$' -count=1 -v
```

DCMTK may also need `DCMDICTPATH` pointing to its `dicom.dic`. Both oracle
tests are skipped by default; missing optional tools are reported explicitly.
These commands do not use a PACS, network, external dataset or CI.

The initial qualification passed 24 independent decoder cases: monochrome
and RGB 4:2:0/4:2:2/4:4:4, qualities 25/90/100, 61x45 synthetic frames with odd
edges, using DCMTK `dcmdjpeg` 3.7.0+DEV and GDCM `gdcmconv` 3.3.0. Tests inspect
SOF0, DICOM attributes, native dimensions and decoded pixels. These tools
preserve native `YBR_FULL` after decompression; the comparison explicitly
normalizes those full-range samples to RGB. Luminance must agree at every
pixel within two levels. Monochrome maximum difference is bounded by two,
4:4:4 RGB by three; subsampled RGB MAE is bounded by four, because independent
chroma interpolation differs from Go's nearest-neighbor expansion. Observed
subsampled maximum differences reached 78 near sharp chroma transitions.

The oracle binaries' SHA-256 values were:

- `dcmdjpeg`: `e81745e0fdd72b0cd01dd5e01d5a201b3e89b797face49f372b622f8222bf88f`
- `gdcmconv`: `e29e1872a632ca5dc055f8a913c1dafe140d07f9997bbd8cee0a2cdfe56d9252`

Their CMake source roots pointed to the locally inspected DCMTK/GDCM trees
(`8d68392db66cd69e13dc878dc9889681062ad478fc` and
`2cd05d13222b175e791b5ae3c08feeabcc1ed1f0`). A source-root path/current checkout
alone does not prove a binary was built from that exact revision; binary
version and digest identify the executed oracles. No reference source was
copied into this implementation.
