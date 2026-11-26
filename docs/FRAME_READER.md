# FrameReader: single-frame decoding

`pixeldata.FrameReader` indexes Pixel Data once and decodes selected frames.
The sample-only `Frame` path reuses exclusive metadata facades instead of
building and decompressing a full `object.Object` for every frame. It caches
one-frame metadata and normalized limits; it still resolves the codec registry
on each call so late registration and replacement keep working.

For already materialized files, `DecodeFileFrame` returns an owned selected
frame and uses the index when the encapsulated layout supports it.
`DecodeFileFramesView` decodes the full object, borrowing read-only native
sample bytes where possible. Both report the sample metadata and preserve
the source frame count. Native bytes keep the stored sample byte order.

The existing decoder dispatch preserves `ContextCodec`, context recovery/color
options, typed codec errors and diagnostics. The same output helpers validate
geometry, count and byte length, normalize color metadata and convert native
Big Endian samples to Little Endian. Decode limits apply before reading the
payload from a deferred source or copying materialized bytes. A native region
charges the requested pixels and output bytes plus the source bytes actually
needed by its rows. Frame and fragment index limits apply regardless of source
backing. Transcode callers that need a native output dataset still receive one,
with a separate returned sample buffer.

The native reader handles frames and region rows that share a storage word or
byte. For Big Endian OW Pixel Data with eight or fewer allocated bits it reads
aligned words and resolves their byte lanes across frame boundaries. For
one-bit native Pixel Data the frames occupy one continuous LSB-first bit
stream. `Frame`, `Region` and the corresponding Into methods return a fresh
bit-zero-aligned frame or rectangle. The unused tail bits are zero.
`RawFrame` returns an independently packed result in the stored order, with an
even-byte value for the Big Endian OW form. Callers streaming the stored
representation can use `RawFrameLength` to check their storage or transport
budget before reading. An OW value field with a partial word is refused.
The packed extraction preserves the existing integer grayscale sample
formats for rendering and averaging.
[PS3.5 §8.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/chapter_8.html)
· [§8.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_8.2.html)
· [retired Big Endian syntax, §A.3](https://dicom.nema.org/medical/dicom/2016b/output/chtml/part05/sect_A.3.html).

## Ownership and concurrency

Keep the file dataset, its value storage and any deferred source unchanged
while the reader is in use. Do not copy a `FrameReader` after first use: its
state contains `sync.Once` and `sync.Pool`; pass the returned pointer instead.
Close an owned source only after active reads finish.

Each active decode borrows a separate facade because Object read methods such
as `GetSequence` can populate caches. The Codec contract is unchanged: the
Pixel Data and complete Object are borrowed, read-only inputs that a codec
must not retain after returning; codec output owns its samples. The pool
clears Pixel Data before returning a facade. `Frame` still copies the validated
decoded frame into caller-owned output through `nativeFrameBytesContext`.
The pool may discard facades during garbage collection and rebuild them later.


Median of five 300 ms samples per cell; one operation decodes one frame:

| Syntax | Path | ns/frame | B/frame | allocations/frame |
| --- | --- | ---: | ---: | ---: |
| RLE | Old object pipeline | 74,673 | 263,880 | 83 |
| RLE | Direct Frame | 6,277 | 21,416 | 10 |
| JPEG-LS | Old object pipeline | 138,965 | 270,213 | 91 |
| JPEG-LS | Direct Frame | 68,485 | 30,849 | 18 |
| Native Big Endian | Old object pipeline | 76,903 | 265,375 | 98 |
| Native Big Endian | Direct Frame | 6,828 | 24,696 | 9 |

These cases improved elapsed time by 11.90×, 2.03× and 11.26× respectively,
with 88.58–91.88% fewer allocated bytes. They measure small frames with many
attributes and a warmed pool, not a universal codec speedup, cold indexing,
file I/O, viewer latency or PACS performance.

Each of five rounds ran RLE baseline/direct, JPEG-LS baseline/direct, then
Big Endian baseline/direct. Baseline was always first in each pair; ordering
was not randomized. The full campaign, including fixture creation,
calibration and measurement, lasted 12.663 seconds (03:40:17.206836 to
03:40:29.869610 UTC). This wall time is separate from per-operation results.

`TestFrameReaderDirectMultiframeByteExact` compared every frame and every
returned Metadata field for 128-frame RLE mono, RLE RGB, JPEG-LS mono and
Big Endian mono series against the old pipeline, outside benchmark timing.
Focused reader and transcode tests passed with the race detector (4.387 s)
and Go 1.22.12 (1.297 s; external linking for this macOS host). `go vet
./pixeldata` passed. Additional tests cover concurrent facade reads, late
codec replacement, context options/cancellation, preflight limits without
payload reads, malformed outputs, caller ownership, reports and output
photometric padding.

Reproduce from the library root; compile outside the measurement window:

```sh
go test -c -o /tmp/dicom-frame-reader.test ./pixeldata
for round in 1 2 3 4 5; do
  GOMAXPROCS=1 /tmp/dicom-frame-reader.test \
    -test.run='^$' -test.bench='^BenchmarkFrameReaderDirectMultiframe$' \
    -test.benchtime=300ms -test.count=1
done
```

The recorded campaign used base checkout
`07d0e8b83eb9d79cc5c2d59a1794c974d7b4b8c8` plus this change and concurrent
wave changes. Both benchmark paths were in the same binary. The measured
`pixeldata/frame_reader.go` SHA-256 was
`252585300e64dd78c9a128d29a726c5fc057c325db3ad44f6fbc39dd7c253df6`.

### Raw benchmark samples

Rows appear in execution order, six per round. Each round completed with
`PASS`.

```text
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/object_baseline         	    4753	     73641 ns/op	 111.24 MB/s	  263880 B/op	      83 allocs/op
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/direct                  	   58221	      6260 ns/op	1308.64 MB/s	   21416 B/op	      10 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/object_baseline         	    2580	    136985 ns/op	  59.80 MB/s	  270213 B/op	      91 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/direct                  	    5757	     67575 ns/op	 121.23 MB/s	   30849 B/op	      18 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/object_baseline   	    4659	     75746 ns/op	 108.15 MB/s	  265375 B/op	      98 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/direct            	   52825	      6764 ns/op	1211.07 MB/s	   24696 B/op	       9 allocs/op
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/object_baseline         	    5347	     74673 ns/op	 109.71 MB/s	  263882 B/op	      83 allocs/op
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/direct                  	   55796	      6267 ns/op	1307.26 MB/s	   21416 B/op	      10 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/object_baseline         	    2564	    138965 ns/op	  58.95 MB/s	  270213 B/op	      91 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/direct                  	    5418	     68485 ns/op	 119.62 MB/s	   30848 B/op	      18 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/object_baseline   	    4234	     76360 ns/op	 107.28 MB/s	  265376 B/op	      98 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/direct            	   53115	      6967 ns/op	1175.75 MB/s	   24696 B/op	       9 allocs/op
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/object_baseline         	    5241	     75361 ns/op	 108.70 MB/s	  263880 B/op	      83 allocs/op
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/direct                  	   56960	      6322 ns/op	1295.81 MB/s	   21416 B/op	      10 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/object_baseline         	    2571	    140231 ns/op	  58.42 MB/s	  270213 B/op	      91 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/direct                  	    5448	     68503 ns/op	 119.59 MB/s	   30849 B/op	      18 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/object_baseline   	    4500	     78330 ns/op	 104.58 MB/s	  265375 B/op	      98 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/direct            	   52062	      6828 ns/op	1199.78 MB/s	   24696 B/op	       9 allocs/op
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/object_baseline         	    5336	     74543 ns/op	 109.90 MB/s	  263881 B/op	      83 allocs/op
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/direct                  	   56732	      6277 ns/op	1305.11 MB/s	   21416 B/op	      10 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/object_baseline         	    2551	    138095 ns/op	  59.32 MB/s	  270213 B/op	      91 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/direct                  	    5560	     68203 ns/op	 120.11 MB/s	   30849 B/op	      18 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/object_baseline   	    4436	     81451 ns/op	 100.58 MB/s	  265375 B/op	      98 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/direct            	   50893	      7014 ns/op	1167.91 MB/s	   24696 B/op	       9 allocs/op
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/object_baseline         	    5221	     77409 ns/op	 105.83 MB/s	  263880 B/op	      83 allocs/op
BenchmarkFrameReaderDirectMultiframe/rle16_128frames/direct                  	   56221	      6532 ns/op	1254.20 MB/s	   21416 B/op	      10 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/object_baseline         	    2482	    142589 ns/op	  57.45 MB/s	  270213 B/op	      91 allocs/op
BenchmarkFrameReaderDirectMultiframe/jls16_128frames/direct                  	    5396	     75851 ns/op	 108.00 MB/s	   30849 B/op	      18 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/object_baseline   	    4339	     76903 ns/op	 106.52 MB/s	  265376 B/op	      98 allocs/op
BenchmarkFrameReaderDirectMultiframe/bigendian16_128frames/direct            	   53028	      6825 ns/op	1200.29 MB/s	   24696 B/op	       9 allocs/op
```
