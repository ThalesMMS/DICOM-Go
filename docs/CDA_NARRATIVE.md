# Bounded CDA narrative reader

`encapdoc.ParseCDANarrative` reads an inert, bounded presentation of the
`ClinicalDocument/component/structuredBody` section narrative. It reuses the
existing `InspectPayload` CDA namespace and HL7 identity checks. For encapsulated
input, `ParseEncapsulatedCDANarrative` also verifies a non-empty DICOM HL7
Instance Identifier (0040,E001) against the direct XML document identifier.

```go
document, err := encapdoc.Extract(dataset)
if err != nil {
    return err
}
declaredIdentifier, err := encapdoc.CDAEnvelopeIdentifier(dataset)
if err != nil {
    return err
}
narrative, err := encapdoc.ParseEncapsulatedCDANarrative(ctx, document.Data, declaredIdentifier,
    encapdoc.DefaultCDANarrativeLimits())
if err != nil {
    return err
}
// Render narrative.Title and narrative.PlainText as inert selectable text.
// Export document.Data to preserve the original CDA bytes.
```

The accepted XML namespace is `urn:hl7-org:v3`, with either a default namespace
or a prefix. `Title` comes from the direct document title.
`HL7InstanceIdentifier` retains the exact identity returned by `InspectPayload`.
`PlainText` contains the body presentation; the title and identity remain separate
header fields. The model does not retain the caller's mutable source buffer.

The identity check compares the XML `id` root and optional extension with the
envelope's `root^extension` form. It ignores DICOM ST trailing-space padding
and compares UUID roots case-insensitively. Extensions are case-sensitive.
An absent or empty envelope identifier allows XML-only reading. An unreadable
envelope element or a mismatch refuses the narrative. A mismatch returns
`ErrCDAIdentityMismatch` without exposing either value and leaves the original
XML available for extraction. `NewCDA` refuses an explicitly supplied
identifier when it contradicts the direct source document ID. The SOP Instance
UID identifies the DICOM storage object independently.

`Blocks` preserve source order and nesting through `Depth`. Their kinds are
`section`, `paragraph`, `list`, `list-item`, `table`, `caption` and `table-row`.
List items carry `Ordered` and a one-based `Number`; unordered lists use bullets
in `PlainText`. Table rows carry ordered `Cells`. The plain representation separates
cells with ` | ` and quotes cells containing separators, quotes, backslashes or
line breaks, so multiline cell text remains associated with its row.

The supported profile includes nested sections, mixed text and `content`,
paragraphs, explicit `br` breaks, ordered and unordered lists, captions, and simple
tables with `thead`, `tbody`, `tfoot`, `tr`, `th` and `td`. A nested list or block
inside a list item keeps its position and depth. Subscripts and superscripts use
`_(text)` and `^(text)` markers. Footnotes use a visible `[footnote: text]` marker.
Deleted revisions are suppressed and inserted revisions use `[inserted: text]`.
Clinical `entry` elements never supply narrative, including when section text is
absent.

This is a presentation subset rather than complete CDA schema, template,
signature or clinical validation. Styles are ignored with a diagnostic. Local
fragment links retain their labels with a diagnostic and remain inert.
Footnote references remain unresolved with a diagnostic. Unknown or foreign
narrative elements are omitted with a diagnostic. If these omissions leave no
readable narrative, parsing returns `ErrCDANarrativeAbsent`.

Multimedia, active elements, external resource references in narrative or titles,
XML processing instructions other than the XML declaration, table spans other
than one, and nested list/table blocks inside inline content or cells return
`ErrCDANarrativeUnsupported`. The safety scan also checks deleted or omitted
narrative subtrees. DTDs, undeclared entities and malformed XML are rejected by
the XML/envelope validation. Numeric character references and predefined XML
entities remain ordinary text. No parser path fetches URLs, reads referenced files,
executes markup, applies stylesheets, or derives display text from clinical entries.

Literal escaped markup and CDATA are returned as literal text. Title and body text
collapse ordinary XML whitespace and visibly escape control and bidirectional
control characters. The source HL7 identity is preserved exactly; display callers
must treat it as untrusted text too. Callers rendering HTML must escape all model
strings using their normal text escaping mechanism. The parser does not return
trusted HTML.

## Limits and errors

Every zero limit selects its finite default. Negative values or values beyond the
hard caps below return `ErrInvalidDocument`.

| Limit | Default | Hard cap | Accounting |
| --- | ---: | ---: | --- |
| `MaxBytes` | 16 MiB | 256 MiB | Source admission before XML decoding |
| `MaxDepth` | 64 | 64 | XML element nesting |
| `MaxTokens` | 100,000 | 1,000,000 | All decoded XML tokens, including ignored content |
| `MaxItems` | 10,000 | 1,000,000 | Each emitted block plus each table cell |
| `MaxTextBytes` | 2 MiB | 64 MiB | Combined title, identity, block and cell bytes; formatted body also bounded separately |

The temporary XML tree is bounded by source bytes, tokens and depth. Narrative
builders also bound individual text fragments before emitting them. Budget
exhaustion returns `ErrEncapsulatedDocumentTooLarge`; cancellation returns the
context error. An absent structured-body narrative returns `ErrCDANarrativeAbsent`,
and a `nonXMLBody` returns `ErrCDANarrativeUnsupported`. Every error returns a zero
model, without a partial clinical presentation. Diagnostics contain fixed codes
and counts, without source values:

- `style-hints-ignored`
- `table-layout-ignored`
- `local-link-inert`
- `footnote-reference-unresolved`
- `deleted-content-suppressed`
- `unsupported-element-omitted`

Consumers should show diagnostics as limitations of the displayed subset and
offer export of the original `Extract(...).Data`. Encapsulated Document Length
is handled by `Extract`; the parser never rewrites or pads the original payload.

## Opt-in CDA validation

`encapdoc.ValidateCDA(ctx, data, options)` checks a selected document header
and the structured body's local reference graph. The library defaults to
`CDAProfileBaseHeader` when the caller invokes validation. To validate while
encapsulating a document, supply the optional `cda2dcm -check-profile base`
or `-check-profile ccd` flag before the input and output paths.

The base profile checks direct header cardinalities for code, effective time,
confidentiality code, record targets, authors, custodian and body. It checks
single optional type ID, title and version identifiers. A supplied type ID
must declare CDA R2. The body can be structured or non-XML. If the document
declares a revision series, it must carry both `setId` and `versionNumber`.
The CCD header profile additionally checks for the CCD document template root
and the LOINC document code `34133-9` with code system
`2.16.840.1.113883.6.1`. It accepts the template identity with or without an
extension and leaves edition-specific template rules to the caller.

The graph checks literal `linkHtml` fragments and `footnoteRef` targets
within the document, including across narrative blocks. A referenced footnote
must be a footnote element. A local entry reference from `text` or
`originalText` must identify narrative in the same section. The checker
reports duplicate, dangling and ambiguous references. A reference from
inside an identified anchor contributes an edge from its nearest enclosing
anchor. A cycle in those edges produces a single document-level finding.
External references are outside this graph and remain inert. Both validation
and rendering leave the source payload intact.

Reports group findings by stable rule code and structural path with occurrence
counts. They contain no CDA values. XML, limit or context failures return an
error and an empty report. Byte, nesting and token limits reuse the narrative
parser defaults and caps. The command refuses publication on either a finding
or an error. The existing presentation and extraction paths remain available
independently. The validation scope excludes full XSD, clinical templates,
terminology bindings and signatures.

The header checks follow the [HL7 CDA R2 ClinicalDocument model](https://hl7.org/cda/stds/core/2.0.3-sd/StructureDefinition-ClinicalDocument.html)
and selected [CCD document constraints](https://hl7.org/cda/us/ccda/5.0.0/StructureDefinition-ContinuityofCareDocumentCCD.html).
The revision pair follows the [US Realm Header](https://www.hl7.org/cda/us/ccda/StructureDefinition-USRealmHeader-definitions.html).

## Sources and focused checks

The presentation behavior follows the narrative elements described in the
[HL7 CDA narrative reference](https://hl7.org/cda/stds/core/2.0.3-sd/narrative.html).
Identity and original encapsulated document length are described by
[DICOM PS3.3 C.24.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.24.2.html).
These references guide the bounded subset above; the reader does not claim full
CDA conformance.

The fixtures are synthetic and contain no patient data. Run:

```sh
GOMAXPROCS=2 go test -p 1 ./encapdoc -run '^Test(CDA|NewCDA|ParseEncapsulatedCDA|EncapsulatedDocumentsRoundTrip|EncapsulatedDocumentRules|InspectPayloadDocumentFamiliesPreserveBytes|InspectPayloadRejectsContradictoryMalformedFamilies|InspectPayloadLimitsAndCancellation)' -count=1
go test ./internal/doccli -run '^TestCDACommandOptInProfile' -count=1
```

The tests cover namespaces, nested order, lists, table cells, literal markup,
revisions, diagnostics, malformed or absent narrative, resource refusal without
network requests, exact budget admission, cancellation, and original envelope
bytes. Set `DICOMGO_CDA_NARRATIVE_PREVIEW_DIR` for the synthetic XML, text and JSON
model artifacts from `TestCDANarrativeVisualFixtureExport`.
