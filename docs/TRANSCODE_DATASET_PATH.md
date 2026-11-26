# Incremental preparation from raw dataset snapshots

`pixeldata.TranscodeDataSetPath` accepts a stable raw dataset path, explicit
source and target syntaxes, caller-owned codec registries and dictionary, and
publishes a complete Part 10 output using the same frame planner, worker
pipeline, verification and atomic publication as `TranscodePath`. It does not
create a Part 10 copy of the input or materialize complete top-level Pixel Data.

`DataSetPathOptions.SourceOffset` can skip framing in an already-owned snapshot.
Its dictionary resolves implicit/private values. Source remains caller-owned and
unchanged for the call; path replacement/source revalidation uses the existing
transaction. The returned `DataSetPathReport.TranscodeReport` records streaming,
verification and transformation. `PeakTemporaryBytes` records the logical peak
sum of ordered frame spools and staged output, not process RSS.

## Qualified scope and resident paths

The initial raw entry qualifies lossless little-endian, nondeflated inputs and
outputs with ordinary top-level integer Pixel Data whose frames the existing
reader can locate. Equivalent layouts, native conversion, native encode and
compressed decode/encode use existing algorithms. A missing/malformed codec,
verification failure, cancellation or resource limit is an operation failure.
It is not permission to retry with resident pixels.

`ErrTranscodeStreamingIneligible` marks unsupported framing/layout or explicit
policies before output staging/codec execution. Only that error allows a caller
to choose its established resident route. Big-endian, deflated, packed-bit,
nested Pixel Data/icon, recovery, UN repair, rewrite and identity-changing/lossy
policies retain the adapters' resident route and its finite input/output bounds.
Lossy output remains resident so its first verified derived UID can be rebound
consistently across Inspect, Open and explicitly authorized retry. No implicit
lossy fallback, UID change or representation cache is introduced.

## Resource ownership and send boundary

`MaxTemporaryBytes` is a finite live aggregate bound on frame spools plus staged
output; zero means 2 GiB and negative values are invalid. Charge is admitted
before disk writes. File charges are returned only after successful owned-file
removal; the published output remains charged until call completion. Existing
per-output and finite frame-in-flight pipeline limits apply independently.
Input snapshots and retained output handles must additionally be counted by the
caller. This ledger does not claim a process memory limit.

DIMSE adapters count input snapshots in their existing spool budget and give
only its remainder to incremental preparation. Native aggregate resources retain
their reservation through handle Close. A complete output is parsed with deferred
pixels and its syntax/identity/size validated before returning the send handle.
The retained Part 10 output's dataset offset is used when copying network bytes:
no framing is sent, no output-sized recopy is made and no codec runs on the
network writer. Selected native metadata keeps its raw-dataset observation
contract. Zero-target/default-original and explicit original fallback preserve
original dataset bytes.

## Qualification and measured tradeoffs

Synthetic raw-path, quota, selected-metadata and close tests passed together with affected DIMSE contracts: 26 top-level tests and 51 passing test events, without skips. Local C-STORE, C-GET and C-MOVE peers checked the complete selected pixels and negotiated syntax; original routes and a later-frame preparation failure before command submission were covered. The same focused cases passed race and Go 1.22.12 checks on darwin/arm64; the minimum-toolchain check used CGO external linking for this host. Vet and 13 shared `TranscodePath` contract tests also passed. These observations do not establish real PACS or Windows runtime behavior.

A single fixed campaign compared the complete resident library at commit `7552a71cf6f372f72162d420fa285e13e3923c84` with this incremental implementation. Both source exports received the identical instrument; inputs and explicit registries/policies/limits were identical. On Apple M4 Pro, Go 1.27.1, darwin/arm64, CGO enabled, `GOMAXPROCS=2` and `GOMEMLIMIT=1GiB`, each workload and route ran three times. The 36 cases verified every selected pixel, exact frame count and metadata, UID/class, syntax, codec calls and cleanup. No retries, filtering or retuning occurred.

Inputs were unsigned 16-bit MONOCHROME2, 512×512, with 4/16/64 frames (2/8/32 MiB native pixels). Explicit `Source` prepared RLE Lossless; `NativeSource` selected Implicit VR Little Endian from explicit native input. Explicit Inspect and Open each encoded every frame; native preparation made no Encode calls. Those measured ordinary `Source`/`NativeSource` paths use no session
representation cache. The separate opt-in
[`PreparedSession`](CSTORE_TRANSCODE_SOURCE.md#explicit-session-owned-prepared-representations)
is outside this historical corpus and does not change these measurements; its
separate 12-worker reuse campaign is reported in that guide, with a different
two-frame 2×2 uint8 corpus and explicit measurement limits.

Cells below are median [minimum, maximum] from three runs. Preparation time is Inspect plus Open, excluding process startup and later output verification. RSS is the **whole test-process lifetime maximum**, including independent verification one frame at a time. These shared-host synthetic observations show lower RSS and **higher preparation time in all six pairs**; they establish neither a speedup nor statistical, universal-scaling or clinical benefit.

| Adapter | Native pixels MiB | Resident preparation ms | Incremental preparation ms | Resident lifetime RSS MiB | Incremental lifetime RSS MiB |
|---|---:|---:|---:|---:|---:|
| source-rle | 2 | 11.218 [10.796, 11.607] | 38.303 [37.111, 42.225] | 36.594 [36.328, 36.609] | 28.672 [27.453, 28.797] |
| source-rle | 8 | 32.163 [30.538, 43.321] | 77.280 [74.105, 82.293] | 65.328 [62.969, 91.578] | 29.547 [27.234, 30.016] |
| source-rle | 32 | 113.878 [111.865, 120.153] | 234.528 [226.265, 251.347] | 195.328 [193.672, 195.703] | 31.391 [30.797, 40.969] |
| native-implicit | 2 | 2.560 [2.271, 2.690] | 14.024 [12.172, 14.198] | 24.812 [24.578, 24.922] | 20.734 [20.641, 21.141] |
| native-implicit | 8 | 7.914 [7.782, 8.249] | 22.780 [22.622, 25.653] | 38.453 [38.375, 38.500] | 23.562 [23.547, 23.875] |
| native-implicit | 32 | 25.427 [25.269, 25.796] | 44.975 [43.826, 48.218] | 86.406 [86.391, 86.797] | 23.938 [23.938, 24.406] |

Temporary usage below is the logical sum of all production spool files sampled every 5 ms: an **observed lower bound**, which can miss short resident peaks. The exact retained-boundary total is independently inventoried after Open, before creating the separate verification output; it sums the input snapshot and retained prepared representation. A boundary total can therefore exceed the sampled peak. Temporary disk bytes are not process RSS. Frame staging requires extra disk space, which must fit the aggregate quota.

| Adapter | Native pixels MiB | Resident observed peak MiB | Incremental observed peak MiB | Resident exact boundary MiB | Incremental exact boundary MiB |
|---|---:|---:|---:|---:|---:|
| source-rle | 2 | 2.000 [2.000, 2.000] | 4.228 [4.228, 4.228] | 3.114 [3.114, 3.114] | 3.115 [3.115, 3.115] |
| source-rle | 8 | 12.459 [12.459, 12.459] | 16.916 [16.916, 16.916] | 12.459 [12.459, 12.459] | 12.459 [12.459, 12.459] |
| source-rle | 32 | 49.824 [49.824, 49.824] | 67.647 [67.647, 67.647] | 49.824 [49.824, 49.824] | 49.825 [49.825, 49.825] |
| native-implicit | 2 | 1.875 [0.250, 2.000] | 4.001 [4.001, 4.001] | 4.001 [4.001, 4.001] | 4.001 [4.001, 4.001] |
| native-implicit | 8 | 16.001 [16.001, 16.001] | 16.001 [16.001, 16.001] | 16.001 [16.001, 16.001] | 16.001 [16.001, 16.001] |
| native-implicit | 32 | 64.001 [64.001, 64.001] | 64.001 [64.001, 64.001] | 64.001 [64.001, 64.001] | 64.001 [64.001, 64.001] |

All production spools were empty after Close and native reservations were released. Separate generated verification outputs were removed after process cleanup. Codec/verification/cancellation/limit errors remain preparation failures. The resident exceptions described above remain resident and were not relabeled as incremental. Full application integration and real PACS qualification are separate from these focused synthetic checks.
