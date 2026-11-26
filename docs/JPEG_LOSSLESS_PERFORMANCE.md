# JPEG Lossless performance qualification


## Changes and correctness

The decoder uses an eight-bit canonical Huffman lookup and a 64-bit entropy
cache. Short codes and magnitudes that fit eight bits are decoded together;
long codes and short tails use the canonical path. Prefetch stops before JPEG
markers and preserves stuffing, fill-bit and restart validation. Selection
value 1 has a row loop that preserves the first-sample prediction; restarts and
the Cornell category-16 extension retain the general path. Native output uses
fixed mono/RGB strides, including each component's point transform.

The encoder retains one packed difference/category word per sample, plus one
previous row for predictors 2–7. Predictor 1 needs only the left and preceding
row's first sample. `bits.Len32` replaces category iteration and the entropy
writer packs a code with its magnitude in one write. Huffman frequencies, code
ordering and encoded bytes stay unchanged.

Seventy frozen SHA256 values cover seven predictors and ten mono/signed/RGB/YBR
profiles, stored precisions 2–16 and row-boundary/extreme samples. Every value
matches unoptimized source. Full MR/RGB Part10 outputs also retain the same
SHA256 in all ten before/after encode samples per profile. Differential Huffman
tests cover all 65,536 signed differences, categories 0/16, nine-/sixteen-bit
codes, stuffing, short tails and markers. Existing tests cover separate scans,
point transforms, restarts, recovery quirks and concurrent encoder use.

## Measured file pipeline

Windows 11 Pro 10.0.26200 / amd64, Intel Core i7-14700K, Go1.26.4,
`GOMAXPROCS=1`, DCMTK3.7.0 from MSYS2 UCRT64. Other campaign agents paused
tests/builds/native tools during timing; no compiler or DCMTK process was
active at its start. Desktop services stayed running; CPU affinity was not set.

Each operation includes Part10 parsing, transcoding, file write and close,
with warm file caches and no fsync. Go runs in its warm test process; DCMTK
starts a new process for each sample. Go's default lossless encode verification
remains enabled. Fixture creation, cross-decoder qualification, output readback,
exact sample comparison and SHA256 calculation occur outside timing. Five
samples alternate the Go/DCMTK order. Decode compares the same DCMTK stream.

DCMTK's MR stream has SOF3 precision16 while retaining dataset BitsStored12 and
HighBit11. Strict Go correctly rejects that mismatch. Both Go versions use the
existing explicit `ReconcileWithCodestream` recovery for that external stream;
their own output stays strict precision12. Reconciliation changes metadata to
the codestream precision and preserves all source samples. It does not relax
the codec's default metadata contract.

Median throughput, native bytes / elapsed time:

| Profile / operation | Before Go (MiB/s) | Optimized Go (MiB/s) | DCMTK in optimized run (MiB/s) | Optimized Go time (ms) |
| --- | ---: | ---: | ---: | ---: |
| MR 512×512, 12 stored bits, 40 frames / encode verified | 149.77 | 247.59 | 218.78 | 80.78 |
| MR / decode | 100.43 | 305.32 | 273.23 | 65.50 |
| RGB 640×480, 8 bits, 20 frames / encode verified | 89.17 | 118.59 | 108.86 | 148.23 |
| RGB / decode | 75.41 | 193.86 | 166.45 | 90.68 |

Optimized medians exceed DCMTK for all four measured operations. Both encoders
were qualified with both decoders before timing; all 120 retained timed
outputs additionally preserve exact source samples. Encoded Go Part10 sizes
stay 6,733,232 bytes (MR) and 3,889,754 bytes (RGB). Go and DCMTK use their own
bitstreams; byte identity here compares Go before and after optimization,
while independent decoding compares native samples.

## Codec microbenchmarks and memory

Five samples, `-benchtime=300ms -benchmem`; setup/encoding before decode is
outside its timer, and decode includes native interleaving. Times and allocated
bytes below are medians; allocation counts remain constant within each case.

| Case | Before ns/op | Optimized ns/op | Before B/op | Optimized B/op | Before / optimized allocations |
| --- | ---: | ---: | ---: | ---: | ---: |
| Encode MR | 3,138,893 | 1,739,032 | 2,373,386 | 1,322,100 | 13 / 12 |
| Encode RGB | 9,638,244 | 7,132,769 | 7,870,760 | 4,173,860 | 27 / 23 |
| Decode MR | 4,685,154 | 1,549,299 | 1,574,184 | 1,576,040 | 16 / 16 |
| Decode RGB | 10,934,514 | 4,368,562 | 4,614,326 | 4,619,895 | 21 / 21 |

Encoder allocation falls by about 44% for MR and 47% for RGB. Decode retains
owned sample planes; the small extra allocation is the per-frame lookups.
These are allocation measurements, not peak RSS or GPU memory measurements.

## Reproduce and validation

From the library root, with installed native tools on PATH:

```powershell
$env:GOMAXPROCS = '1'
$env:DICOMGO_DCMTK_INTEGRATION = '1'
$env:DICOMGO_DCMTK_BIN = 'C:\msys64\ucrt64\bin'
$env:DICOMGO_JPEGLOSSLESS_CAMPAIGN = '1'
$env:DICOMGO_JPEGLOSSLESS_CAMPAIGN_DIR = Join-Path $env:TEMP 'jpeg-lossless-campaign'
go test -p 1 ./pixeldata/jpeglossless -run '^TestLosslessPerformanceCampaign$' -count=1 -v
go test -p 1 ./pixeldata/jpeglossless -run '^$' -bench '^Benchmark(Encode|Decode)SV1(MR16|RGB8)$' -count=5 -benchtime=300ms -benchmem
```

The campaign skips by default and qualifies samples rather than asserting
unstable timing in ordinary tests. Measurement used binaries compiled before
the quiet interval with `go test -c`; compilation was excluded. Baseline used
`git archive 212dd7fb dicom-go-dev` and only compatible campaign/golden test
files copied into an isolated extraction. Fixtures are generated locally;
no patient or third-party fixture is needed.

Full affected packages, full codec race and bounded multirow mono/RGB
`FuzzEncoderRoundTrip` passed. The final source fuzz run exercised 166,816
inputs in 10 seconds with two workers (11.539s including startup/drain).
Native macOS, other CPUs, live PACS and GPU runtimes were not exercised for
this pure-Go codec campaign.

## Integration checks

The integrated campaign was compiled against `1c885132` plus this codec patch,
including the explicit encoder-compatibility API added earlier that day. The
receipt records those caller-source hashes; all four integrated medians pass
the DCMTK comparison and both complete Part10 bitstreams remain identical to
the baseline. Candidate timings are retained separately, not overwritten.

`go test -p 1 ./pixeldata/jpeglossless ./pixeldata/builtin
./pixeldata/codecfixture -count=1` passed again after integration. Twin's actual
JPEG-family loading, independent lossless RGB rendering and planar refusal
selectors passed. Full codec race passed on the identical codec source.
`make check` passed formatting and vet, but broad tests failed unchanged
Windows permissions/path/socket contracts and existing mesh/registration/CPR
numerical or rendering timing assertions. This is not a green module baseline.
The remaining targets, `make build docs-check codec-policy-check
codec-optional-check codec-conformance-check codec-manifest-check`, passed.
All three native Windows Twin builds and its release-compliance checks passed
against the integrated library. Native-only and unrelated baseline failures
are not substituted by the focused codec or performance results.
