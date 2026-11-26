# Pixel Transcoding Benchmark Evidence


This report records the first pure-Go RLE encode profiles added with the
explicit encoder registry. The fixtures are deterministic synthetic pixels and
contain no patient data.

## Environment


The `peak-heap-delta-bytes` metric samples `runtime.MemStats.HeapAlloc` every
100 microseconds while the benchmark runs and reports the largest increase
over a forced-GC baseline. It is a bounded in-process heap peak, not resident
set size.

| Profile | Time/op | Throughput | Allocated/op | Allocs/op | Peak heap delta |
| --- | ---: | ---: | ---: | ---: | ---: |
| RLE mono 512x512 8-bit | 455,521 ns | 575.48 MB/s | 270,851 B | 2 | 7,585,384 B |
| RLE RGB 512x512 16-bit | 2,669,000 ns | 589.31 MB/s | 1,589,772 B | 2 | 14,308,632 B |

These values are qualification evidence for this environment, not a portable
performance guarantee. Re-run the command for release platforms and record a
new report rather than overwriting this one.

## JPEG Lossless SV1 encoder


The comparison encodes the same synthetic multi-frame objects through
`pixeldata.TranscodeDataSet` and through `dcmcjpeg +e1`. The DCMTK time
includes process start and file I/O, so it favors neither side strongly.

| Profile | dicom-go | dcmcjpeg +e1 | dicom-go bytes | dcmcjpeg bytes |
| --- | ---: | ---: | ---: | ---: |
| MR 512x512 12-bit, 40 frames | 158 MiB/s | 199 MiB/s | 6,733,216 | 6,733,978 |
| RGB 640x480 8-bit, 20 frames | 97 MiB/s | 107 MiB/s | 3,889,738 | 3,889,060 |

`go test -bench`: 179.6 MB/s for one 16-bit MR frame and 101.9 MB/s for one
8-bit RGB frame. Compressed sizes match DCMTK within 0.1%, since both build
optimal Huffman tables per frame.
