# Semantic presentation of imported SR

`sr.RenderPresentation(document, options)` exposes a reusable semantic model
alongside the technical `RenderItems`, `RenderText` and `RenderHTML` views. The
application chooses a view without interpreting the SR tree again. This path
reads existing document facts; it does not author templates, validate clinical
content or recover missing authorship.

## Model and continuity

`PresentationDocument` contains `Title`, `Header`, root `Continuity`, flat
preorder `Blocks`, and `Truncated`. Each `PresentationBlock` retains its `Path`,
`Depth`, `Relationship`, `ValueType`, `Concept`, `Value`, `ReferenceTo`, and
container `Continuity`. Paths count the implicit root as `1`, matching the
technical renderer and Referenced Content Item Identifier.

`InNarrative` explicitly marks non-container `CONTAINS` values and references
under an immediately continuous container. Applications group these blocks
using the library-derived flag; they do not reinterpret continuity or infer
the parent from content paths.

Block `Kind` describes layout:

| Kind | Meaning |
| --- | --- |
| `section` | A separate container, including a container with missing or unknown continuity. The original continuity remains visible in the model. |
| `narrative` | A container with `CONTINUOUS` content. |
| `inline` | A by-value non-container `CONTAINS` child of an immediately continuous container. |
| `item` | A separate value or a context/modifier relationship. |
| `reference` | A by-reference relationship, retaining its target path. A continuous `CONTAINS` reference may participate in the parent's flow. |

Continuity applies only to the immediate `CONTAINS` children of a container.
Relations such as `HAS OBS CONTEXT` are displayed separately. A nested container
uses its own continuity; a measurement's child relationships do not inherit the
measurement's surrounding narrative. Source order, including context relations
between narrative values, is retained. Nested content remains attached to its
parent; separate relationships interrupt the surrounding flow. These rules follow
[PS3.3 2026d C.18.8.1.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.18.8.html).

The root remains implicit in the flat model and has an HTML anchor `item-1`.
Separate containers become sections; continuous values share a flow group
with HTML paragraph semantics.
Both layouts retain individual item anchors. Containers nested under values
and non-narrative relationships remain visibly indented. Missing headings do
not generate invented clinical content.

## Lifecycle facts and references

The shared header now includes an existing completion description, verifying
observers (name, organization, date/time, identification code) and predecessor
documents (class/instance, study and series identifiers). Absent fields are
omitted. The same optional fields are available in technical renderers; their
existing output for documents without these fields remains unchanged.

`Document verification` displays the document's recorded Verification Flag;
`VERIFIED` records verifying observer attestation. The technical header keeps
its existing `Verification` label. Presentation neither verifies a cryptographic signature nor
establishes trust in the observer. Predecessors identify referenced documents;
they are inert text, with no implicit search, navigation or retrieval. The
semantics of these source attributes are defined in
[PS3.3 2026d C.17.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.17.2.html).

By-reference relationships display the relationship and target path only.
Malformed value type, concept, value, continuity and children on a reference
node are suppressed, including in the technical renderer. Such children are
not counted as valid omitted content. The target's payload is never copied or
expanded. HTML links use generated local paths; unresolved or truncated targets
remain identifiable references, without loading external resources.

## Bounds and code context

`RenderOptions.MaxItems` bounds emitted content blocks (default 100,000).
`Truncated` reports the number of additional content items. Each verifying
observer and predecessor list independently shows at most `MaxItems` entries,
then a header field with the omitted count. Caller-supplied header fields retain
their existing contract. `MaxPoints` bounds displayed coordinate values (default
32), using the same formatting as the technical renderer.

The semantic walk is iterative, with at most 64 content levels. Excess depth,
including cyclic authored trees and items beyond the output limit, returns an
error wrapping `sr.ErrResourceLimitExceeded`. Counting omitted items requires a
linear walk over admitted input; limits bound emitted content and traversal
depth rather than replacing the reader's byte/item admission limits. Input text
size remains the responsibility of bounded reading or the authoring caller.
Readers/renderers must not run concurrently with mutation of a document.

`RenderPresentationWithMetadataSnapshot`,
`RenderPresentationTextWithMetadataSnapshot` and
`RenderPresentationHTMLWithMetadataSnapshot` reuse the existing snapshot guard
and short/Long/URN code formatting. A stale snapshot fails before returning a
model or writing output, even when the modified content is outside `MaxItems`.
A nil snapshot chooses ordinary semantic presentation. `ShowCodes` displays
available identifiers and scheme/version using that same code-context path;
see [metadata association and code presentation](./SR_CONTEXT_RENDERING.md).

Model strings remain plain text. Text output escapes terminal and bidirectional
control characters; HTML escapes controls and markup. Code URLs and URNs remain
inert text. The standalone HTML preview contains no scripts or external assets.
Ordinary writer failures retain `io.Writer` behavior.

## Use and local verification

```go
presentation, err := sr.RenderPresentation(document, sr.RenderOptions{
    MaxItems: 2000,
    MaxPoints: 16,
})
if err != nil {
    return err
}
// An application adapts presentation.Blocks and presentation.Header.
return sr.RenderPresentationHTML(writer, document, sr.RenderOptions{
    MaxItems: 2000,
    MaxPoints: 16,
})
```

Synthetic tests cover nested continuous/separate containers, immediate-child
continuity, relationship order, a partial document, a verified revision with a
predecessor, absent lifecycle facts, malformed references, content/header/point
bounds, cyclic depth refusal, escaping, local root links, snapshot reuse and
stale refusal before output. Existing technical golden files remain unchanged.
Optional HTML/text previews for visual inspection are generated by:

```sh
DICOMGO_SR_PRESENTATION_PREVIEW_DIR=/tmp/sr-semantic-preview \
    go test ./sr -run '^TestPresentationVisualFixtures$' -count=1
```

The fixtures contain synthetic names and identifiers. Functional unit tests and
preview inspection do not establish clinical conformance, live PACS behavior or
cryptographic verification. The module `make check` remains the integration gate.
