# Defined Sequence and Item streaming

`parser.Writer` calculates the wire lengths needed by a defined SQ/Item and
then emits its headers, children and padding directly to the destination. It
retains a private layout of metadata and borrowed values during that write,
not a complete serialized Item/SQ, a temporary spool or a seek-and-patch output.
The ordinary undefined-length route continues to emit incrementally.

The public constructors, write methods and `LengthPolicy` choices remain.
Undefined length is the default. Preserve retains the SQ length form; a
`core.DataSet` does not retain the original Item length form, so the existing
normalization of Items to their enclosing SQ's chosen form remains. In an
explicit syntax, UN containing a SequenceValue retains its CP-246 exception:
undefined outer value and undefined Implicit VR Little Endian Items.

```go
writer := parser.NewWriterWithOptions(destination, transfer.ExplicitVRLittleEndian,
    parser.WriterOptions{
        LengthPolicy: parser.LengthPolicyDefined,
        SequenceLimits: parser.SequenceWriteLimits{
            MaxDepth:              128,
            MaxPlanNodes:          1_000_000,
            MaxPlanBytes:          64 << 20,
            MaxRetainedValueBytes: 32 << 20,
        },
    })
err := writer.WriteElement(sequence)
```

`object.WriteFileOptions` and `object.WriteDataSetOptions` expose the same
`SequenceLimits` and `BulkDataSize`. `SequenceWriteLimits` is an object alias.
Invalid negative limits are refused before a Part 10 prefix/dataset write.

## Layout and length agreement

Every defined VL is even and less than the undefined sentinel `0xffffffff`.
Explicit VR headers are 8 or 12 bytes using the emitter's normalized VR;
Implicit VR headers are 8 bytes. Short explicit VL must fit uint16. Checked
int64 addition protects lengths/offsets before conversion or allocation, and
typed numeric count × width uses the same checked helper as its encoder.

For each Item, `value = sum(element header + encoded value) + item padding`;
a defined Item emits its 8-byte header plus that value, without a delimiter.
An undefined Item also emits its 8-byte delimiter. SQ value is the sum of
those Item wire lengths, plus an 8-byte Sequence Delimiter only when undefined.
An undefined child inside a defined ancestor therefore has a known *wire*
size; its VL is still the undefined sentinel. Empty datasets/items work with
the same arithmetic and do not receive unnecessary padding.

Charset comes from the enclosing dataset unless an Item declares its own
Specific Character Set. Items keep independent charset state. The current
string encoder measures charset/PN/VM/padding using temporary scratch for one
primitive and encodes it again during emission. Fixed-width numeric values
need no encoded buffer for sizing. Padding shares one calculation between
planning and emission; ItemAlignment excludes Item header/delimiter, while
Part 10 top-level alignment includes its prefix. Planning does not spend the
padding budget; the complete planned addition is admitted before its ancestor
header, then consumed once. Zeros are emitted in chunks of at most 4096 bytes.

Deflate still receives one logical Explicit VR Little Endian dataset stream
before compression, preserves raw Deflate/A.5 trailing NULL behavior, and uses
logical padding domains. It does not require a whole uncompressed SQ buffer.
See [Deflate and padding](DEFLATE_AND_PADDING.md).

Actual destination byte counts are checked against planned Item/element wire
sizes. Positive short writes with no error retain the existing write-all
policy; source/destination errors stop emission. A deferred copier cannot
write beyond its planned value boundary or report a successful count different
from the bytes it actually wrote. No implicit retry of a payload source occurs.

## Finite admission and retention

Each zero field selects its finite default; negative fields are invalid.
There is no unlimited sentinel.

| Limit | Default | Admission |
| --- | --- | --- |
| MaxDepth | 128 | Combined SQ + Item nesting, checked before recursion/path copies; also applies to undefined writing. |
| MaxPlanNodes | 1,000,000 | Prepared element/Item records, including generated GL/padding metadata. |
| MaxPlanBytes | 64 MiB | Requested backing sizes for layout records, paths, pending posts, and conservative policy scratch/map charges before allocation. |
| MaxRetainedValueBytes | 32 MiB | Conservative logical payload retained from hook isolation/transformations or CP-246 BE normalization; borrowed original values are not charged. |

Slices for layout records, paths and pending posts have admitted exact
capacities. Policy scratch is charged before its helpers allocate; map charges
are conservative per entry. The known initial working-value, detached-result and callback-input clones of an active
PreSerialization chain are charged as three conservative payload/structure
requests **before Handle**, starting at the root. The shared charge remains
until that root write finishes and accumulates with descendant clones and
layout records; it is not reset for every child. Structural clone requests
also undergo node/depth checks before a hook or BE normalization.
These are logical planning limits, **not a process RSS ceiling**: caller-owned
datasets, primitive/string encoder scratch, stack, runtime map buckets,
allocator rounding/GC, validation report/operation bookkeeping and external
sources/transformers are not included in an exact heap accounting.

A single large numeric or string primitive can still allocate its current
encoded value buffer. That is separate from eliminated Item/SQ serialization
buffers. Rule validation and its public detached results retain their existing
isolation clones. An active serialization hook receives isolated payload and
can itself allocate an arbitrary substitution. Handle must clone an unknown
replacement to preserve its public detached contract before returning it. The
writer admits any larger returned footprint afterward; that guard cannot
prevent this unknown-size replacement allocation inside Handle. Known initial
clones are admitted beforehand; replacements can also cause further callback
input copies inside Handle before the writer sees their footprint; neither this boundary nor allocator/GC overhead
is presented as an exact cap on transient peak heap/RSS. All clone charges are
conservative and kept through the ancestor operation rather than reused after
a child returns. Rule isolation is a separate existing validation cost; even with no custom
rules/hooks, `ValidateElement` clones and recursively validates SQ subtrees.
Repeated ancestor/descendant validation remains outside these layout quotas.

CP-246 UN under Big Endian retains the existing normalization of raw numeric
subtrees into Little Endian. Payload **and structural clone requests** share
the root quota with hook isolation and layout records. The UN route reserves
all of its planned Item normalizations before its outer header; a UN child in
a defined layout also reserves both before copying. Charges remain through
the ancestor operation, conservatively including sequential Items. It can
retain a normalized Item subtree. It is explicitly an exception to the ordinary
borrowed-payload path; this change does not introduce a new transcode primitive.

## Metadata-only deferred and Bulk Data sizing

Sizing never opens a Bulk resolver or calls a deferred payload copier.

- A deferred defined primitive with a copier uses its declared Header.Length,
  including any pad the copier must supply. The selected copier is captured
  for that layout and invoked once during emission.
- A deferred undefined encapsulated Pixel Data value requiring a size uses
  `WriterOptions.DeferredValueLength(item, element)`, returning its actual
  encoded value bytes, including BOT/Fragment Item headers and delimiter.
- A BulkDataValue with a defined header uses that encoded length. Without one,
  `WriterOptions.BulkDataSize(reference)` returns the source's *unpadded* size.
  Its callback is metadata-only: it must not open/read/close or consume a source.
- Missing metadata needed by a defined ancestor or Group Length yields
  `parser.ErrEncodedLengthUnavailable` before that ancestor's header. This is
  an intentional refusal of inputs previously measured by buffering/replay.
  The writer does not fall back to a whole-value buffer or count-only read.
- Callback errors retain `errors.Is` through a diagnostic wrapper that does
  not interpolate URI contents. Callbacks must be pure and stable during the
  write; Group Length can request metadata again without reading payload.

At emission, BulkDataResolver acquires one owned Reader, verifies its Size
against the prepared padded length, copies/checks EOF, and closes it once,
including failure paths. No collection of open readers is retained by the
plan. Existing Bulk resolver extra-byte/close/short-source behavior remains.
Borrowed deferred sources are not closed by sizing or writing.

Object readers use recorded ValueLocations, root vs Item offset and tag, for
an unambiguous encoded length without Read/Seek/reparse. General SQ replay
and ambiguous locations remain unsupported. ValueLocations returns its existing detached metadata index list under the
reader's source limits; that provider/index scratch is separate from the
writer's layout quota. Original references, physical source offsets,
charset/byte-order replay, bindings and invalidation contracts
are unchanged. A source and its parsed backing must remain open and immutable
through the write; a writer/provider is not concurrently reusable. There is
no protection against arbitrary in-place modification during a call.

WriteFileFrames uses already collected FrameLength values and the existing
encapsulation layout's Fragment count. Encapsulated value size is
`16 + len(BOT) + 8*fragments + sum(frameLength + frameLength%2)`; native size is
the padded total. GL calculation therefore does not pull frames a second time,
including Basic/empty/Extended tables, split fragments and terminal-fragment
adjustments. The frame provider delegates other deferred metadata to its base.
A custom provider lacking required metadata is refused instead of replayed.

## Validation callbacks and failure phases

For a defined ancestor, PreSerialization follows the existing traversal/order
once per visited element at its predicted logical offset. Transformations and
filters are frozen in the layout; emission does not call the hook again. Paths
retain Item indexes and transformed tags. PostWrite records contain only
headers/paths/offsets/actual counts and are withheld until their enclosing
defined ancestor is fully emitted, in the existing child-before-parent order.
A late destination/source failure suppresses its uncommitted child posts;
the enclosing failure retains actual committed BytesWritten and Complete=false.
A post hook error never undoes already emitted bytes.

When the frozen operation has no PreSerialization consumer (nil/empty chain
or hooks registered only at other points), a private fast path calls Handle
with Header/Path/Offset only. It retains context/report location handling
without cloning a payload that no hook uses. `Operation.HasHooks(point)` reads
the frozen registration snapshot; late registration does not alter it. Public
HookChain.Run/Handle results remain detached; rule validation is unchanged.

The phase of a source error necessarily changes: complete preparation now
precedes the first source read. Later children's PreSerialization callbacks
may already have run when an earlier deferred source fails during emission.
Callbacks can have side effects and are not rolled back. Metadata/header/limit
or preparation errors still occur before the dependent ancestor's bytes, but
no rollback is promised for earlier top-level bytes or File Meta.

Create/Recalculate Group Length is first computed from original encoding
metadata, then checked against the effective prepared/filtered bytes. An
inconsistent emitted GL in a prepared defined subtree is refused with
`ErrGroupLengthAfterHook` before its ancestor header. The writer does not
silently overwrite a hook's GL value, call it twice, or promise compatibility
with formerly stale GL output. Same-size changes/observers are accepted, and
hooks can provide a correct GL explicitly. Preserve retains received GL and
therefore retains its explicit risk of stale values after transformation or
padding changes. Top-level/manual GL helper calls do not constitute a new
transactional dataset transformation API.

## Qualification and measured scope

A reduced-quota observer regression demonstrated the initial accounting gap:
4 payload bytes were wrongly accepted with quota7, and root/descendant clones
were wrongly accepted with quota11. The initial two-copy remediation (generation2) made quota7 refuse before the
root hook, quota11 before the descendant, and quota16 accept that provisional
two-root/two-leaf charge. These are historical intermediate results. Those RED and PASS
logs and the prior source generation are retained in local evidence. An independent review then found invokeHook
creates a third known clone: primitive Raw4/quota8 wrongly invoked its
observer and emitted16 bytes. Three-copy admission now refuses before that
callback; root/descendant observer controls require conservative quotas11
(root refusal),17 (descendant refusal) and24 (acceptance). A further
review found that CP-246 normalization had only a per-Item admission without
charging that shared root quota. Payload and structure controls with an active
observer initially emitted20 bytes before refusal; after the fix both refuse
before normalization/header output. That RED generation is also preserved.

The candidate is built in an isolated archive of main
`6dea777b047a65508d5fe7a2c8627ef01a58f515`, with only the 21 approved paths.
Correctness fixtures cover LE/BE/Implicit VR, charset inheritance/local state,
empty/nested Items, bulk/deferred single-pass ownership, frames with GL,
Deflate/padding, limits/overflow, hooks/offsets/filters, cancellation, positive
short writes and source/destination failures. No codec, live PACS, clinical or
native Windows qualification is claimed.

The synthetic benchmark/memory harness writes to a SHA-256 counter without
retaining output. It fixes leaf size at 64 KiB while varying total payload and
depth, has a file-backed deferred case, and separates GL/padding, validated
nil/active hooks and a CP-246 BE/OW control that actually normalizes raw
payload, rather than only copying OB structure. The standalone memory probe is opt-in
through `DICOM_SEQUENCE_MEMORY_CASE`, excluded from ordinary tests. Input
construction is outside serializer time/allocation cutpoints; absolute RSS
still includes fixture/runtime costs. Sampled live heap is not an exact peak,
and RSS minus a baseline is indicative, not exact additional payload retention.


Both binaries use the same test-only harness; baseline product is the exact
base archive, candidate product is frozen generation7. The full correctness
gates use product-identical generation6; generation7 only adjusts the OW
control and documentary byte count. Final changes after the campaign are
documentation only. Measured source/binary/runner hashes, paired order, raw
samples and every equivalence result are retained in the local evidence.

These are median total writer times and cumulative allocations. Planning and
emission are both included; sizing alone was not separately timed. All 11
paired median times improved in this run. Short local samples are descriptive
evidence and do not establish confidence intervals or universal throughput.

| Synthetic case | Baseline → candidate ms/op | Baseline → candidate MiB allocated/op |
| --- | --- | --- |
| Raw1MiBDepth1 | 0.51 → 0.32 | 3.189 → 0.006 |
| Raw8MiBDepth1 | 3.29 → 2.53 | 25.941 → 0.036 |
| Raw32MiBDepth1 | 12.76 → 10.08 | 103.950 → 0.130 |
| Raw64MiBDepth1 | 25.46 → 20.12 | 207.970 → 0.252 |
| Raw1MiBDepth16 | 2.61 → 0.32 | 33.432 → 0.026 |
| Raw1MiBDepth64 | 9.36 → 0.34 | 130.210 → 0.089 |
| Deferred64MiBDepth1 | 29.20 → 26.32 | 224.109 → 32.346 |
| GroupPadding8MiBDepth4 | 13.76 → 2.65 | 319.882 → 0.499 |
| ValidationNil8MiBDepth4 | 26.29 → 22.35 | 555.080 → 441.044 |
| ValidationActive1MiBDepth4 | 5.93 → 3.85 | 109.915 → 70.290 |
| UNBigEndian8MiBDepth4 | 7.83 → 5.74 | 65.982 → 8.047 |

The fresh-process memory medians below separate sampled additional live heap
from absolute process RSS. MiB is 2^20 bytes. The RSS includes fixture/runtime
and is measured by `/usr/bin/time -l`. The offline campaign retained all raw
samples and minimum–maximum ranges; these tables summarize medians. Heap
sampling at 1 ms plus the final snapshot can miss a true transient peak.

| Synthetic case | Baseline → candidate extra heap MiB | Baseline → candidate absolute peak RSS MiB |
| --- | --- | --- |
| Raw1MiBDepth1 | 3.217 → 0.036 | 25.05 → 21.89 |
| Raw8MiBDepth1 | 21.542 → 0.065 | 55.08 → 29.09 |
| Raw32MiBDepth1 | 86.051 → 0.161 | 157.31 → 53.20 |
| Raw64MiBDepth1 | 190.065 → 0.282 | 293.75 → 85.56 |
| Raw1MiBDepth16 | 9.110 → 0.055 | 32.69 → 22.52 |
| Raw1MiBDepth64 | 9.126 → 0.118 | 33.05 → 22.64 |
| Deferred64MiBDepth1 | 184.086 → 6.080 | 245.56 → 27.11 |
| GroupPadding8MiBDepth4 | 55.525 → 0.529 | 93.34 → 29.62 |
| ValidationNil8MiBDepth4 | 122.276 → 63.554 | 160.44 → 95.03 |
| ValidationActive1MiBDepth4 | 22.409 → 20.789 | 47.52 → 44.39 |
| UNBigEndian8MiBDepth4 | 47.823 → 8.077 | 87.12 → 36.81 |

The ordinary resident 1–64 MiB series now adds 0.036–0.282 MiB sampled live
heap while reusing the original payload. It still allocates metadata for each
64 KiB leaf, so neither record storage nor B/op is constant as leaf count
grows. The depth64 control removes the repeated complete ancestor wire
buffers; it does not bound arbitrary input, hooks or validation memory.

`Deferred64MiBDepth1` uses a sparse local file and one `io.Copy` per leaf. Its
cumulative candidate allocation remains 32.346 MiB/op from `io.Copy` buffers in the
provider/harness and writer metadata while sampled additional live heap is
6.080 MiB. Allocation count
**regresses from 7203 to 9231 per operation (+28.2%)** despite lower total
allocation/retention/time. This is local cached/sparse-file evidence, not
cold-disk throughput. The byte-boundary and deferred-copy machinery still has
a per-leaf cost.

**Validated writing still has substantial payload-dependent validation cost.**
The nil-hook fast path removes only unused PreSerialization hook isolation.
`ValidationWriter` still calls `ValidateElement`; `ValidateDataSet` clones
its input and recursively checks SQ descendants, and descendant preparation
repeats those checks for sequence ancestors. Existing default validation/rule
isolation and report work are outside the writer layout quotas. Consequently
the 8 MiB nil-hook control still allocates **441.044 MiB/op** and has **63.554
MiB** sampled extra heap; active1MiB/depth4 retains **70.290 MiB/op** cumulative
allocation and **20.789 MiB** sampled extra heap. Neither validated path has
payload-constant retention, and `MaxRetainedValueBytes=32 MiB` is not their
heap/RSS cap. Eliminating those separate validation copies is residual work.

The UN BE/OW control includes its deliberate 8 MiB numeric normalization copy;
it is the documented CP-246 exception. The benchmark destination hashes all
bytes without retaining output, and both sides include the same hash and
external RSS-monitor costs. Parsing/builders, arbitrary hook substitutions,
cold disk, PACS/network, codecs and GUI/GPU are not measured. The offline
campaign retained raw ranges, provenance and independent review; none of
those measurements constitutes a runtime memory guarantee.

- [PS3.5 7.1: Element headers](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/chapter_7.html)
- [PS3.5 7.2: Group Length consistency](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_7.2.html)
- [PS3.5 7.5: SQ/Item length forms](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_7.5.html)
- [PS3.5 7.5.3: Item charset inheritance](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_7.5.3.html)
- [PS3.5 6.2.2: UN byte order](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.2.html)
- [PS3.5 A.5: Deflate](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.5.html)

Standards, current APIs and synthetic fixtures determine this implementation.
Local dcm4che was read only as a clean-room strategy/behavior reference; its
code, mutable length caches and implementation were not copied.
