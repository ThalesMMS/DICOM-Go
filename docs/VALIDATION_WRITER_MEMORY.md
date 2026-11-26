# Validated writer copy admission

`parser.NewWriterWithValidation` uses `validation.NewWriteOperation`. The public
`ValidateDataSet`, `Operation.Handle` and `HookChain.Run` still return detached
known values, including when the public hook chain is nil or has no matching
point. The writer's private path only consumes reports and discards unobserved
hook results, so it can borrow immutable value backing without constructing an
unused detached dataset. Ordinary writers do not execute validation hooks.

The built-in report-only profile requires no VR resolver, no dataset rules and
known `core.Value` types. It reuses the existing validation checks, traversal,
findings and lifecycle policy. Dictionary calls still occur at every original
visit; custom dictionaries are not assumed pure. Ancestor and descendant rule
passes remain where findings/order/multiplicity require them. This removes
payload copies and clone traversals; it does not promise one validation pass.

Resolvers, dataset rules and opaque values use an owned working fallback for
known backing. Each dataset rule and active hook receives its own isolated
known value copy, as before. An opaque value retains the existing public clone
boundary: the library cannot copy its private backing or bound methods called
by caller code. Estimation never calls its `EncodedLength`, opens a bulk source
or replays a deferred provider. Immutable strings share their content while
owning their slice headers. Known slice copies request exact length/capacity;
Raw nil/empty and other typed empty normalization remain compatible.

## Limits and ownership

Set `validation.Options.WriteLimits` to change writer-specific admission.
Zero fields select finite defaults; negative fields are invalid:

| Limit | Default | What is admitted |
| --- | ---: | --- |
| `MaxCopiedValueBytes` | 64 MiB | Cumulative known Raw, numeric and fragment backing requested by library clones in one write, including fallback, rule inputs, working/detached hook results and each callback input. |
| `MaxWorkBytes` | 64 MiB | Known structural backing, scope/ordinal metadata, paths, report/configuration scratch, conservative map-entry charges and text decode/split/charset scratch. |
| `MaxScopeNodes` | 1,000,000 | Cumulative element/item traversal and scope/offset metadata admissions. Repeated passes consume the same budget. |
| `MaxHookRegistrations` | 4,096 | Frozen hook-reference snapshot, checked under the registry lock before copying. |
| `MaxRuleRegistrations` | 1,024 | Frozen dataset-rule policy before its validation map/slice is allocated. |

Admission precedes each known library copy, including the third hook snapshot
before entering the callback and the library copy of a returned replacement.
A root scope shares these counters with descendants until the public write
returns. Charges are conservative and cumulative; they are not released on GC
or a guessed lifetime. Configuration and retained offset metadata also count
when a reused operation begins its next scope. Its report remains bounded by
`MaxFindings`; scope tokens/contexts expire between calls.

The counters measure logical requested backing. They are **not heap or RSS
caps**. They exclude input owned by the caller, Go allocator rounding,
map-bucket/runtime/stack overhead and arbitrary allocations made by hooks,
resolvers, rules or dictionary methods. Those functions must return before the
library can inspect their output. The library admits its subsequent copies
of returned values and paths; it cannot prevent the callback's own allocation.
Library text scratch uses a conservative bound on known bytes/components before
decoding or splitting; large text may require a higher explicit work budget.
No raw binary payload is decoded merely to estimate a reservation.

`parser.WriterOptions.SequenceLimits` is a separate domain for writer layout,
retained transformations, CP-246 byte-order normalization and writer metadata.
Both domains can require admission of the same operation without counting a
copy twice in either counter. Validation limits do not cover all encoder,
provider, deflater or destination allocations. Existing `MaxDepth`,
`MaxElements`, cancellation and collection limits still apply.

A borrowed input, its nested items, metadata and payload backing must remain
immutable for the complete synchronous call. Caller callback objects and
policy implementations are also caller-owned and must remain valid; freezing
registration/slice policy does not clone their arbitrary internal state.
`WriteOperation` and writers are not concurrent-safe. Discard contexts after
`EndWrite`; a context from another operation/epoch is rejected. The writer
retains neither a cross-call payload cache nor a deferred/bulk source. Existing
provider/resolver ownership, cancellation and single emission remain intact.

## Serialization changes and diagnostics

A dataset validated before `PreSerialization` is not an unconditional proof for
its replacement. Transformations, retags and filters invalidate the affected
ordinal/subtree even if the new value has the same length. Changed elements
are checked before their header, using the writer's effective character set;
new presence/duplicate/order defects use the effective item metadata. Required
UID policy remains a root-dataset check and uses the existing validator rather
than treating items as independent required-UID roots. Unchanged reports and
callback order are preserved; historical findings are not deleted.

When an opaque resolver/dataset rule has already established a context proof,
a change that would require replaying it returns
`validation.ErrWriteValidationScope`. A retroactive Specific Character Set
change likewise fails before the affected header when prior/planned values
cannot be proved under the new context. Observers returning no decision remain
allowed and their input mutations stay isolated. This is a deliberate refusal,
not memoization or a claim that arbitrary rules are pure.

An invalid replacement returns `ErrValidationFailed` in strict mode. Preserve
and warn modes keep their documented finding/severity behavior. Resource and
unprovable-scope failures reject in every mode. Previously committed top-level
bytes cannot be rolled back. Defined ancestors are prepared before their
header; undefined ancestors can already have committed a prefix when a child
fails. Existing `ValidationWriteError` bytes/completion and PostWrite behavior
still describe actual committed bytes, including short writes and late errors.
Group Length Create/Recalculate stale-transform refusal, padding, Deflate,
CP-246 and source metadata rules are unchanged.

## Qualification and measurements

Synthetic correctness includes detached typed/nil/empty values, callback and
rule isolation, unchanged reports in all modes, bounded admissions,
cancellation, expired scope tokens, changed/filtered/retagged required UIDs,
same-length/nested UID changes, opaque-context and charset refusal, deferred
source reuse and failure before Deflate publication. A published-base oracle
compares full reports, output bytes, callback order/offsets and completion.
These controls establish library contracts, not full IOD or clinical validity.

`BenchmarkValidatedWriterCopy` and `TestValidatedWriterMemoryProbe` are opt-in
synthetic controls. A reserved comparison on darwin/arm64, Go 1.27.1 used
published baseline `fd289b1ddb3de33cacc362e6975be10a2938bf64`, identical own
fixtures and immutable prebuilt executables. Thirteen cases had five pairs of
200 ms benchmarks and three fresh-process memory pairs each, alternating
baseline/candidate order. All 208 processes completed with identical output
bytes/SHA256 in all 39 memory pairs. The destination retains a SHA256 state.
Fixture construction is excluded from timed writes and B/op; writer,
validation, layout, provider and harness operations inside the write remain
included. These results qualify these synthetic paths on this host, not an
arbitrary dataset, caller callback, codec, PACS or clinical workflow.

Time values below are medians in milliseconds. Reduction is the median of the
five paired `(1-candidate/baseline)*100` values; brackets give their full range.
B/op is the median reported allocation volume per write, rather than retained
heap or bytes charged by an admission counter.

| Case | Baseline ms/op | Candidate ms/op | Paired time reduction % [range] | B/op baseline → candidate |
| --- | ---: | ---: | ---: | ---: |
| Nil1MiBDepth1 | 0.920 | 0.340 | 63.01 [61.05, 63.17] | 10,541,019 → 43,189 |
| Nil8MiBDepth1 | 6.532 | 2.704 | 58.56 [58.34, 58.98] | 84,285,668 → 315,272 |
| Nil32MiBDepth1 | 25.444 | 10.814 | 57.50 [57.13, 57.99] | 337,122,328 → 1,253,016 |
| Nil64MiBDepth1 | 50.329 | 22.235 | 54.92 [53.87, 56.50] | 674,188,884 → 2,485,944 |
| Nil1MiBDepth4 | 3.304 | 0.388 | 88.30 [87.48, 88.37] | 57,841,217 → 83,793 |
| Nil8MiBDepth4 | 22.194 | 3.009 | 86.45 [86.09, 86.52] | 462,468,428 → 548,168 |
| Nil1MiBDepth16 | 32.093 | 0.915 | 97.17 [97.05, 97.31] | 625,603,225 → 452,148 |
| Observer1MiBDepth4 | 3.852 | 1.153 | 70.07 [68.95, 72.08] | 73,599,382 → 15,843,340 |
| Transform1MiBDepth4 | 4.048 | 1.438 | 64.27 [64.05, 65.28] | 76,385,676 → 18,793,835 |
| Rule1MiBDepth4 | 4.077 | 1.504 | 62.84 [56.25, 65.61] | 73,601,481 → 21,097,294 |
| Resolver1MiBDepth4 | 3.187 | 0.695 | 78.44 [77.73, 79.07] | 57,847,635 → 5,347,637 |
| DataSet8MiBDepth4 | 8.877 | 2.816 | 68.41 [67.97, 68.59] | 159,940,530 → 381,120 |
| ObjectDeferred8MiBDepth4 | 2.629 | 2.636 | -0.33 [-0.84, 0.51] | 67,708 → 59,862 |

The Object case has native OB Pixel Data of 8 MiB **at the dataset root**
(2048×4096, one sample with 8 unsigned bits, MONOCHROME2) and small materialized
metadata nested in four SQ scopes. The bulk depth is zero; metadata depth is
four. Root deferral uses finite element/pixel/total/count/depth reader limits.
Its validated output, complete flag, full report and streamed payload hash
match the resident control in both versions. It showed **no timing advantage**:
paired median −0.33%, with samples from −0.84% to +0.51%. The validated Object
facade explicitly rejects deferred values nested inside SQ. An earlier own
fixture attempted that unsupported representation; its baseline failure was
preserved and the campaign stopped before timing. The corrected root-bulk
fixture was proved separately before a new fully reserved campaign.

The no-hook copy volume drops substantially, while metadata allocations and
required ancestor/descendant validation visits remain. Active hooks, returned
replacements and rule/resolver fallbacks still allocate their required isolated
known values. This is not constant total allocation or constant retained heap.
Allocator volume also includes provider/encoder/harness operations; neither
the copied-value budget nor a size/depth ratio predicts every B/op result.

Examples below are medians of three fresh processes in MiB. Additional heap
is the maximum sampled/final `HeapAlloc` minus its post-fixture-GC baseline;
absolute process RSS is the child peak reported by `/usr/bin/time -l`.

| Case | Additional sampled heap baseline → candidate MiB | Absolute process RSS baseline → candidate MiB |
| --- | ---: | ---: |
| Nil8MiBDepth4 | 67.196 → 0.556 | 98.109 → 29.500 |
| Nil64MiBDepth1 | 386.490 → 2.407 | 473.078 → 86.984 |
| Observer1MiBDepth4 | 19.960 → 11.115 | 43.266 → 33.109 |
| Transform1MiBDepth4 | 19.586 → 14.992 | 43.469 → 37.453 |
| ObjectDeferred8MiBDepth4 | 0.099 → 0.092 | 37.391 → 37.469 |

Heap sampling every 1 ms plus the final sample and RSS sampling every 20 ms can
miss transient peaks. Absolute RSS includes fixture construction, input,
runtime, executable mappings and retained allocator pages; it is not an exact
increment of writer memory. The Object case has a small increase in median
absolute RSS despite lower B/op; no universal RSS or timing gain is claimed.
The operational safety guard sums all members of a dedicated process group
(including the time wrapper), separately from the measured Go target PID.
Shared resident pages can count more than once. Its sampled 1 GiB group limit
is not a kernel allocation ceiling or the validation API's memory quota.
Every process has a 45 s limit and sampled 4 MiB guards for stdout and stderr
each, within a shared 120 s campaign limit. Direct children are reaped and groups verified
empty before final metric parsing or the next process; any cap/error stops
with retained partial evidence and no retry or partial PASS.
