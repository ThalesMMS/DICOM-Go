# Large-dataset benchmark workflow


The scalable parser and Part 10 benchmarks are informational performance
evidence. They report `ns/op`, `B/op`, `allocs/op`, processed bytes and
throughput across stable sub-benchmarks for many elements, deep sequences,
large native and encapsulated Pixel Data, streaming thresholds, deferred
locations, reads, writes and round trips.

Large payloads are generated in memory or in temporary files; no bulky corpus
is committed. Encapsulated cases measure Part 10 container, fragment and
deferred-I/O behavior with structural synthetic bytes. They do not invoke or
qualify an RLE/JPEG decoder; codec conformance remains in the codec-specific
gates and fixture suites.

Wall-clock measurements depend on CPU model, power state, operating-system
scheduling, compiler version and background load. `make check` therefore does
not apply a percentage threshold to these results. Correctness, resource-limit
and allocation-ownership tests remain deterministic gates in their package
tests; this workflow is a separate regression investigation tool.

## Comparable runs

Compare commits only when both contain the same benchmark suite. Use the same
machine, Go toolchain, working-tree state and Make variables for both runs. Run
the baseline immediately before the candidate, close unrelated workloads and
repeat in the opposite order when a small difference matters.

From each commit's `dicom-go` directory:

```sh
make benchmark-large BENCH_COUNT=6 BENCH_TIME=250ms BENCH_CPU=1 > /tmp/baseline.txt
make benchmark-large BENCH_COUNT=6 BENCH_TIME=250ms BENCH_CPU=1 > /tmp/candidate.txt
```

Each command executes exactly these top-level benchmark families:

- parser: `BenchmarkReaderNextScalable*` and
  `BenchmarkReadDataSetScalable*`;
- object: `BenchmarkScalablePart10Read`, `BenchmarkScalablePart10Write`,
  `BenchmarkScalablePart10RoundTrip`, and
  `BenchmarkScalablePart10DeferredRead`.

The sub-benchmark path is part of the baseline identity. Do not compare renamed,
added or removed sizes as though they were paired samples.

## DIMSE P-DATA send size

`BenchmarkLoopbackDataSetSendPDU` in `net/dimse` streams a 32 MiB data set
over a loopback association whose acceptor offers a 1 MiB Maximum Length,
with progress timeouts on both sides as `storescu`/`storescp` use them, at
the former 16 KiB send size, 64 KiB and the 128 KiB default:

```sh
go test ./net/dimse -run '^$' -bench BenchmarkLoopbackDataSetSendPDU -benchtime 30x -cpu 2
```

Informational figures from an i7-14700K running Windows 11 and Go 1.26:

| Send size | Throughput | B/op | allocs/op |
| --- | --- | --- | --- |
| 16 KiB | 1.0-1.25 GB/s | 37 MB | 65,600 |
| 64 KiB | 1.64-1.75 GB/s | 35 MB | 23,000 |
| 128 KiB | 1.79-1.94 GB/s | 35 MB | 15,700 |


## DIMSE socket buffers

`BenchmarkSocketBuffersReceiverWindow` in `net/ul` streams 8 MiB over an
established loopback association whose acceptor sets
`ListenOptions.SocketBuffers.Receive` and drains its socket once per simulated
round-trip time, so each round trip delivers at most what the kernel receive
buffer held: the receive-window bound (buffer / RTT) of a long path. It does
not emulate the sender's buffer, loss or congestion control, and "default"
leaves the operating system size with its automatic tuning:

```sh
go test ./net/ul -run '^$' -bench BenchmarkSocketBuffersReceiverWindow -benchtime 3x -count 2
```

Informational figures from an Apple M4 Pro running macOS and Go 1.27,
ranges over two runs; `rcvbuf` is the size the kernel reported after the
transfer:

| RTT | Receive buffer | Throughput | rcvbuf |
| --- | --- | ---: | ---: |
| 0 (loopback) | default or explicit | 2-11 GB/s, no consistent order | |
| 10 ms | default (auto-tuned) | 135-205 MB/s | 2.0-3.8 MB |
| 10 ms | 64 KiB | 52-55 MB/s | 0.3 MB |
| 10 ms | 256 KiB | 72-76 MB/s | 0.26 MB |
| 10 ms | 1 MiB | 140-151 MB/s | 1 MB |
| 10 ms | 4 MiB | 235-277 MB/s | 4 MB |
| 40 ms | default (auto-tuned) | 49-61 MB/s | 2.6-3.5 MB |
| 40 ms | 64 KiB | 14.5-14.6 MB/s | 0.3 MB |
| 40 ms | 256 KiB | 20 MB/s | 0.26 MB |
| 40 ms | 1 MiB | 38-41 MB/s | 1 MB |
| 40 ms | 4 MiB | 55-70 MB/s | 4 MB |

Throughput follows the receive buffer divided by the RTT. The automatic tuning
reaches about 4 MiB within the first megabytes, so an explicit 4 MiB gains
only while it ramps up (up to 1.8 times over the first 8 MiB at 10 ms) and an
explicit smaller buffer loses up to fourfold. The defaults therefore stay with
the operating system; an explicit size helps where the bandwidth-delay
product exceeds the operating system's tuning ceiling, or caps memory.

## Skipped Pixel Data I/O

`BenchmarkSkipPixelDataSource` in `parser` reads a data set with 64 MiB of
native Pixel Data under `SkipPixelData`, once from a source that hides `Seek`
and once from a sized seekable source, and reports the bytes the source
returned:

```sh
go test ./parser -run '^$' -bench BenchmarkSkipPixelDataSource -benchmem -count=3
```

Informational figures from an Apple M4 Pro running macOS and Go 1.27:

| Source | bytes read/op | Time/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Seek past the value | 48 | 1.9-2.5 µs | 2.6 KB | 43 |

A skipped or deferred value is seeked past only when that is
indistinguishable from reading it: the source is seekable with a known end
and the value ends before the source end, its enclosing container and
`MaxTotalBytes`. Otherwise it is read, so truncated values and exceeded limits
fail exactly as before (`TestSkipPixelDataSeekKeepsTruncationAndLimitErrors`).

## Seek-skipped unselected sequences

A `DataSetTags` projection already seeks past unselected primitive values on a
seekable source, but it still parses every header of an unselected sequence's
items. The opt-in `ReadFileOptions.SeekUnselectedSequences` (parser
`ReaderOptions.SeekUnselectedSequences`) passes an unselected root sequence
with a defined length by one seek instead. The same seek rule applies:
undefined-length sequences, non-seekable and deflated sources, and lengths that
overrun the source or `MaxTotalBytes` are parsed as without the option, so
those failures are unchanged (`TestDataSetProjectionSeekKeepsLengthFailures`).
The content of a sequence passed by seek is not parsed, so a malformed item,
a declared length shorter than the content, nesting depth and element limits
inside it are not observed. Use it where the caller does not rely on the
projection as structural validation of unselected sequences.

```sh
go test ./parser -run '^$' -bench BenchmarkDataSetProjectionUnselectedSequence -benchmem -count=6
```

Windows amd64, Intel i7-14700K, Go 1.26.4, a 2,048-item unselected sequence
from a sized in-memory source, medians of six runs:

| Projection | Time/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| Items parsed structurally | 900.6 µs | 51,456 | 22,570 |
| Sequence passed by seek | 1.6 µs | 2,019 | 35 |

## Pinned benchstat comparison

Install the repository-pinned `benchstat` into a disposable tool directory and
compare the two result files:

```sh
tools=$(mktemp -d)
make benchstat-install BENCHSTAT_INSTALL_DIR="$tools"
make benchmark-compare \
  BENCHSTAT="$tools/benchstat" \
  BASE_BENCH=/tmp/baseline.txt \
  CANDIDATE_BENCH=/tmp/candidate.txt
```

`benchmark-compare` first normalizes away only Go's trailing CPU suffix and
requires the complete sub-benchmark names and sample counts to match exactly.
It then runs `golang.org/x/perf/cmd/benchstat` at the pinned revision declared by
`BENCHSTAT_VERSION` in the Makefile. A statistically significant difference is
evidence to investigate, not an automatic merge failure; inspect throughput,
allocations and the affected size/family together.

