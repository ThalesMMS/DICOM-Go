# JPEG Baseline decode paths

The registered JPEG Baseline codec (`1.2.840.10008.1.2.4.50`) and the Baseline
SOF0 branch of `jpeg.New()` use a lightweight SOF admission walk followed by one
standard `image/jpeg.Decode` call for common matching headers. The admission
walk also checks width/height against DICOM metadata before the standard decoder
can allocate an image. It replaces the former separate `DecodeConfig` call on
that path; it does not port the standard decoder or eliminate the decoder's own
SOF interpretation. Quantization, Huffman tables, color interpretation and
entropy data remain the standard decoder's responsibility.

Incomplete or unusual SOF0 headers and dimension mismatches retain the old
`DecodeConfig` gate. On a decode failure, a config-only retry preserves the
historical error classification without retrying pixel decoding. Oversized
codestream dimensions are refused before image allocation even when DICOM
metadata declares a small image. Encapsulated framing validation and the
existing request working-set admission remain in place.

For decoded `*image.Gray` with monochrome metadata, visible rows are copied to
a new owned buffer. The copy respects nonzero rectangle origins and stride,
omits padding, and permits cancellation between rows. Stored-bit limiting never
mutates the decoder image. Other image types retain the previous `At`/color-model
conversion: a color JPEG under monochrome metadata is not approximated by its
Y plane. Color policies and the existing 4:4:4/4:2:2/4:2:0 chroma reconstruction
are unchanged. No decoder changes were made for Extended, progressive or
lossless JPEG.

The standard decoder checks cancellation when refilling its input reader; this
does not promise preemption inside an individual IDCT/entropy loop. Baseline
post-decode/conversion checks prevent canceled output from being returned, and
a canceled multi-frame request returns no partial `Frames` result.

## Focused validation and measurement

[Tests](../pixeldata/jpeg/baseline_decode_test.go) compare visible grayscale
bytes, signed/unsigned stored-bit limiting, output ownership, color policies and
subsampling against the unchanged conversion helpers and the former standard
decode gate. Header fixtures cover false SOF bytes inside APP payloads, marker
fill bytes, truncated/malformed segments/tables/SOF, maximum 16-bit dimensions,
error-string parity and cancellation. Full package tests also cover the other
registered JPEG processes.

[The benchmark](../pixeldata/jpeg/baseline_decode_bench_test.go) uses synthetic
64²/512²/1024² grayscale and 512² color frames, encoded by the same standard
encoder at quality 90. Fixture encoding and output hashes are outside its timed
loop. The identical harness can run against the pinned pre-change implementation
and candidate to attribute the decode difference. `ns/op`, `B/op` and
`allocs/op` describe that complete adapter decode workload, not isolated IDCT
cost, process RSS, clinical image quality or target-PACS performance.

```sh
go test ./pixeldata/jpeg
go test -race ./pixeldata/jpeg
go test ./pixeldata/jpeg -run '^$' -bench '^BenchmarkBaselineDecode1309$' -benchmem
```

There is no universal speedup claim: a grayscale row-copy benefit depends on
frame size/content, and color conversion has not been optimized in this change.

## Local before/after result


| Synthetic frame | Baseline median | Candidate median | Baseline/candidate | B/op baseline → candidate | Allocs/op baseline → candidate |
| --- | ---: | ---: | ---: | ---: | ---: |
| Gray 64×64 | 31.414 µs | 18.289 µs | 1.72× | 35,824 → 22,240 | 16 → 15 |
| Gray 512×512 | 1.425 ms | 0.683 ms | 2.08× | 551,952 → 538,368 | 18 → 17 |
| Gray 1024×1024 | 5.791 ms | 2.811 ms | 2.06× | 2,124,784 → 2,111,200 | 16 → 15 |
| Color 512×512 | 1.980 ms | 1.960 ms | 1.01× | 1,208,448 → 1,194,864 | 19 → 18 |

The color timing ranges overlapped (baseline 1.973–2.020 ms, candidate
1.934–1.997 ms), so this small difference is not evidence of a color speedup.
The measurement is a local synthetic decode result, with no statistical,
process-RSS, clinical or cross-platform performance claim. Full package tests,
race tests, Go 1.22.12 compatibility tests and `go vet` passed; corpus-dependent
opt-in tests were skipped rather than presented as external-corpus evidence.
