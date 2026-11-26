# Caller-owned frame and region buffers

`FrameReader.FrameInto(ctx, index, dst)` and `RegionInto(ctx, index, rectangle,
dst)` complement the existing allocating `Frame` and `Region` methods. They
reuse a caller-owned destination without retaining it. No application cache or
viewer policy is introduced.

```go
layout, err := reader.FrameBufferLayout(ctx, 0)
if err != nil {
    // A legacy codec may report ErrBufferLayoutUnavailable; decode/copy can
    // still be used and BufferSizeError reports its actual required size.
    return err
}
dst := make([]byte, layout.Bytes)
for i := 0; i < reader.Metadata().NumberOfFrames; i++ {
    result, err := reader.FrameInto(ctx, i, dst)
    if err != nil {
        // result.Written bounds potentially touched storage, not valid pixels.
        return err
    }
    consume(dst[:result.Written], result.Layout.Metadata)
}
```

`FrameBufferLayout` and `RegionBufferLayout` negotiate direct-path requirements
without reading pixel payloads. Layout describes one decoded frame/region,
packed little-endian with its actual planar/interleaved metadata. Rectangles use
stored-frame coordinates. No stride, output color-space selection or arbitrary
layout conversion is provided. The destination's **length**, rather than spare
capacity, bounds writable bytes; explicitly reslice before use. The result of
each operation is authoritative if a registry changes after a separate query.

The output owns no reader storage. Subsequent operations cannot overwrite a
previous destination unless the caller supplies that same storage again. The
caller must keep input sources alive and immutable, use distinct destinations
for concurrent calls, and provide a concurrently safe ReaderAt and codec.
`Close` must not race with operations. An optional codec must borrow encoded
input and destination only until return; it may keep no alias to either.

## Direct paths and copy fallback

| Result mode | Destination writes | Other allocations still possible |
|---|---|---|
| `native-direct` | ReaderAt writes the destination, or the library copies from aligned storage units | ReaderAt implementation and up to 64 KiB of scratch for shared words or bytes |
| `codec-direct` | Optional FrameBufferDecoder writes destination | Encoded frame assembly, codec workspaces, diagnostic slices |
| `frame-copy` | Allocating Frame result copied to destination | Entire decoded frame plus codec workspaces |
| `region-copy` | Existing regional decoder result copied to destination | Allocated regional output plus codec workspaces |
| `frame-region-copy` | Entire decoded frame cropped, then copied | Entire frame and allocated crop plus codec workspaces |

Native frames retain existing packed layout, including whole-frame 4:2:2;
4:2:2 regions continue to return `ErrFrameLayout`. A native region reads its
requested rows plus the bytes or words that share their boundary samples.
One-bit output starts at bit zero with zero spare bits at the end. Native
reads check for cancellation between rows and chunks of at most 64 KiB. A
blocking ReaderAt call must return before the check can run. For aligned
Big Endian samples byte reversal occurs directly in the destination.

RLE is the first in-tree `FrameBufferDecoder`: interleaved little-endian output
is written directly into the caller destination. Whole-frame requests reuse
RLE’s full-frame strict/recovery decoder against the destination without a
decoded frame or byte-band allocation. Regional requests use a decoded byte-band
scratch covering stored column width and requested row height, reused across
segments. `FrameBufferRequest.WholeFrame` distinguishes full-frame admission
from regional prefix clipping, even when the rectangle covers the entire frame. Encoded frame bytes still allocate; this is not zero-allocation codec
decoding. Context recovery policy and RLE overrun diagnostics remain available.
Pure-Go JPEG-LS writes whole frames and qualified monochrome regions directly
into the caller destination. Region writes retain three rolling predictor rows
and validate entropy through the last frame row. The explicitly registered
Near-Lossless codec supports the same monochrome region path. Other color
profiles and adapters use their existing fallback. The whole-frame path
reports applied pad, JAI and HP diagnostics. The regional path preserves
applicable pad and JAI diagnostics.

Codecs without direct support continue through explicit copy modes. A codec
can decline one region using `ErrRegionUnsupported`; that operation follows the old decoder
or complete-frame/crop fallback and reports the mode actually used.

## Failure and cancellation state

- Invalid index/rectangle/layout, an initially canceled context, and an
  insufficient destination leave all destination bytes unchanged. A
  `BufferSizeError` wraps `ErrBufferTooSmall`, with exact Required and Provided
  lengths. Legacy codecs may need to decode first to discover required size;
  this decode does not write the destination.
- After a direct read/decode starts, cancellation or a source/codec error can
  leave partial writes. `Written` is the highest potentially touched offset:
  bytes at offsets `Written` and above remain unchanged. It is **not** a
  guarantee that the lower prefix is valid, dense, or in complete samples.
  For instance RLE regional byte segments write sparse positions and truncated native
  reads may touch part of a big-endian sample. Whole-frame RLE initializes the
  entire footprint to zero for tolerated short segments; errors after that
  initialization conservatively report the whole footprint touched. Discard
  that output as a frame.
- Success writes exactly Layout.Bytes; bytes beyond that footprint stay
  unchanged. Codecs see a slice whose length and capacity both equal the
  negotiated footprint. Invalid codec extents are rejected; the full
  footprint is conservatively reported potentially touched.
- The direct codec path checks decompression budgets before payload access
  or destination writes. Native layout admission checks the requested pixel
  count, output bytes, and the actual bytes to be read. Whole native Big
  Endian frames also validate other binary metadata before writing pixels.
- Copy fallbacks preserve the whole destination on decode failure or short
  output. They check cancellation before the final copy; a completed copy is
  not rolled back if cancellation arrives afterward.

## Qualification and measurement

Synthetic parity tests cover 8/16-bit mono and RGB, planar/interleaved native
frames, little/big endian, all frames and full/interior/edge/single-row regions,
RLE and JPEG-LS direct whole-frame and region paths. Additional cases check refused 4:2:2 regions,
short slices with spare capacity, malformed codec layout/output, bounded codec
input/decoded limits, ReaderAt errors and cancellation, sparse RLE errors,
concurrent distinct destinations and source ownership after reuse.

Run the affected tests from the library module, using the scheduling/limits of
the local execution supervisor:

```sh
go test -p 1 ./pixeldata ./pixeldata/rle -run '^(TestFrameReaderInto.*|TestRLECallerBuffer.*)$' -count 1 -timeout 60s
go test -race -p 1 ./pixeldata ./pixeldata/rle -run '^(TestFrameReaderIntoConcurrentOwnership|TestFrameReaderIntoPartialSourceAndCancellation|TestRLECallerBuffer.*)$' -count 1 -timeout 60s
go test -p 1 ./pixeldata -run '^(TestFrameReaderMatchesFullDecode|TestFrameReaderRegion.*|TestFrameReaderDirect.*)$' -count 1 -timeout 60s
```

`BenchmarkFrameReaderCallerBuffers` pairs allocated/into operations on 32
256x256 mono16 frames and 64x64 regions, rotating frame index. Constructor,
encoding, warming and the caller's reused destination allocation are outside
the timer. Native LE/BE, RLE direct, capability-hidden RLE frame/regional copy,
JPEG-LS direct and capability-hidden JPEG-LS copy are separate rows. The codec
wrappers hide only the caller-buffer capability, preserving identical inputs
and legacy decoding.
Encoded frame allocations, RLE scratch and full-frame/crop fallback costs stay
inside the timer. The measured run below qualifies native output-allocation reduction for these
synthetic steady-state workloads. Reproduce its recipe with:

```sh
GOMAXPROCS=2 GOMEMLIMIT=1GiB go test -p 1 ./pixeldata -run '^$' -bench '^BenchmarkFrameReaderCallerBuffers$' -benchmem -count 5 -benchtime 200ms -timeout 120s
```

Record host/toolchain/source hashes, exact commands, ns/op, B/op and allocs/op
per pair in a quiet measurement lane. Record the caller's one-time destination
bytes separately (131072 frame / 8192 region), not as absent memory. Reuse is
not an assertion that every codec becomes faster or stops allocating. These
commands are reproduction recipes. Actual evidence and its limits follow;
they do not replace the integration baseline or qualify every codec/runtime.

### Recorded functional and race checks

On macOS (`darwin/arm64`, Apple M4 Pro), configured Go 1.27.1, the exact fourteen
`TestFrameReaderInto*` / `TestRLECallerBuffer*` top-level tests passed across
`pixeldata` and `pixeldata/rle`, with zero failures or skips. The RLE RGB fixture
uses supported interleaved encoder input and then a compressed planar-1 header;
8/16-bit Frame/Region parity checks retain the existing interleaved decoded
output. An earlier fixture-setup encoder failure is preserved separately.

One selective race run passed these three top-level tests in `pixeldata`, with
zero failures or skips:

```sh
go test -race -p=1 ./pixeldata -run '^(TestFrameReaderIntoConcurrentOwnership|TestFrameReaderIntoPartialSourceAndCancellation|TestFrameReaderIntoCodecContractsAndLimits)$' -count=1 -timeout=60s
```

This is affected ownership/concurrency/cancellation evidence, not a full-module
race run. Both functional and race checks verified the frozen seven-file
candidate and complete private source inventory before/after/final, with owned
process groups drained and reaped. The integration baseline remains a separate
delivery check.

### Recorded benchmark: 100 rows, five measurements per case

The recipe ran once with `GOMAXPROCS=2`, `GOMEMLIMIT=1GiB`, `-p=1`,
`-count=5`, `-benchtime=200ms`, `-timeout=120s`; the child process exited zero in
27.8295 seconds. All twenty frame/region/allocated/into cases produced exactly
five rows. The table reports median [minimum, maximum], retaining all five
measurements; units are ns/op, B/op and allocs/op.

Constructor, encoding, initialization/warm-up and caller buffer allocation are
outside timing. In particular, the old Big Endian `Frame` warm-up can prepare
and cache converted reader state: these are **steady-state reused-reader**
costs, not end-to-end memory/RSS or first-decode costs. The caller still owns a
131072-byte frame buffer or 8192-byte region buffer, allocated once and reused.
A measured zero B/op on a native Into row does not mean zero live memory.

| Profile / operation / API | ns/op median [min, max] | B/op median [min, max] | allocs/op median [min, max] |
|---|---:|---:|---:|
| native-le / frame / allocated | 6329 [5981, 6510] | 131072 [131072, 131072] | 1 [1, 1] |
| native-le / frame / into | 1741 [1738, 1908] | 0 [0, 0] | 0 [0, 0] |
| native-le / region / allocated | 743.6 [736, 754.2] | 8192 [8192, 8192] | 1 [1, 1] |
| native-le / region / into | 621.7 [618.6, 624.8] | 0 [0, 0] | 0 [0, 0] |
| native-be / frame / allocated | 62941 [62761, 63292] | 393350 [393350, 393351] | 9 [9, 9] |
| native-be / frame / into | 47126 [46884, 47650] | 0 [0, 0] | 0 [0, 0] |
| native-be / region / allocated | 3926 [3912, 4010] | 16384 [16384, 16384] | 2 [2, 2] |
| native-be / region / into | 3691 [3680, 3739] | 0 [0, 0] | 0 [0, 0] |
| rle-direct / frame / allocated | 61427 [60331, 62311] | 336058 [336057, 336058] | 10 [10, 10] |
| rle-direct / frame / into | 47909 [47643, 48292] | 73744 [73744, 73744] | 2 [2, 2] |
| rle-direct / region / allocated | 23332 [23104, 23482] | 98344 [98344, 98344] | 5 [5, 5] |
| rle-direct / region / into | 25259 [24658, 27229] | 90128 [90128, 90128] | 3 [3, 3] |
| rle-copy / frame / allocated | 66422 [60711, 67662] | 336057 [336056, 336058] | 10 [10, 10] |
| rle-copy / frame / into | 62798 [62341, 63284] | 336059 [336058, 336059] | 10 [10, 10] |
| rle-copy / region / allocated | 23844 [23732, 24980] | 98344 [98344, 98344] | 5 [5, 5] |
| rle-copy / region / into | 23957 [23915, 23985] | 98344 [98344, 98344] | 5 [5, 5] |
| jls-copy / frame / allocated | 1014107 [1010841, 1015073] | 296660 [296658, 298889] | 18 [18, 18] |
| jls-copy / frame / into | 1020570 [1015418, 1043470] | 296656 [296655, 301140] | 18 [18, 18] |
| jls-copy / region / allocated | 1022123 [1019383, 1057196] | 309338 [307090, 311632] | 19 [19, 19] |
| jls-copy / region / into | 1040703 [1033071, 1091106] | 307106 [304848, 307266] | 19 [19, 19] |

All four native LE/BE frame/region pairs met the predeclared criterion: strictly
lower median B/op **and** allocs/op with Into. No universal latency improvement
is claimed. RLE direct whole-frame output removes the decoded frame/band
allocation, while encoded input still allocates; its regional byte-band scratch
also remains timed. RLE direct Region reduced median B/op from 98344 to 90128
and allocations from five to three, but median time rose from 23332 to 25259
ns/op (**+8.26%**). Capability-hidden RLE preserves the old full/regional codec
and reports `frame-copy` / `region-copy`; JPEG-LS reports `frame-copy` /
`frame-region-copy`. Their copy/codec costs remain timed, with no promise of
allocation-count or latency improvement. The table retains their noise and
regressions as well as improvements.

The original benchmark **supervisor receipt remains `FAILED_PRESERVED`**,
despite the successful child and complete measured rows. Its parser overwrote
the source-inventory variable `expected` with a set of benchmark case names
before the final source guard. The closure then compared a source dictionary
to that set, causing a deterministic false mismatch. Full before/after source
guards were exact; a separate complete current inventory/frozen-postimage audit
and independent raw-log/count/median audit reconciled the measurements and
identified this wrapper defect. No benchmark was rerun, and the failed receipt
was not relabeled. This reconciliation qualifies the recorded measurements,
not a blanket supervisor pass or a claim about unobserved transient changes.

Evidence identifiers (SHA-256):

- Measured G4 seven-file source manifest:
  `5a532077432686527f7631339f8ac54f303b918fc01b05b31718c87727ee5b3b`.
- Functional focal14 receipt:
  `29238dc34266ebbe0a70515515db15c8e484e80dc1741bcec3c316cca16c343f`.
- Selective race3 receipt:
  `8093a4f7f8e3c72b928a3735d02a652b37397ded9baea7be68b02bdee2dfb18e`.
- Preserved failed benchmark supervisor receipt:
  `327c45366d33974a490fd4693c371bbfc4cce9de07a623b9c5d5c4751a84b7d1`.
- Reconciliation proof:
  `e0b6bbad8d3495fcf44d10b5f09139280ceb57621ca2108b498dc091f1e69711`.
