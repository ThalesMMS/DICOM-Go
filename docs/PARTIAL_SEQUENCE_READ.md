# Partial sequence reads

`object.ReadFileOptions.AllowPartialRead` and
`parser.Reader.ReadDataSetPartial` opt into inspection of an incomplete data
set. Normal readers remain strict. This option does not repair framing: it
stops at the first parser error without scanning for later tags or inventing
item or sequence delimiters.

Completed root attributes and completed sequence items remain available. An
interrupted item can retain the elements completed before its failure. Nested
interrupted sequences can likewise retain their item prefixes. Such sequence
and item wrappers are **incomplete**, not completed components. A sequence is
retained only when it contains a retained item; an interrupted item is retained
only when it has a retained element. Completed empty items remain present;
empty interrupted items and sequences are omitted. Truncated primitive values
and incomplete encapsulated Pixel Data are discarded atomically.

The separate [incomplete Pixel Data profile](INCOMPLETE_PIXEL_DATA.md) requires
its own explicit read/decode options and retains actual terminal bytes/Items for
complete-frame qualification. It cannot be combined with `AllowPartialRead`;
this generic collector keeps its atomic-drop behavior.

## Diagnostics and ownership

An incomplete File is returned together with `*object.PartialReadError`. The
error still matches `object.ErrPartialRead`, `object.ErrDataSet` and its original
cause via `errors.Is`/`errors.As`. `File.PartialRead()` describes the result:

- `Offset`, `Tag` and `Err` retain the existing failure metadata.
- `Elements` counts complete root elements, excluding a retained incomplete SQ.
- `RetainedElements` counts all retained root entries, including that SQ prefix.
- `Path` contains numeric tags and zero-based item indices. It ends at the
  failed attribute if known, otherwise the innermost open sequence or item.
- `Containers` lists incomplete SQ/item containers, outermost first. Each entry
  carries its tag, actual header offset and item index (`-1` for a sequence).
  `Retained` means its prefix is present, never that the container closed or
  owns a complete encoded source span. Closed items do not appear in this list.

For example, if item 0 closes and item 1 contains a complete UID followed by a
truncated PN, both item prefixes are retained. The failure path is
`(0008,1110)[1]/(0010,0010)`; Containers marks the outer SQ and item 1 incomplete
and retained. Item 0 remains a completed item. If item 1's header or first value
is truncated, it is omitted and its container, if successfully opened, is
marked `Retained=false`.

Paths and container lists returned by `File.PartialRead()` and by the File error
are separate owned copies. The parser returns the same diagnostics in
`*parser.PartialDataSetError`, which wraps the original failure. Metadata never
contains element values. The snapshot has one entry per successfully opened
container and obeys the normal combined sequence/item depth policy; no second
depth allowance is introduced.

## Limits and source-backed values

Partial collection consumes ordinary parser tokens. Defined-length boundaries
close only at their declared end; undefined-length containers close only upon
the matching delimiter. Existing element, value-byte, total-byte, fragment and
depth checks remain active, as do separately opted-in recovery switches.
Cancellation or deadline expiry never returns a partial File.

A completed primitive in a retained item can remain deferred on a seekable
source. Its original item offset and authenticated value location remain
unchanged. `CopyNestedValueTo`, `DeferredBulkDataReference` and
`CopyBulkDataValueTo` may access that complete primitive while its original
source remains open. An incomplete SQ or Pixel Data fragment sequence never
receives a fabricated complete source reference. Foreign or renamed item
paths do not authorize replay from the original source. A partial File opened
by `OpenFileWithOptions` retains its source handle until `File.Close`.

## Partial results and explicit rewriting

Incomplete results remain incomplete Files: `WriteFile`, frame writing,
validation and decompression refuse them with `object.ErrPartialFile`.
`WriteFileOptions.AllowPartial` explicitly permits writing a **new** structurally
closed file from the retained prefix. It never permits writing diagnosed
incomplete Pixel Data, whose odd tail could become a fabricated sample through
ordinary padding; that separate profile requires complete-frame normalization.
This does not restore omitted bytes,
prove semantic completeness, or preserve the original encoded container span.
Deferred complete values can be streamed into that explicit rewrite.

`StopBeforeTag` keeps its existing deliberate prefix-read contract. Raw
`object.ReadDataSetWithOptions` stays strict. Readers created with validation
lifecycle keep their existing strict collector: incomplete items and sequences
are not passed to item/sequence completion hooks. Transfer-syntax recovery
readers retain their existing behavior rather than accepting partial results.

## Evidence scope

Finite synthetic parser fixtures cover Explicit VR Little Endian, Explicit VR
Big Endian and Implicit VR Little Endian, defined/undefined SQ and item lengths,
seekable/stream sources, nested prefixes, empty containers, partial headers and
delimiters, encapsulated Pixel Data, malformed controls and existing limits.
File fixtures cover diagnostic ownership, write/validation refusal, explicit
rewrites, inherited/local character sets, cancellation and authenticated nested
deferred references. These are structural fixture checks, not external-PACS,
clinical or performance validation.
