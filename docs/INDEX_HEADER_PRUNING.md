# Header pruning and deliberate File prefixes

The library offers explicit faster metadata reads without changing the default
index traversal or complete File reader. All modes read headers before deciding
whether to consume values. These APIs extract metadata; they do not prove an
IOD complete, validate an unread suffix, or qualify behavior against a PACS.

## Index options

```go
options := index.DefaultOptions()
options.AssumeSortedTags = true
options.SequencePruning = index.SequencePruningOpaque
result, err := index.ReadPath(ctx, path, options)
```

`AssumeSortedTags` is an opt-in caller guarantee of increasing **root** tags.
The maximum includes the chosen builtin profile, root Specific Character Set,
root primitive selectors, and the first sequence ancestor of every nested
selector. The first root header strictly greater than this maximum stops the
read before its value. Item tag ordering does not change the root maximum.
The default remains compatible with out-of-order root input and last-wins
metadata. Faster mode cannot verify ordering in a suffix it never reads.

A DICOMDIR selector under Directory Record Sequence `(0004,1220)` requires that
ancestor, while a Per-Frame Functional Groups selector requires `(5200,9230)`.
The leaf tag alone is insufficient. An empty selector path denotes a root
primitive, not a wildcard sequence. Occurrence `First`, `Last` and `All`, callback
order, selected Item paths and original offsets remain unchanged. A demanded
Directory Record Sequence is traversed; an undemanded nested icon may be pruned.

`SequencePruningStrict` (zero) retains structural token traversal. It already
skips unselected primitive payloads, so this is not whole-value or whole-IOD
validation. `SequencePruningOpaque` skips a sequence only if no requested
selector ancestry can reach its descendants. These policies are independent of
the sorted-input option.

| Input body | Opaque behavior | Validation limit |
| --- | --- | --- |
| Defined-length SQ | Check complete source/parent/total extent, then seek or discard the admitted body | Descendants, internal order, private reservations, hidden depth and delimiters are unexamined |
| Undefined-length SQ/UN | Visit bounded headers and Item/Sequence framing; skip defined child bodies | Undefined framing is checked; defined child internals remain opaque |
| Visited encapsulated Pixel Data | Check BOT/fragments, lengths, cumulative pixel bytes and delimiters; discard payload | No codec decoding or pixel validation |

Malformed bytes hidden in an admitted defined body may be accepted by opaque
mode and rejected by structural traversal. This is the chosen validation
boundary, not equivalent malformed-input checking. Delimiter-shaped payload
bytes are never searched as control tags. Undefined-length UN retains CP246
implicit VR Little Endian scope even under an Explicit VR Big Endian parent.
Visited private creators remain bounded and Item-local; skipped reservations
cannot leak into following siblings or the root dataset.

All index modes keep the existing root integer, Float and Double Float Pixel
Data barrier. It sets `StoppedBeforePixelData`. An earlier non-Pixel maximum
stop sets `StoppedAfterRequestedTags`. `StoppedHeaderSet` and `StoppedHeader`
describe the value-free stopping header. These flags describe selected-metadata
completion only; an index `Result` is not an `object.File` for rewriting.

## Low-level parser contracts

`SelectiveSkip` keeps its existing primitive and encapsulated-pixel semantics.
The separate `SelectiveSkipSequence` disposition accepts SQ or undefined-length
UN. It emits a header-only element with `core.DiscardedValue`, which a writer
refuses; it does not turn a skipped SQ into a writable empty SQ.

`SelectiveReaderOptions.MaxTokens` counts decoded headers/control and synthetic
closing events, including framing visited inside pruning. Zero preserves the
existing unlimited parser-local budget; negative values are invalid. The
opaque index supplies its finite `Limits.MaxTokens`. Unvisited descendants in
a defined body are not counted. Unknown dispositions and pruning a primitive
through the sequence disposition are explicit errors.

`Reader.ReadDataSetPrefix` is a separate collector for a selective reader. It
returns completed elements and a `parser.ReadStop` descriptor; a root stop has
`Stopped=true`, while clean EOF has `Stopped=false`. Nested stops are errors,
and validation/sink readers are refused. Ordinary `ReadDataSet` and
`ReadDataSetPartial` continue to reject stop tokens rather than reporting a
complete Data Set.

## Object File prefixes

```go
threshold := core.NewTag(0x7FE0, 0x0010)
file, err := object.OpenFileWithOptions(path, object.ReadFileOptions{
    StopBeforeTag: &threshold,
})
if errors.Is(err, object.ErrReadStopped) {
    stop, _ := file.ReadStop()
    _ = stop // header and logical offset; the stopped value is absent
}
if file != nil {
    defer file.Close()
}
```

The threshold is copied before input or callbacks. This File option stops at
the first **root** header greater than or equal to the threshold (`>=`); the
index maximum retains requested values and uses `>`. Whole File Meta is read
first. Nested high tags do not stop their parent. Every completed pre-cut
sequence and Item remains intact, using ordinary collectors; this route does
not perform opaque pruning. The threshold is a physical-prefix boundary on
unsorted native input, not a promise that every lower tag was found.

An actual stop returns both a `File` and `*ReadStopError`, matching
`ErrReadStopped` and `ErrPartialRead`. `File.PartialRead()` is true, and
`File.ReadStop()` returns a detached value-free descriptor. The returned public
error can be inspected or modified without changing the File's retained stop
metadata. `PartialRead.Err` matches the stop sentinel; use `File.ReadStop()` for
its descriptor. Clean EOF before any qualifying header returns a complete File
and nil error. A malformed value/header/container before the boundary returns
the ordinary parse error and no File.

`ValidateFile` refuses a deliberate prefix with `ErrPartialFile`; the focused
`ValidatePart10Identity` still only checks identity pairs. Ordinary write,
frame-write, decompress and icon APIs already refuse a partial File before
output or decode. The existing `WriteFileOptions.AllowPartial` remains an
explicit truncated-output override; it does not establish complete input.

File prefix reading requires seekable Part 10 input. Non-seekable sources,
`AllowPartialRead`, native/encapsulated sinks, raw dataset and raw-validation
entrypoints, transfer-syntax recovery and recovery ladders reject prefix options
before input/callbacks. Recovery callbacks also cannot introduce prefix options
into a complete-read attempt. Invalid `PrefixMaxTokens` or negative prefix
limits fail before reads. `ReadPart10Header` keeps its header-only contract and
does not apply a main-dataset threshold.

Native seekable input consumes the stopped header only and remains at the
value start. With Deflate, the stop and offsets are **logical**; the inflater may
read ahead from compressed input. A descriptor is not a physical compressed
value offset. SectionReader offsets remain relative to that section. Borrowed
sources stay caller-owned. Path-backed sources are retained only for pre-cut
deferred values, and `File.Close` releases them; those values remain replayable
with their existing source and Item offsets. Deflated input keeps the existing
deferred-value refusal.

Index/selective-parser contexts are checked before header work, seeks and
discard chunks. File reading retains its existing API without a Context field;
a caller-supplied context-checking seekable source can propagate cancellation.
An arbitrary blocking source `Read` or `Seek` remains non-interruptible.

## Finite limits and measurements

| Resource | Default index | Default opt-in File prefix |
| --- | --- | --- |
| Logical total extent | 512 MiB | 512 MiB |
| Selected/allocated primitive value | 1 MiB selected value | 1 MiB allocation ceiling |
| Primitive elements | 200,000 | 200,000 |
| Active combined SQ/Item frames | 64 | 128 |
| Encapsulated fragments excluding BOT | 200,000 | 200,000 |
| Dataset token work | 1,000,000; opaque also includes hidden visited framing | 1,000,000 via `PrefixMaxTokens` |
| Selected occurrences / index diagnostics | 4,096 / 256 | Existing object private/recovery caps |

Prefix zero limits receive these finite defaults; configured positive limits
override them. `PrefixMaxTokens` applies exclusively to the selective **dataset**
pass. File Meta is read completely first by its existing readers under their
separate byte, primitive-element and depth limits; its headers, Items and
control tokens do not consume `PrefixMaxTokens`. Complete nil-threshold reads
retain their historical zero-limit
behavior. Negative unlimited sequence depth is refused only in the new File
prefix route. Sequence aggregate lengths do not consume a primitive allocation
allowance. Total bytes, parent/source fences, visited tokens, depth and existing
private/fragment limits constrain admitted work and individual allocations;
they are not a global heap/RSS limit. At an exact total-byte boundary the
existing hard-limit reader may conservatively report exhaustion rather than
read one extra byte to distinguish EOF.

`Result.BytesScanned` measures logical parser position, including seek-skipped
values and inflated bytes. Physical `Read` bytes/calls and `Seek` calls require
separate instrumentation. A large CSA private OB is already cheaply seeked;
its size alone does not prove a new speedup. The benchmark harness contains
Enhanced selected/unselected groups, actual CSA-tag OB controls, demanded
DICOMDIR records with undemanded icons, and a distinctly named private-SQ tail.
It also compiles against the archived baseline and uses identical bytes,
selectors, profiles and limits. The qualified campaign below used separately
reserved quiet windows. It does not establish GDCM parity, a disk-I/O or RSS
reduction, or external-PACS behavior.

The implementation uses the library's existing parser primitives. The primary
rules are [PS3.5 chapter 7](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/chapter_7.html),
[7.5](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_7.5.html),
[7.5.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_7.5.2.html)
and [6.2.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.2.html).
Only public GDCM Reader/Scanner headers informed behavior; no foreign
implementation was translated.

## Qualified synthetic campaign


Each separately authorized G4, G5 and G6 window ran two untimed correctness
and Read/Seek preflights, then five fixtures × four modes × three paired samples
× two binaries: 120 timing calls of `-benchtime=10x -count=1`, alternating A/B
order. The preflights checked independent identities/counts, identical selected
record/fixture hashes, paths/offsets/order and unchanged source bytes for all
20 cases/modes per binary. No sample, outlier or prior generation was discarded,
retried, pooled or selected for a favorable result.

A shared 120-second deadline, 30-second process limit and 1-GiB process-group
RSS guard sampled every 20 ms cover preflight and timing. Every child starts a
private process group; every call is waited/reaped and its group verified empty
before the next. The RSS sampler can miss short peaks and is an operational
guard, not a kernel-enforced allocation ceiling or a comparative memory study.
All 122 cycles passed in each window. Before/after hashes matched for 8,435
covered blobs in G4, 8,592 in G5 and 8,766 in G6, including source, exact baseline
binaries, harness, guard and retained prior campaign artifacts.

### Final G6 results

G6 removes an extra aggregate-return wrapper per selective token and avoids
unused accounting calls when the parser token budget is zero. Its iterative
loop, all cancellation polls, positive budgets and opaque framing remain.
Disassembly verified the removed call boundary; no percentage of timing was
attributed to that static call graph. The final runtime passed complete
parser/index/object tests with 2,571 passing test events and eight existing
opt-in external-oracle skips on both native Go and Go 1.22.12; 76 focused events
also passed with the race detector, plus vet. Counts include parent/subtest
events, not only distinct top-level test functions.

Positive paired reduction means faster; negative means slower. The median
paired reduction is calculated from the three A/B pairs, so it need not equal
the ratio of the two separate time medians. `B/op` and `allocs/op` are benchmark
allocation counters, not retained memory or RSS. All ranges below retain every
sample.

| Fixture / mode | Baseline median ms | G6 median ms | Paired reduction median [range] % | B/op baseline → G6 | allocs/op baseline → G6 |
| --- | ---: | ---: | ---: | ---: | ---: |
| EnhancedUnselected / Legacy | 10.593896 | 10.867946 | -2.59 [-7.88, 1.46] | 3804979 → 3805032 | 352439 → 352439 |
| EnhancedUnselected / Sorted | 10.565388 | 0.010712 | 99.90 [99.90, 99.92] | 3804260 → 9671 | 352438 → 168 |
| EnhancedUnselected / Opaque | 10.559550 | 0.011412 | 99.89 [99.89, 99.89] | 3804320 → 9876 | 352439 → 175 |
| EnhancedUnselected / Both | 10.546479 | 0.010062 | 99.90 [99.90, 99.92] | 3804517 → 9696 | 352439 → 168 |
| EnhancedSelected / Legacy | 11.742400 | 11.743217 | -1.41 [-3.96, 0.78] | 4826061 → 4826000 | 389326 → 389325 |
| EnhancedSelected / Sorted | 11.948779 | 11.780471 | 0.75 [0.17, 1.41] | 4826334 → 4826044 | 389326 → 389326 |
| EnhancedSelected / Opaque | 11.666638 | 1.789462 | 84.68 [84.44, 84.80] | 4826469 → 1137415 | 389325 → 55496 |
| EnhancedSelected / Both | 11.934317 | 1.770917 | 85.16 [85.16, 85.59] | 4826019 → 1137421 | 389326 → 55496 |
| CSAPrimitive / Legacy | 0.010992 | 0.010842 | -3.37 [-5.50, 29.42] | 9836 → 9889 | 180 → 181 |
| CSAPrimitive / Sorted | 0.009958 | 0.011183 | -16.49 [-23.27, 20.31] | 9868 → 9710 | 180 → 168 |
| CSAPrimitive / Opaque | 0.010475 | 0.010704 | -5.57 [-11.05, -2.19] | 9861 → 9879 | 180 → 180 |
| CSAPrimitive / Both | 0.010183 | 0.010504 | 10.07 [-18.72, 17.43] | 9863 → 9702 | 180 → 168 |
| DICOMDIRIcons / Legacy | 4.344029 | 4.348200 | -0.55 [-1.58, 1.33] | 2700749 → 2700800 | 145551 → 145552 |
| DICOMDIRIcons / Sorted | 4.377588 | 4.369379 | 0.19 [-2.23, 3.31] | 2700788 → 2700764 | 145552 → 145551 |
| DICOMDIRIcons / Opaque | 4.380012 | 1.569617 | 64.14 [63.34, 65.32] | 2700786 → 1758016 | 145552 → 55437 |
| DICOMDIRIcons / Both | 4.427650 | 1.565129 | 64.07 [20.25, 64.78] | 2700792 → 1758022 | 145552 → 55437 |
| PrivateSQTail / Legacy | 10.719408 | 10.778192 | -0.33 [-0.97, -0.05] | 3804503 → 3804490 | 352439 → 352438 |
| PrivateSQTail / Sorted | 10.620112 | 0.009329 | 99.91 [99.91, 99.91] | 3804476 → 9693 | 352439 → 168 |
| PrivateSQTail / Opaque | 10.650683 | 0.009204 | 99.91 [99.89, 99.92] | 3804485 → 9870 | 352439 → 174 |
| PrivateSQTail / Both | 10.712121 | 0.010929 | 99.90 [99.89, 99.90] | 3804298 → 9672 | 352438 → 168 |

Opaque/Both materially reduced the selected Enhanced and demanded DICOMDIR
workloads; the latter still traverses every demanded record. The roughly
99.9% reductions concern tails with no demanded descendants, and are not a
prediction for arbitrary clinical files. Sorted alone cannot avoid demanded
high-tag sequences and has essentially unchanged cost in those two fixtures.
The cheap CSA OB control already seeks over the blobs: it shows no consistent
material advantage. The default still has measured residual slowdowns of
0.33–2.59% median in the four large SQ fixtures. This is not zero regression or
a speedup of the default path; three of those paired ranges cross zero, while
PrivateSQTail's range remains negative.

G6's shared preflight/timing wall time was 14.178 s, of which 13.822 s covered
timing calls and guard/drain overhead. The guard's maximum sampled group RSS
was 106,954,752 bytes. These process-level observations are separate from
`ns/op` and provide no global-heap or RSS-reduction claim. The retained DICOMDIR
Both outlier yields a paired range of 20.25–64.78%; CSA timing ranges are broad
at approximately 10 microseconds per operation.

### Retained earlier generations and physical-interface work

G4/G5 had a wrapper and accounting helper calls on the unlimited legacy path.
G5 only removed nil-error matching and a redundant zero-budget context check;
its cancellation counterexample was retained and corrected with the required
post-header poll. Static examination then justified G6's removal of the extra
wrapper and unused helper calls. All generations remain separate observations;
no quantitative causal attribution or significance test is claimed.

| Default fixture | G4 paired reduction median [range] % | G5 paired reduction median [range] % | G6 paired reduction median [range] % |
| --- | ---: | ---: | ---: |
| EnhancedUnselected | -7.53 [-8.17, -6.69] | -7.42 [-11.00, -7.00] | -2.59 [-7.88, 1.46] |
| EnhancedSelected | -7.87 [-55.48, -7.39] | -6.24 [-6.42, -5.91] | -1.41 [-3.96, 0.78] |
| DICOMDIRIcons | -8.32 [-9.31, -7.06] | -5.64 [-8.28, -5.31] | -0.55 [-1.58, 1.33] |
| PrivateSQTail | -8.08 [-9.06, -7.74] | -5.46 [-5.72, -5.11] | -0.33 [-0.97, -0.05] |

G4 Opaque/Both reductions were 83.75/84.04% for EnhancedSelected and
62.29/61.77% for DICOMDIRIcons; G5 was 84.59/83.78% and 62.53/61.94%,
respectively. Earlier outliers remain: G4 EnhancedSelected Legacy reached
−55.48% paired reduction; G5 CSA Legacy reached −73.65%; G5 DICOMDIR Sorted
had a −21.66..+3.46% paired range with elevated times in both binaries. G4/G5
CSA medians also mixed gains and losses, and support no general CSA advantage.

The untimed adapters count calls to an in-memory source, not disk transactions.
Their 20 rows per binary are identical across G4/G5/G6. For EnhancedSelected
Opaque, source Read bytes fall from 651,624 to 110,952, Read calls from 311,362
to 43,074 and Seek calls from 262,151 to 8,199; logical bytes remain 782,696.
For DICOMDIR Opaque, Read bytes fall from 266,288 to 110,640, Read calls from
110,623 to 36,895 and Seek calls from 65,543 to 8,199; logical bytes remain
8,683,568. CSA Legacy/Opaque both read 372 bytes using 71 Read calls and
15 Seek calls, despite a 16,777,588-byte logical extent. These counters explain
the already-cheap primitive control and distinguish parser position from
source-interface work; none measures physical compressed offsets or disk I/O.
