# SR metadata binding and code presentation

`sr.TemplateMetadataSnapshot` adds an opt-in guard around the existing
`TemplateValidationMetadata`. It privately copies Enhanced Code Sequence,
equivalent-code and Content Template Sequence metadata and binds them to one
ordered document tree. The same snapshot serves writing, validation and
presentation; consumers do not implement their own path reassociation.

The legacy `ReadDocumentWithOptions`, `ReadResult.TemplateMetadata`,
`ElementsWithTemplateMetadata`, `DatasetWithTemplateMetadata`, template
validation and render APIs retain their existing contracts. `CodedEntry`,
`CodeContext` and `CodeKey` remain comparable and the existing document/item
layouts are unchanged. A legacy sidecar is still caller-owned and can become
obsolete if the caller changes its tree.

## Reading and explicit binding

```go
result, snapshot, err := sr.ReadDocumentWithMetadataSnapshot(
    dataset, sr.ReadOptions{}, sr.MetadataSnapshotOptions{},
)
if err != nil {
    return err
}
return sr.RenderTextWithMetadataSnapshot(
    writer, result.Document, snapshot, sr.RenderOptions{ShowCodes: true},
)
```

`ReadDocumentWithMetadataSnapshot` uses the existing bounded reader and binds
only after a successful read. When binding fails, it returns the preserved
`ReadResult`, a nil snapshot and the error. A nil render snapshot deliberately
selects the legacy no-context presentation path; callers that require complete
code identifiers must handle the read/binding error before rendering.

For authored or already-read documents, `BindTemplateMetadata(document,
metadata, options)` explicitly binds metadata that the caller knows is already
associated correctly. It checks paths and code/template roles, but cannot
recover or verify historical provenance of a previously stale sidecar.
`snapshot.Metadata()` returns a defensive copy of all equivalent-code slices;
passing that copy to a legacy writer restores the caller-owned legacy contract.

## Association and mutation contract

The snapshot guards title, root SOP Class/continuity, ordered content structure
and every modeled `ContentItem` payload, including image hierarchy/frames,
waveform channels, coordinates, temporal values, reference paths, encoding
provenance and preserved `ValueElements`. Insertions, deletions, distinguishable
reorders and content changes invalidate it. Replacement of item backing storage
also invalidates it, even if copied values are equal. Returning the document
to its identical original guarded state restores validity; this is a state
guard, not an event log.

Other document headers, lifecycle fields, dates/instance identifiers and
`Common` modules do not identify content items and may change between snapshot
operations. Do not concurrently mutate the document or its reachable payloads
while any guard, writer, validator or renderer reads it. Multiple goroutines
may share the immutable snapshot and an unchanged document for reading.

Completely indistinguishable legacy values cannot reveal a swap by value-copy.
If two identical subtrees carry different attached metadata, including metadata
of their descendants, binding refuses them with
`MetadataSnapshotAmbiguous`. No identity is invented and no metadata is moved
by matching a concept/code. Equal subtrees carrying equal metadata are
association-equivalent; their exchange cannot attach a different code context.
For distinguishable items that share a Value Type and Concept Name, the complete
payload fingerprint detects reordering. If ambiguous documents need editing,
the caller must keep identity and metadata together in its own authoring model
and supply a newly correct document/sidecar explicitly.

`snapshot.Check(document)`, `Document.ElementsWithMetadataSnapshot`,
`Document.DatasetWithMetadataSnapshot`, and
`TemplateValidator.ValidateWithMetadataSnapshot` use the same guard. An obsolete
snapshot fails before element/dataset creation or template validation, including
warning mode. Rendering also checks every item before output, even when
`ShowCodes` is false or `MaxItems` would hide the changed item. Guard errors do
not return partially rendered documents or write a prefix to the output writer.
Ordinary writer failures retain normal `io.Writer` behavior.

## Resource and diagnostic bounds

Zero snapshot options select 64 content levels, 100,000 content items, 100,000
metadata/equivalent-code entries and 64 MiB of fingerprint input. `MaxDepth`
must be between 1 and 64. `MaxItems` and `MaxValueBytes` may be adjusted to
positive finite values; the existing metadata index also retains its 100,000
entry ceiling. Fingerprint input includes type/length markers, metadata and
fixed-size subtree hashes, rather than only text bytes. Exhaustion is explicit.
The walk hashes each item's payload once and composes child hashes, with
bounded auxiliary storage proportional to item count. There is no global cache
or mutable shared state.

Preserved in-memory core values are hashed as data without calling methods,
source handles or callbacks. Known raw, string, numeric, tag, sequence, fragment,
bulk-URI and discarded representations are covered; custom `core.Value`
implementations fail closed with `MetadataSnapshotUnsupported`. Preserved core
sequence nesting is independently bounded to 64 levels. Bulk URIs remain data
and are never fetched. Tree/sequence cycles terminate at the bounds.

`MetadataSnapshotError` contains only a stable code and unwraps to
`ErrMetadataSnapshot`. Its resource-limit code is also detectable with
`errors.Is(err, sr.ErrResourceLimitExceeded)`. Diagnostics do not include SR
text, names, UIDs, code meanings or identifiers. Malformed metadata uses the
existing value-free `TemplateValidationError` codes.

## Context-aware presentation

`RenderItemsWithMetadataSnapshot`, `RenderTextWithMetadataSnapshot` and
`RenderHTMLWithMetadataSnapshot` resolve the title, Concept Name, CODE value,
numeric units and Numeric Value Qualifier through the guarded context.
`ShowCodes: true` displays the actual Code Value, Long Code Value or URN Code
Value, followed by Coding Scheme Designator and Coding Scheme Version when
present. For example:

```text
Value (synthetic-long-value-code, 99VALUE, V2)
Concept (urn:example:concept)
12.5 Units (synthetic-long-units-code, 99UNITS, V3)
```

No empty scheme/version is invented for a URN. A missing Code Meaning falls
back to the preserved identifier. A present numeric qualifier is displayed with
the measurement under `ShowCodes`; for an absent measurement it remains the
value explanation. `ShowCodes: false` keeps meaning-only presentation and the
existing compact UCUM unit rules. Equivalent codes and template identities
are retained for writing/validation; rendering does not invent new semantic
roles for them.

These entry points share the legacy text/HTML emitters. HTML escapes markup;
text escapes terminal-control and bidirectional-control characters, while
retaining the existing newline/tab formatting contract. Code URLs and URNs are
inert text, with no active links, attributes, scripts, external resources or
network I/O. The selected-code and optional scheme/version rules follow
[PS3.3 2026d Table 8.8-1](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_8.8.html).

## Verification scope

Synthetic tests exercise the code roles, exact actual presentation, escaping,
no URL requests, stale/foreign context refusal before output, structure and
payload mutations, ambiguity refusal, explicit reconstruction, enhanced/template
round trips, bounds and concurrent unchanged readers. Existing render goldens
remain unchanged. Native, race, Go 1.22 and vet checks are recorded in the
delivery evidence; the integrated library check remains a separate gate.

The opt-in `TestContextRenderDCMTKIdentifiers` writes an independently authored
Part 10 report and compares identifiers, schemes and versions in their actual
DCMTK `dsrdump +Pn +Pc +Pl` content paths, rather than comparing formatting.
Enable with `DICOMGO_DCMTK_INTEGRATION=1` and optionally `DICOMGO_DCMTK_BIN`.
The compared fixture supplies an optional coding scheme for its URN concept.
The locally qualified DCMTK 3.7.0+DEV refused the otherwise permitted scheme-free
URN concept; that result is retained as an oracle limitation. Independent
contract tests cover scheme-free URNs. This focused comparison does not claim
general IOD, clinical or PACS interoperability validation, and no performance
gain is claimed.
