# JPEG 2000 / HTJ2K Production Profile Decision

## Decision


It is **not** sufficient by itself for the clinical `codecfull` production
profile. The qualified profile uses OpenJPEG 2.5.4 for JPEG 2000 Part 1/2 and
OpenJPH 0.31.0 for HTJ2K. Its external clinical fixtures, independent pixel
references, fail-closed runtime checks, and performance evidence are documented
in [`../../../docs/CODECFULL_PROFILE.md`](../../../docs/CODECFULL_PROFILE.md).
The packaged `ojph_expand` executable must have an adjacent
`<executable>.codecfull` sidecar containing exactly the qualified OpenJPH
version and source commit exposed by `QualifiedOpenJPHMarker`; this stable
packaging marker avoids depending on incidental strings retained in a stripped
binary.

## Core backend decision


**Decision: the `NewCore`/`RegisterCore` Part 1 backend stays optional and
opt-in.** It is qualified only for the subset demonstrated below and is not
registered by `New`, `Register`, `-tags jpeg2000` or `codecfull`. OpenJPEG and
OpenJPH remain the qualified `codecfull` profile and are not removed; Part 2 and
HTJ2K are not claimed for this backend. The decision is about decoding only:
the backend has no encoder and changes no transport or negotiation behavior.

### Demonstrated subset


### Corpus and oracles


Beyond the corpus, `TestCoreFeaturesAgainstOpenJPEG` and
`TestCorePackedHeadersAndPOCAgainstOpenJPEG` (opt-in,
`DICOM_GO_OPENJPEG_COMPRESS`/`DICOM_GO_OPENJPEG_DECOMPRESS`) encode 25 more
streams with OpenJPEG and require the core to equal `opj_decompress` exactly
for reversible ones (and the source samples), within one step for
irreversible ones, including `opj_decompress -r` for reduced resolution; and
`TestDecodeCoreReducedMatchesOpenJPEG` compares seven reduced decodes with
committed `opj_decompress -r` output in every run, and
`TestCoreQualityLayersMatchOpenJPEG` four decodes of the first layers (one
and two of three, at full and reduced resolution) with committed
`opj_decompress -l`/`-r` output, exactly. Every gdcmData JPEG 2000
file decodes except FUJI's, whose Bits Allocated and Bits Stored are 0xFFFF.

- Lossless fixtures must equal the source samples **and** the OpenJPEG
  reconstruction exactly; there is no 1-LSB tolerance.
- Irreversible fixtures compare the core with OpenJPEG decoding the same
  codestream. Each has its own policy (maximum absolute error, minimum PSNR per
  component, maximum mean absolute error per 8x8 tile) with a rationale in the
  manifest: 9/7 synthesis in float32 against OpenJPEG's fixed-point path rounds
  a few samples differently. Measured: maximum error 1, PSNR at least 73 dB
  (8-bit), 118 dB (16-bit) and 73.2 dB (ICT colour).
- OpenJPEG is the independent oracle; the adapted upstream decoder is never its
  own reference, and the two pure-Go backends are not compared with each other.
- Exclusions are explicit: the two Part 2 and three HTJ2K fixtures stay with
  OpenJPEG/OpenJPH.

`TestCoreQualificationCorpus` writes the report (per fixture: oracle, policy,
first deviation by frame/row/column/component, per-component metrics, totals
and exclusions) when `DICOM_GO_J2K_CORE_REPORT` names a file. Negative and
resource evidence: `TestCoreRefusesInvalidMetadataStreamsAndFeatures` (size and
precision conflicts, truncation, an RGN style other than Maxshift, JP2),
`TestCoreRefusesTruncatedCorpusCodestreams` (every corpus codestream cut at
five points), `TestCoreBudgetsRefuseBeforeDecoding`,
`TestCoreCancellationStopsWorkAndPublishesNothing`,
`TestCorePixelRepresentationMismatchCorpus` (the synthetic equivalent of
`J2K_pixelrep_mismatch.dcm`; the reconciliation policy is in the README) and
`TestCoreMultiFrameCorpusLayouts`.


### Benchmarks

`BenchmarkJPEG2000Backends` verifies every backend's output against the
cohort's oracle before timing it; a backend that cannot produce equivalent
output is reported as an unsupported combination, never timed. `core` is frame
planning, decode and native packing; `core-decode-only` is the
codestream-to-planes stage; `openjpeg` includes temporary files and one
`opj_decompress` process per frame; `openjpeg-process-startup` is one process
launch with its version check (9.2 ms median, 95.7 KB/op in Go).

Environment: Windows 11 Pro 10.0.26200, windows/amd64, Intel Core i7-14700K,
Go 1.26.4, OpenJPEG 2.5.4 (MSYS2 UCRT64). Six repetitions; medians, with the
range across repetitions:

```sh
DICOM_GO_OPENJPEG_DECOMPRESS=/path/to/opj_decompress DICOM_GO_J2K_BENCH_REQUIRE_OPENJPEG=1 \
  go test -tags jpeg2000_openjpeg -run '^$' -bench BenchmarkJPEG2000Backends -benchmem -count 6 .
```

| Cohort | Backend | Time | Range | B/op | allocs/op | Peak heap |
|---|---|---|---|---|---|---|
| CT 512x512 signed 16-bit lossless | core | 31.6 ms | 31.1-32.1 | 9.64 MB | 2832 | 7.39 MB |
| | core decode only | 31.2 ms | 30.8-31.6 | 8.59 MB | 2810 | |
| | OpenJPEG | 49.1 ms | 48.4-50.0 | 1.70 MB | 450 | 1.21 MB |
| MR 64x64, 10 frames, 12-bit lossless | core | 8.11 ms | 8.00-8.16 | 1.84 MB | 6622 | 1.75 MB |
| | core decode only | 7.85 ms | 7.78-8.03 | 1.68 MB | 6580 | |
| | OpenJPEG | 250 ms | 232-260 | 1.13 MB | 4380 | 1.22 MB |
| US 640x480 RGB 8-bit irreversible | core | 38.1 ms | 37.8-38.4 | 38.6 MB | 8773 | 23.8 MB |
| | core decode only | 37.4 ms | 36.9-37.7 | 36.7 MB | 8748 | |
| | OpenJPEG | 51.2 ms | 50.1-51.8 | 2.90 MB | 452 | 2.47 MB |
| 64x33 16-bit extremes, RLCP lossless | core | 0.68 ms | 0.67-0.68 | 120 KB | 616 | |
| | core decode only | 0.66 ms | 0.66-0.67 | 110 KB | 602 | |
| | OpenJPEG | 20.3 ms | 20.0-23.0 | 104 KB | 443 | |
| 64x64 8-bit irreversible | core | 0.18 ms | 0.18-0.19 | 220 KB | 631 | |
| | core decode only | 0.17 ms | 0.17-0.18 | 211 KB | 617 | |
| | OpenJPEG | 19.8 ms | 19.7-20.4 | 102 KB | 444 | |

Reading the table:

- Packing and planning cost 1-3% of the core's time; the rest is decoding.
- OpenJPEG's time is dominated by process startup and file I/O on small frames
  (about 20 ms per frame against 9.2 ms for a bare launch), which is why the
  10-frame MR cohort is 30x slower there. On the two large single frames the
  core is 1.3-1.6x faster end to end, but that does not show a faster decode
  than OpenJPEG's library: its in-process decode time is not isolated here.
- Go-side B/op and peak heap do not include the OpenJPEG child process, whose
  memory is not measured. The core allocates 5-13x more than the Go side of
  the OpenJPEG path on large frames. Peak heap is sampled every 200 µs and is
  left blank where a decode is too short for sampling to mean anything.
- The legacy pure-Go backend (`New`) produced no verified output on any cohort:
  it refuses PixelRepresentation 1 and a 16-bit codestream with BitsStored 12,
  and on the other three cohorts its output differs from the oracle (up to
  every sample). It is therefore not timed; this matches its documented
  boundary of self-encoded streams (Known Gaps below).

### Platforms

Qualification, tests and benchmarks ran on windows/amd64 only. The backend is
pure Go and builds wherever Go does, but no other platform is claimed as
qualified. Missing OpenJPEG fails the benchmark when
`DICOM_GO_J2K_BENCH_REQUIRE_OPENJPEG=1`, and a missing or failing corpus fixture
fails `TestCoreQualificationCorpus`; neither counts as a skip.

### Why not promote (yet)


Until the last two are met the backend stays opt-in; promoting it means moving
`internal/j2kcore` into the base module (an embedded MIT artifact in
`THIRD_PARTY_PROVENANCE.json`) and registering it from `builtin`.

## Covered Profile

The pure-Go `-tags jpeg2000` adapter profile covers:


The adapter validates codestream width, height, component count, component
precision against DICOM `BitsStored`, and signedness before converting through
Go `image.Image`, so RGBA, bit-depth or dimension mismatches cannot be silently
converted into DICOM-compatible native frames. Decompression through
`pixeldata.DecompressFile` can instead correct such objects before any
adapter runs, opt-in and reported: `DecodeRecovery.ReconcileWithCodestream`
takes size, precision, sign and Planar Configuration from the SIZ marker
segment, and `DecodeRecovery.UnwrapJP2Container` passes the codestream of a JP2
file (the adapter's own `AllowJP2Wrapper` stays for direct use).

## Known Gaps

The current pure-Go backend is intentionally limited. These gaps do not
describe the separate `codecfull` release profile:

- External clinical and OpenJPEG/OpenJPH-generated fixtures are deliberately
  outside the pure-Go adapter's qualification boundary. They are checked in,
  hash-verified, and exercised by the separate `codecfull` profile with its
  qualified native runtimes.
- The dependency's tile-data path documents that one decode path only works for
  codestreams produced by its own encoder and returns no tile data for external
  T2 packet layouts (`github.com/mrjoshuak/go-jpeg2000@v1.2.1/decoder.go`).
  Therefore this pure-Go decision remains limited to its synthetic/self-encoded
  conformance profile; external-fixture coverage belongs to `codecfull` and
  does not qualify the pure-Go backend.
- Full JPX/JPEG 2000 Part 2 extension coverage is not proven; current Part 2
  coverage is raw-profile smoke coverage only.
- 16-bit lossless high-value fixtures decode within 1 LSB but are not exact near
  the upper stored-value range; this prevents claiming full lossless production
  exactness.
- JPIP, streaming transfer syntaxes, video transfer syntaxes, transcoding and
  progressive external retrieval are out of scope for this still-image adapter.

## Benchmark Evidence

Command used for the table below:

```sh
go test -bench BenchmarkDecodeJPEG2000Profile -benchmem ./...
```

The `make codec-jpeg2000-profile-check` target intentionally runs only a
`-benchtime=1x` benchmark smoke and is not comparable to this table.

Environment:

- `goos`: darwin
- `goarch`: arm64
- CPU: Apple M4

Latest local result:

| Case | Time | Memory | Allocations |
|------|------|--------|-------------|
| small/lossless | 23748 ns/op | 208741 B/op | 191 allocs/op |
| small/lossy | 18809 ns/op | 209080 B/op | 192 allocs/op |
| small/htj2k | 18492 ns/op | 208852 B/op | 193 allocs/op |
| medium/lossless | 360127 ns/op | 385342 B/op | 499 allocs/op |
| medium/lossy | 1031770 ns/op | 429550 B/op | 506 allocs/op |
| medium/htj2k | 371362 ns/op | 385356 B/op | 501 allocs/op |
| large/lossless | 6547368 ns/op | 1829722 B/op | 1351 allocs/op |
| large/lossy | 11416959 ns/op | 2505882 B/op | 1357 allocs/op |
| large/htj2k | 828175 ns/op | 802664 B/op | 1134 allocs/op |

## Verification Commands

Run these for this profile:

```sh
go test ./...
go test -bench BenchmarkDecodeJPEG2000Profile -benchmem ./...
```

From the `dicom-go` root:

```sh
make codec-jpeg2000-profile-check
```


```sh
make codec-profile-check
```
