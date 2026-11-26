# Incremental derived image and SEG writing

The additive APIs keep the existing in-memory builders: imagebuild.Build and
seg.Write continue to produce owned objects. The incremental APIs serialize
through object.WriteFileFrames, keep a bounded pixel window, and leave paths,
jobs and publication to the application. Fixtures and measurements below are
original synthetic inputs; they provide no clinical or live-peer qualification.

## Classic CT, MR and Secondary Capture series

imagebuild.SliceSource reports a fixed count and returns one borrowed Slice.
Its values and references remain valid until the next Slice call. Calls are
serial; the count, metadata and values must remain unchanged until WriteSeries
returns. A source must unblock when its context is cancelled.

WriteSeries requires positive MaxSlices and MaxPixelBufferBytes. The pixel
budget covers one borrowed float64 plane plus one owned native16 plane:
10 bytes per pixel, independent of slice count. Caller-retained input storage
and inherited/derivation metadata are separate; callers must bound metadata.
The synchronous emit callback calls its writer exactly once and cannot retain
it. Ignoring a serialization error or calling twice fails the series.

Replayable explicitly authorizes two passes: the first finds the global range
and representation, and the second emits every instance with one coherent
rescale. Alternatively, provide Options.Rescale and StoredRepresentation (0
unsigned, 1 signed) for one pass. Every quantized value must fit; the streaming
path refuses clipping and nonfinite samples. Cancellation is checked while
producing values and serializing chunks. Options.SOPInstanceUIDs, series/FoR
identities, ContentTime and Common.Now allow fixed fixture metadata. Empty SOP
identities retain generated UIDs, as in Build.

Earlier emitted instances may be complete even if a later read/write fails.
SeriesWriteResult.Instances counts successful callbacks, not published files.
Stage the entire series for all-or-nothing visibility.

## BINARY, FRACTIONAL and LABELMAP SEG

seg.FrameSource is serial and replayable; Document.Frames supplies bounded
geometry/reference metadata separately from the borrowed FramePayload. The
payload remains valid only until the next Frame call. DocumentFrames adapts an
existing immutable in-memory Document without cloning masks/sample arrays.

WriteFrames requires positive limits for frame count, source reads, pixel
buffers and a conservative metadata reservation. Its pixel reservation is one
encoded frame, two packed presence planes and 32 KiB for bit concatenation.
The caller's payload storage is excluded. Functional groups, reference indexes
and header structures still grow with frame count; their separate reservation
and frame cap prevent unbounded metadata. The reservation is a conservative
admission profile, not an exact live-heap measurement.

BINARY uses PackedBitFrameSource: each source frame starts at bit zero, LSB
first, has ceil(frame bits/8) bytes and zero unused high bits. The native writer
joins frames continuously, including 35-pixel frames, and pads only the final
Pixel Data value to even length. Encapsulated output refuses this source type.
FRACTIONAL preserves uint8 values and Maximum Fractional Value; LABELMAP uses
native 8 or 16 bits according to the described labels.

Validation precedes serialization and is repeated when fetching emitted frames.
Exact Segments Overlap uses two presence buffers and replays pairs at the same
position in source order. This can be quadratic within a shared position:
MaxFrameReads bounds the work and fails explicitly when exhausted. It does not
retain all masks. Even a source valid during preflight can fail later; Frames is
zero unless the whole object was successfully serialized. A writer may already
contain partial bytes on error, so it must target private staging.

## Reproducible measurements

From this module, the allocation/time benchmarks are:

```sh
GOMAXPROCS=4 go test ./imagebuild ./seg -run '^$' -bench '^BenchmarkIncremental' -benchtime=1x -count=1
```

On Windows, run the fresh-process peak campaign:

```powershell
./scripts/measure-incremental-exports.ps1
```

The script compiles disposable test executables and launches one process per
profile/count/mode, without visible windows. Input allocation precedes timing
and MemStats baselines. Both modes retain the same caller-owned input until
measurement ends; no source memory is attributed to the writer. Heap is sampled
every millisecond; the parent observes Windows PeakWorkingSet64 every 10 ms.
These are observed peaks, including process/runtime/GC effects, not an exact
pixel-buffer measurement or a statistical throughput claim. Outputs use
io.Discard; disk durability, archive import and GUI overhead are excluded.


| Profile/count | Caller pixels MiB | Allocated MiB whole/stream | Observed working set MiB whole/stream | Time ms whole/stream |
| --- | ---: | ---: | ---: | ---: |
| CT / 32 | 64 | 17.4 / 2.4 | 106.3 / 92.3 | 77.0 / 101.6 |
| CT / 128 | 256 | 69.6 / 8.1 | 352.2 / 290.0 | 306.7 / 403.4 |
| CT / 512 | 1024 | 278.3 / 30.8 | 1330.5 / 1083.3 | 1235.2 / 1607.6 |
| FRACTIONAL SEG / 32 | 8 | 18.8 / 0.7 | 53.2 / 24.2 | 16.6 / 6.2 |
| FRACTIONAL SEG / 128 | 32 | 75.1 / 1.6 | 133.4 / 57.8 | 63.1 / 24.0 |
| FRACTIONAL SEG / 512 | 128 | 300.0 / 5.2 | 454.5 / 160.0 | 252.6 / 89.6 |

The classic-series two-pass path reduced allocated/output pixel storage at a
time cost in this run; it is not advertised as faster. The SEG run used one
segment per position, so it does not benchmark worst-case overlap replay.
Full-byte parity fixtures cover CT/MR/SC and BINARY/FRACTIONAL/LABELMAP8/16 with
fixed identities/times, oblique geometry and enhanced-frame references.
