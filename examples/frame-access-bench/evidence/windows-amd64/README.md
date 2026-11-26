# Windows amd64 results

The comparison ran on Windows 11 Pro 10.0.26200 amd64, Intel Core i7-14700K,
Go 1.26.4 with GOMAXPROCS=1, GCC 16.2.0 and MSYS2 UCRT DCMTK 3.7.0 with
modified CharLS 1.0. The Go baseline is 7a2ea876. The candidate includes
whole-frame JPEG-LS direct output and preservation of cancellation error
classification through 19a86585. Both Go binaries used the same benchmark
source. The DCMTK DLL SHA-256 values were
1b875bbf5f5cb4765d0149750d423ff9a0cb5195043d57ec370884a0309f82fe for libdcmdata
and 5a13d61cd599f477222a7788198301f974409f3a3108b3faac3dd93d4e8f31b3 for libdcmjpls.

[Rerun the harness](../../README.md). This run used five trials, 64 cycles of
four accesses per process, and alternated adapter order. The CSV has all
210 observations. The caller buffer benchmark separately has five samples
per variant. Fixtures were synthetic. [Timing CSV](results.csv) ·
[Fixture hashes](fixtures.csv) · [Frame hashes and layout](parity.csv) ·
[Go benchmark](caller-buffers.txt)

## Valid framing and exact samples

All 40 candidate Frame and FrameInto rows matched the analytical
signed, little-endian fixture. In 38 rows DCMTK matched the raw SHA-256,
the same index and the pixel format. In the remaining two rows (resident
and deferred JPEG-LS with two Items per frame and an empty BOT), DCMTK
returned its expected cannot-determine-start-fragment error for the
unhinted seek to frame 63. It returned matching output for first, next
and return. Empty-BOT RLE kept one Item per frame and matched across all
positions. The procedure checked the next Item returned by DCMTK.

With a valid BOT, Go's deferred JPEG-LS setup read 784 bytes through
ReaderAt for one Item per frame and 1296 bytes for two. Every decode read
only its encoded frame bytes after setup. DCMTK deferred JPEG-LS loaded
one or two Items per frame with no foreign Items. With an empty BOT and
two Items per frame, Go scanned 1,342,776 ReaderAt bytes during reader
setup and then still read only the requested frame bytes on each operation.
DCMTK accessed one extra Item of the next frame on first, next and return
to determine the frame boundary. It then decoded only the selected frame.
For RLE DCMTK returns the next Item after the requested one and drops the
current file-backed Item inside the decoder. Go reads 70,208 encoded bytes
through ReaderAt per deferred RLE frame in this corpus.

## Runtime and allocation

Median timing in microseconds per frame, with observed minimum–maximum
across five trials. Go allocation is median allocated bytes per FrameInto
call, with the caller's 131,072-byte destination reused by both adapters.

| Fixture | Storage | DCMTK | Baseline FrameInto | Candidate FrameInto | Candidate Frame | Go B/op before → after |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| RLE | Resident | 56.8 (50.0–60.0) | 154.6 (127.1–178.2) | 173.5 (142.3–328.5) | 242.7 (230.0–280.3) | 73,744 → 73,744 |
| RLE | Deferred | 161.0 (112.4–178.7) | 150.0 (108.1–190.5) | 174.6 (128.4–189.4) | 230.3 (200.4–289.6) | 73,747 → 73,747 |
| JPEG-LS | Resident | 613.0 (584.8–632.7) | 1509.8 (1253.6–1635.3) | 1477.2 (1165.7–1837.5) | 1395.3 (1274.9–1483.0) | 297,776 → 35,259 |
| JPEG-LS | Deferred | 817.8 (746.6–926.2) | 1931.4 (1333.9–2369.9) | 1738.2 (1580.0–1863.3) | 1868.0 (1589.5–2348.4) | 297,800 → 35,261 |
| JPEG-LS, split | Resident | 681.9 (632.8–763.4) | 1689.9 (1390.7–1900.7) | 1610.4 (1103.2–2439.2) | 1757.2 (1599.1–1805.1) | 297,779 → 35,259 |
| JPEG-LS, split | Deferred | 731.8 (644.4–2565.7) | 1318.4 (1201.1–3228.2) | 1446.7 (1258.7–2496.2) | 1353.2 (1175.2–2659.0) | 297,801 → 35,261 |

JPEG-LS FrameInto allocated about 88% fewer bytes. The JPEG-LS direct
path bypasses the codec-owned decoded frame and the FrameReader-owned
normalized frame assembled for the allocating API. It writes decoded
samples in little-endian order to the negotiated caller destination.
The separate Go benchmark's JPEG-LS FrameInto copy and direct cases
measured 300,774 and 38,267 B/op at the median, with 19 and 7 allocations
per operation. The corresponding median durations were 1.912 and
1.717 ms, with overlapping ranges. The separate unchanged RLE
allocation control had observable timing variability across processes.
The paired DCMTK run also showed wide timing ranges and gives no
consistent JPEG-LS latency improvement across all storage profiles.
DCMTK's JPEG-LS medians remain approximately 2.0–2.4× faster.

Go index lookup was about 0.1 µs per phase at the clock's resolution,
with no allocations. Resident JPEG-LS RawFrame copied a completed
encoded frame into an owned result and allocated a median 18,752 B/op.
File-backed JPEG-LS RawFrame included reading the encoded Items and took
about 8–19 µs per phase. Its one-shot setup and per-access ReaderAt counts
are separate from the decoder. In both the baseline and candidate CPU
profiles, entropy and context processing accounted for most CPU time.
The measured cumulative share of decodeLine was 74.2% in the baseline and
88.6% in the candidate. Regular-mode context updates, Golomb codes and
the scan loop remained the largest costs after removing the output copies.
The profiling run used the same pixels and fragment layout before
the synthetic Pixel Measures attributes were sorted by tag.
[Baseline profile](base-cpu.txt) · [Candidate profile](candidate-cpu.txt)

The reference RLE decoder source constructs one 65,536-byte stripe
workspace for this fixture and interleaves its two decoded byte planes
into the caller's destination. The JPEG-LS source assembles an encoded
frame in one allocation equal to the encoded fragment lengths before
calling CharLS to decode to the destination. The CSV measures candidate
Go allocation traffic and read bytes. The reference decoder's complete
heap allocation totals were unavailable, so the source workspace sizes
describe named buffers only. The Go fixture code and source inspection
describe the copy stages without assigning their exact isolated latency.

## Windows frontend verification

The MacStyle and WindowsStyle Fyne test-driver paths each passed the
same Enhanced CT first/seek/return checks three times per codec. All
first outputs and jumps were verified while progressive preparation
reported pending. Both styles checked the rendered sample values,
frame indices and successful volume completion. The first display
test starts after publication of the even-slice load event. Its timings
include contention and are not cold native-window latency measurements.
The tested application paths continue to use the allocating Frame API.

The library suites covering caller ownership, concurrent readers,
preflight byte limits, cancelation, truncated codestreams and explicit
JAI/HP recovery passed. Twin's frame cache suite and the lazy multiframe
tests covered cache admission and reuse.
