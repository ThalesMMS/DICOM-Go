# Resident 2D frame identity

`render.Frame.CacheIdentity` optionally names one immutable input generation.
Adapters can be recreated while preserving the same opaque `FrameIdentity`.
Without a token, `RenderCache` retains its historical `*Frame` pointer keys.
SOP Instance UID, file path and frame number alone are not cache identities.

The caller owns generation changes: use a new token after changing pixels,
rescale, color conversion or other render metadata. A frame and its input bytes
remain immutable while used by a cache. Copying an adapter with the same token
is valid only for the same input generation. Tokens contain no source frame,
slice, pixels, file handle or dataset. The library stores keys in its existing
decoded, windowed and pinned maps; there is no additional identity registry.
Normal byte limits, pin replacement, pressure eviction and `Clear` still apply.
Decoded entries with tokens no longer retain the input `Frame` or raw bytes.
Cache occupancy telemetry continues to report decoded/window pixel payloads,
not Go map, adapter or source-metadata overhead.

`LookupFrame` atomically retrieves/promotes an already windowed image without
loading pixels or decoding. It records a hit when found and leaves misses
uncounted, so a caller can check residency before its normal loading workflow.
An identity-only adapter is valid for lookup/pinning, never for decoding.
`Clear` rejects cache admission from library decodes already in progress when
it cleared the stores. It does not cancel callers or prevent a caller starting
another render after the clear from populating the new cache generation.


Each study slice owns a token under its pixel mutex. Explicit pixel edits and
synthetic pixel/grid replacement invalidate it at the producing boundary.
Local lazy pixel eviction/rehydration preserves identity for the same immutable
source; changed decoded metadata invalidates it. JPIP retrieval eviction/load
keeps its generation invalidation. Parametric Map materialization also owns its
token, and rendering still runs the map's validity check before using a cache.
The adapter LRU remains bounded to 512 entries independently of pixel payload
size; recreating an adapter no longer loses a resident decoded/windowed hit.

A local resident window can be shown, tested for presence and pinned without
reopening an evicted raw source. This remains possible while that source file
is temporarily unavailable; after `Clear`, missing source files fail normally.
JPIP and Parametric Map rendering retains the existing source validity checks.
The token does not extend a source/volume lease or authorize an old regional
reader: source-close and regional-read lifetime rules remain separate. Closing
a series volume continues to preserve slices for their independent 2D owner.

Public slice fields used as render inputs remain caller-coordinated: direct
metadata changes require clearing affected render caches before reuse, as
before. This implementation does not poll file modification times or make
unsynchronized mutation of borrowed pixels safe during an active render.

## Qualification and measured scope

The regression suite covers 1,025 resident one-pixel slices, retained window
identity after adapter eviction, exact existing rendered pixels, recreated
adapter pinning, pressure eviction, pixel/grid generation changes, invalid
Parametric Map source rejection, and concurrent revisit/pin/clear. A separate
real Part 10 test loads 513 files with a four-byte raw-source LRU, removes the
first source file after warming, and verifies presence/pinning/revisit without
another raw decode. Clearing then requires the unavailable source and fails.
The library also blocks a decode across `Clear` and checks that it cannot
repopulate the retired stores.

Three alternating baseline/candidate process pairs were measured on Windows
amd64, Go 1.26.4, Intel i7-14700K, benchmark CPU count 1. Baseline was commit
`687a4879`. Both sides used identical benchmark source, 16 MiB per pixel store,
10 complete warm revisit cycles per case, and the same fixtures. Values below
are medians. The filesystem cache was uncontrolled; these are warm component
workflows, not cold study-open, UI adoption, GPU, FPS or physical scanout data.

| Complete warm task | Before | After |
| --- | ---: | ---: |
| Revisit all 1,025 resident one-pixel frames | 1.492 ms | 0.181 ms |
| Allocations for that complete task | 1,606,396 B / 16,411 allocs | 328,320 B / 513 allocs |
| Final resident pixel payload after 10 cycles | 56,375 B | 5,125 B |
| Revisit all 513 file-backed 2x2 frames, raw LRU 4 B | 48.331 ms | 0.048 ms |
| Allocations for that complete task | 8,330,921 B / 102,610 allocs | 640 B / 1 alloc |
| Raw source decodes per complete file task | 513 | 0 |
| Final resident pixel payload after 10 file cycles | 112,860 B | 10,260 B |

One benchmark process runs both cases, including fixture writes, parsing,
warming, testing startup and cleanup. Median whole-process wall time was
1,209 to 670 ms; CPU time 1,125 to 594 ms; Windows peak process working set
66,932,736 to 57,466,880 bytes (sampled via the process high-water property).
These totals are not per-revisit CPU/peak measurements. No child processes were
created by the benchmark binaries. Cold-run costs and clinical-size UI latency
were not measured. Small token-only adapters still allocate when the adapter
LRU lacks an entry; the change does not add an unbounded adapter cache to avoid
that cost.

The RAV64 architectural reference is static image-object reuse in
RadiAntViewer.exe at `0x140115330` (retained decompilation lines 220337-220422).
It motivated checking identity ownership; no recovered code or binary was
copied, and no RAV64 execution/performance comparison was performed.

Run focused checks from each module:

