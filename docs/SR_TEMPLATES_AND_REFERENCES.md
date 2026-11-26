# Structured Report references and template validation

Package `sr` provides structural support for DICOM SR content-item trees,
including by-reference relationships and caller-defined template/context-group
validation. This is a bounded library surface, not a claim of complete SR IOD,
Template ID (TID), or Context Group ID (CID) conformance.

## By-reference content items

`ContentItem.ReferencedContentItemIdentifier` preserves Referenced Content Item
Identifier (0040,DB73) as a one-based path from the document root. Both forward
and backward references are supported. A by-reference relationship item carries
only Relationship Type (0040,A010) and the referenced path; value type, concept
name, value fields, child content, and other by-value macros are rejected when
writing.

`ReadDocument` decodes and preserves reference paths but does not resolve or
validate the graph automatically. Applications that need navigation or
validation can use either:

- `ReadDocumentWithOptions` with `ResolveReferences` enabled; or
- `ResolveReferences` after constructing or reading the complete `Document`.

Resolution happens after the full content tree is available, so forward
references work. The returned `ReferenceIndex` supports edge enumeration,
source-to-target lookup, reverse lookup, and lookup by relationship slot. It is
immutable and bound to the indexed document shape. Structural mutation makes
subsequent index queries return `ErrStaleReferenceIndex`; callers must resolve
the document again.

Generic document serialization validates references with bounded strict
defaults before emitting a dataset. This prevents malformed by-reference
macros, dangling/self/ancestor references, cycles, and relationships forbidden
by the implemented SOP Class profile from being written silently.

### Relationship content constraints (PS3.3 Tables A.35.x-2)

Every SR Document IOD of PS3.3 A.35 (23 in edition 2026d, including the
Rendition Selection Document, SOP Class 1.2.840.10008.10.4) has a table of the
source value type, relationship type and target value type combinations it
allows, and a rule for by-reference relationships. The tables are generated,
not written by hand: `cmd/srrelgen -extract` reads the PS3.3 chtml pages into
`internal/standard/sr_relationships.tsv` (edition recorded in the header, see
`internal/standard/README.md`), and `go generate ./sr` turns it into
`sr/relationship_constraints_gen.go`. `RelationshipConstraintsEdition` names
the edition.

- `ValidateRelationships(document, options)` checks every by-value
  relationship, with the root CONTAINER as the source of the top-level items.
  Findings (`sr.relationship.forbidden`, `.missing`, `.unknown`,
  `.unknown_value_type`, `.resource_limit`) carry the item path, the SOP Class,
  the source and target value types, the relationship type and the rule
  (`PS3.3-2026d.Table.A.35.x-2`), never values. A value type the edition does
  not define is always a warning, since a later edition may define it. A SOP
  Class without a table returns `ErrUnconstrainedSOPClass`.
- `ReadOptions.ValidateRelationships` adds those findings to the read report
  (warning mode by default); `RelationshipAllowed` and
  `RelationshipConstraintFor` answer single questions.
- `ResolveReferences` applies each IOD's by-reference rule (none; all; all
  except `CONTAINS` and `HAS CONCEPT MOD` for Comprehensive and Comprehensive
  3D SR; only the listed relationships for the CAD and Waveform Annotation
  SRs) and then the same table. `CONTAINS` is also never accepted by-reference
  in Extensible SR, since it would give an item a second parent.
- `Document.Elements`/`Dataset` and `WriteMeasurementReport` refuse a tree that
  violates the constraints (strict mode). Checking the tree written by
  `WriteMeasurementReport` found that the Tracking Identifier, Tracking Unique
  Identifier and Referenced Segment of a measurement group were written with
  `CONTAINS`, `HAS PROPERTIES` and `INFERRED FROM`; they are now `HAS OBS
  CONTEXT`, `HAS OBS CONTEXT` and `CONTAINS`, as TID 1501 rows 2-3 and TID 1411
  row 7 require (a CONTAINER cannot have `HAS PROPERTIES` or `INFERRED FROM`
  children in the Comprehensive SR IODs). Reading matches these items by
  concept name, so reports written earlier still read. Likewise
  `NewKeyObjectSelection` wrote per-image findings as `IMAGE CONTAINS TEXT`,
  which no edition allows; they are now `IMAGE HAS ACQ CONTEXT TEXT`, the
  CP-2595 row (readers that predate CP-2595, such as DCMTK 3.7.0, refuse it).
  `KeyObjectSelectionFindings` reads either form. `ImageReference.RetrieveURL`
  is the Retrieve URL (0008,1190) of the referenced series in the evidence
  (Hierarchical Series Reference Macro, CP-2595): written once per series,
  refused when two references of a series disagree or it is not a URI without
  spaces, and read back on every image of the series.

For an SOP Class without a table, the resolver still checks the graph,
by-reference macro, path, and non-empty relationship type, but it does not
infer an IOD-specific relationship matrix. Callers must not treat a clean
structural report for such a UID as SOP Class conformance.

By-reference cycles that do not pass through an ancestor (such as two items
`INFERRED FROM` each other) are refused (`sr.reference.cycle`) although PS3.3
only forbids references to ancestors; dicom-go keeps this stricter rule so
that consumers following references always terminate.

DCMTK 3.7 is the oracle (opt-in `DICOMGO_DCMTK_INTEGRATION=1`):

- `TestRelationshipConstraintsMatchDCMTK` writes, for each IOD DCMTK reads,
  one child under each value type the root may contain for every relationship
  and target value type, and compares `dsrdump -Ei` ("Cannot add ...") with
  the tables: about 19,500 combinations over 21 IODs. DCMTK does not read
  Extensible SR or Enhanced X-Ray Radiation Dose SR, and has no `TABLE` value
  type. Documents of the ten IODs whose root template fixes the document
  title (Mammography, Chest and Colon CAD, Spectacle Prescription, Macular
  Grid, Implantation Plan, Simplified Adult Echo, Patient Radiation Dose and
  the two Imaging Agent Administration SRs) carry that title, which DCMTK
  checks since `92b04fe1` (the pinned `8d68392d` refuses any other title).
  The differences are all DCMTK lagging the 2026d tables: the 3.7.0
  release refuses the CP-2595 rows of Key Object Selection (`CONTAINER
  CONTAINS CONTAINER`, `IMAGE`/`COMPOSITE`/`WAVEFORM HAS ACQ CONTEXT`), which
  later DCMTK sources accept with a compatibility warning; it still accepts `PNAME HAS
  PROPERTIES DATE`/`TIME` in the X-Ray and Patient Radiation Dose SRs, accepts
  only `COMPOSITE` targets of `HAS PROPERTIES` in the Implantation Plan SR, and
  has `COMPOSITE` where the Simplified Adult Echo SR table has `CONTAINER` as a
  `HAS OBS CONTEXT` target.
- `TestMKReportDocumentsMatchDSRDump` (`DICOMGO_DCMTK_MKREPORT` names an
  `mkreport` built from DCMTK `dcmsr/tests/mkreport.cc`) reads its 23 reports:
  those `dsrdump -Ev` reads have no relationship finding here, and the
  deliberate by-reference loop of `reportlp.dcm` is the only reference
  finding (see the cycle rule above).

## Strict and warning modes

Reference resolution/validation during reading and all template validation are
opt-in operations. Existing `ReadDocument` callers do not receive new
reference-validation failures merely because a document contains a relationship
that needs application-specific handling. Serialization remains the fail-closed
boundary described above and always checks the reference graph.

`ValidationModeStrict` records error-severity findings and returns a typed error
(`ErrReferenceResolution` or `ErrTemplateValidation`) when violations exist.
The document/report and, for reference resolution, the built index remain
available for inspection. `ValidationModeWarn` returns the same structural
findings as warnings without rejecting the operation. `DefaultReferenceOptions`
and `DefaultTemplateValidationOptions` are strict; the opt-in reference step in
`DefaultReadOptions` uses warning mode when `ResolveReferences` is enabled.

This distinction does not make warning mode a repair mechanism. Encoded paths
are never rewritten, and applications decide whether a warning is acceptable
for their workflow.

## Template and context-group registry

Applications supply `TemplateDefinition` and `ContextGroupDefinition` values
to `NewTemplateRegistry`: their own, or the generated PS3.16 definitions of
`StandardTemplates` and `StandardContextGroups` (below). Nothing is
registered or validated implicitly.

Each `TemplateKey` and `ContextGroupKey` contains a mapping resource,
identifier, and explicit version. Context-group definitions can additionally
record source/checksum provenance. The constructor validates definitions,
duplicates, includes, and include cycles, then deep-copies the input into a
frozen registry. Lookups return caller-owned copies, and `WithTemplate` and
`WithContextGroup` return new registries rather than mutating an existing one.
Registries and validators may therefore be shared across goroutines.

`TemplateValidator` can check row cardinality and order, by-value versus
by-reference mode, relationship/value types, coded concept identity, context
group membership, includes, conditional rows, and caller-supplied encoded code
or template-identification metadata. Validation occurs only when the caller
invokes `Validate` or `ValidateWithMetadata`. A successful validation means the
document matches only the exact registered definition and metadata supplied for
that operation; it is not proof that the registry reproduces the current DICOM
standard.

When several rows describe a child equally well (the Measurement Group
CONTAINERs of TID 1410, 1411 and 1501 have the same relationship, value type
and concept name), the validator compares their child rows with the item's
children, one level deep, and chooses the row that matches most children
with the fewest unmatched children and missing required rows. A conditional
row's `Condition` receives its compiled `RuleID` and the number of children
each row of the same parent matched (`TemplateConditionContext.RowCounts`),
so a condition can depend on its sibling rows. An Include row with
`IncludeFillsRelationship` passes its relationship only to included rows
without one, as a PS3.16 INCLUDE row does.

### PS3.16 template catalog

`StandardTemplates(ids...)` returns PS3.16 templates, with every template
they include and the context groups of their enforced value sets, as
definitions keyed `DCMR`/TID/`StandardTemplateEdition`;
`NewStandardTemplateRegistry(ids...)` builds the registry directly, and
`LookupStandardTemplate` describes a template (name, Type, Order, Root, its
conditional rows and how many conditions are evaluated, includes outside the
catalog). Each definition records its provenance (edition, source and the
SHA-256 of the catalog). The catalog is generated: `cmd/srtemplategen
-extract` reads the PS3.16 DocBook into `internal/standard/sr_templates.tsv`
(edition, source hash and date in the header, see
`internal/standard/README.md`) and `go generate ./sr` writes
`sr/template_catalog_gen.go`.

The committed catalog holds 407 templates of PS3.16 2026d: the 404 numeric
TIDs and the letter-suffixed TID 10003A, 10003B and 10003C. The same pinned
`part16.xml`, edition, source SHA-256 and extraction date are preserved; the
catalog checksum includes the added tables. `StandardTemplateIdentifiers`,
`StandardTemplateKeyByIdentifier`, `LookupStandardTemplateByIdentifier`,
`StandardTemplatesByIdentifier` and `NewStandardTemplateRegistryByIdentifier`
accept exact canonical identifiers. Numeric identifiers have no leading zero;
the pinned edition's suffix is one uppercase ASCII letter. Integer APIs keep
all numeric clients and return only the numeric IDs. `StandardTemplateInfo.ID`
is zero for an alphanumeric TID; `Identifier` carries its normative name.

INCLUDE rows remain references to immutable definitions. Validation resolves
only the rows of an actual content parent; child definitions are visited when
content descends, rather than materializing every combination during registry
construction. TID 3900 and 3902 therefore register and validate small fixtures
under the same bounds. The four CAD self-includes (4004, 4006, 4102 and 4125)
remain in the definitions and accept finite nested findings. Their rule IDs and
paths identify the specific nested occurrence.

Custom definitions may set `TemplateRow.AllowRecursiveInclude` only on an
explicit self-include below ordinary parent rows. The include edge and every
ordinary ancestor must explicitly select `ReferenceModeByValue`; the compatible
zero/Either modes do not establish this recursion contract. An occurrence can
recur only after another content item is consumed. Default cyclic definitions,
mutual cycles, marked root self-includes and recursive by-reference edges or
ancestors are refused with typed registry errors. Empty content does not expand
speculative descendant combinations. Content references retain their existing
separate target/reference-mode rules; no referenced target tree is recursively
expanded as an INCLUDE. Include relationship fill/override, cardinality,
conditional requirements, ordering and scoped sibling counts are preserved.

Include visits and matching share `MaxSteps`. Current-level output additionally
has the raw row ceiling, bounded include depth and RuleID/condition scope
length; admission occurs before producing rows. Depth, items and findings bound
the actual tree. Finding exhaustion is marked truncated and retains a typed
resource-limit finding. Unsupported conditions remain a coverage limitation,
and these logical traversal/output bounds are not an RSS guarantee.

The catalog's notation becomes rows as `StandardTemplates` documents: the
root CONTAINER row of a root template is the validated document itself; EV
and DT concept names constrain the item; a value set is enforced only for a
DCID of a non-extensible group; M is required and U optional; an MC row is
conditional when its condition only tests sibling rows (TID 1500 rows 6, 10
and 12, the XOR rows of TID 1411, 1601 and 320, and other sibling-presence conditions)
and is otherwise validated as an optional row, as are UC rows. A clean
result therefore means the document satisfies the evaluable constraints of
the catalog, not complete PS3.16 conformance.

`TestStandardTID1500ValidatesTheDocumentsOfTheCurrentDefinition` compares
the generated TID 1500 with `CurrentMeasurementReportTemplate`: both reject
an empty report and one without Imaging Measurements, Derived Imaging
Measurements or Qualitative Evaluations; both accept the reports
`WriteMeasurementReport` writes (with no finding at all from TID 1500); TID
1500 also accepts what the current definition, which predates the
Derivation modifier and other implementations' rows, rejects: Derivation
modifiers, a person observer before the device observer, and the DCMTK and
dcmqi reports.

Enhanced Code Sequence attributes and Content Template Sequence identities are
available through the opt-in `ReadResult.TemplateMetadata` sidecar. The
sidecar keeps `CodedEntry` comparable while preserving coding-scheme versions,
context-group identifiers and versions, extension metadata, mapping-resource
identities, long/URN code values, equivalent codes, and template identities.
Use `Document.ElementsWithTemplateMetadata` or
`Document.DatasetWithTemplateMetadata` to re-emit that metadata. The legacy
`Elements` and `Dataset` methods intentionally retain their existing output;
callers that need enhanced-code round trips must use the explicit sidecar API.

Legacy metadata entries are keyed by one-based content paths. After inserting,
removing, or reordering content items, callers must discard or explicitly
rebuild the caller-owned sidecar before writing; otherwise the old path can
describe a different item. That legacy contract is unchanged.

For an automatic opt-in guard, use `ReadDocumentWithMetadataSnapshot` or
`BindTemplateMetadata`, followed by `ElementsWithMetadataSnapshot`,
`DatasetWithMetadataSnapshot`, or `ValidateWithMetadataSnapshot`. The privately
owned metadata and the ordered tree/content binding are checked before those
operations. Insertion, removal, reordering of distinguishable items, content
mutation and backing-storage replacement are rejected. A tree with completely
indistinguishable subtrees carrying different metadata cannot be bound safely
and fails closed rather than inventing identities. Rebinding is explicit and
does not reconstruct historical association. Context-aware renderers consume
this same guard to present short, long and URN code identifiers. See
[SR context binding and rendering](./SR_CONTEXT_RENDERING.md) for the bounds,
allowed header mutations, concurrency contract and presentation details.

## Resource and diagnostic policy

Reference resolution defaults to at most 64 levels, 100,000 content items,
100,000 references, 65 path components, and 1,024 findings. Template validation
defaults to at most 64 levels, 100,000 items, 1,000,000 matching steps, and
1,024 findings. Callers may set different positive bounds. Registry construction
is also bounded for definition, row, code, include, and string counts. Reports
set `Truncated` when the finding limit is reached.

Resource-limit exhaustion is fatal in both strict and warning modes. Warning
mode preserves conformance findings, but it never returns a partial reference
index or silently accepts input that exceeded a traversal or metadata budget.

Built-in error strings and `DiagnosticFinding` messages are value-free: they do
not echo SR text, person names, UIDs, dates, code meanings, or registry
definition contents. Findings identify structural paths and targets, stable
codes, severity, generic messages, and, where applicable, a caller-provided
template row `RuleID`. Callers must keep `RuleID` values free of PHI. The
diagnostic boundary also does not cover application logging or caller-owned
condition callbacks, which remain trusted application code.

## Measurement Report scope

`MeasurementReport`, `FromMeasurements`, `WriteMeasurementReport`, and
`ReadMeasurementReport` write and read the TID 1500 Measurement Report tree
in the Enhanced, Comprehensive, or Comprehensive 3D SR Storage SOP Classes:

- the root CONTAINER (126000) identifies TID 1500 (DCMR) and holds the
  Language of Content Item and Descendants (TID 1204, default en-US), the
  Observation Context (TID 1001/1002: person or device observers, default
  the dicom-go device `DefaultObserverDeviceUID`), the Procedures reported
  (121058, default 363679005 Imaging procedure), the Image Library (TID 1600,
  by default the images the measurements reference) and Imaging
  Measurements (126010);
- each measurement group is a Measurement Group (125007) identifying TID
  1501, or TID 1411 when it references a segment, with Tracking Identifier
  and UID, Finding, Referenced Segment, Source series for segmentation,
  Finding Sites and NUM measurements with their Derivation (121401, TID 300
  row 3, such as Mean or Maximum) and whose source image and 3D coordinates
  are INFERRED FROM with the purpose Source of Measurement (TID 320);
- every referenced instance that carries its Study and Series Instance UIDs
  is listed in the Current Requested Procedure Evidence Sequence, and the
  SR Document Series and General modules carry their Type 2 sequences.

For the same report, the tree equals, item by item, the one DCMTK 3.7.0's
own TID 1500 classes write (`sr/testdata/dcmtk_tid1500_oracle.cxx`), and
`ReadMeasurementReport` reads that DCMTK report back to the same model;
dsrdump and dciodvfy report no error on the written report (opt-in
`DICOMGO_DCMTK_INTEGRATION`, `DICOMGO_DCIODVFY`). Reports written before
this layout, with the groups directly under the root, still read
(`MeasurementReport.Legacy`).

Two other TID 1500 implementations are checked as well:

- highdicom 0.28.1 (`highdicom.sr.srread` and its `MeasurementReport`) reads
  a written Comprehensive 3D SR report
  (`TestMeasurementReportReadsInHighdicom`, opt-in `DICOM_GO_HIGHDICOM=1`
  with `DICOM_GO_PYTHON` naming an interpreter with highdicom). The script
  `sr/testdata/highdicom_measurement_report.py` prints what highdicom finds,
  and the test requires it to equal the report: the person and device
  observers, the image library, the TID 1501 and TID 1411 groups with their
  tracking identifier and UID, finding and finding sites, the referenced
  segment and its source series, each measurement's value, units,
  derivation, source image and SCOORD3D, and the evidence with its study
  and series.
- `ReadMeasurementReport` reads a report written by dcmqi 1.5.7's
  `tid1500writer` from synthetic inputs (`testdata/upstream/dcmqi/`,
  provenance in its README) back to the metadata it was given. That
  metadata covers the language, person observer, procedure reported, a
  three-image library, and two TID 1411 groups with Mean and Maximum
  measurements of one concept. Writing the report read and reading it again
  gives the same groups (`TestReadMeasurementReportReadsDcmqi`, offline).
  Reading this report is what showed that the Derivation modifier was lost,
  which left the Mean and Maximum measurements indistinguishable.

Not covered: Qualitative Evaluations and Derived Imaging Measurements (read
past, not written), Planar ROI measurements (TID 1410), the other TID 1419
and TID 300 modifiers (method, algorithm identification, laterality of a
finding site, time point, session, real world value map; read past), image
library entry descriptors (TID 1602) and procedure/subject context of TID
1001. `CurrentMeasurementReportTemplate` checks the structure written here
under its own key; it is not a normative TID 1500 definition. The catalog's
TID 1500 (`NewStandardTemplateRegistry(1500)`) validates the same reports.

## Document lifecycle

`Document.Complete`, `Verify` and `Revise` follow DCMTK's `completeDocument`,
`verifyDocument` and `createRevisedVersion`: completion is final (Completion
Flag COMPLETE with an optional Completion Flag Description); only a complete
document is verified, each verification appending a Verifying Observer
(name, organization, Verification DateTime, optional identification code)
and setting VERIFIED; a revision of a complete document lists it in the
Predecessor Documents Sequence, takes a new SOP Instance UID and returns to
PARTIAL and UNVERIFIED without description, observers or content date/time.
Writing refuses VERIFIED without a verifying observer, and Key Object
Selection documents, which have no SR Document General module, refuse all
three. `MeasurementReport` carries the same three fields. Verified and
revised documents read back unchanged, and dsrdump and dciodvfy accept them
(opt-in `DICOMGO_DCMTK_INTEGRATION`).

## Rendering

`RenderItems` formats a document for display: the title, a header (the
caller's fields, then document type, completion, verification, content
date/time and SOP Instance UID) and each content item in document order with
its path (the Referenced Content Item Identifier, root = 1, as dsrdump +Pn),
relationship, value type, concept and value: text, codes (with their code
under `ShowCodes`), numbers with UCUM units or their qualifier, dates and
times, person names, image/composite/waveform references by SOP Class name,
frames and channels, 2D/3D/temporal coordinates (bounded by `MaxPoints`), and
by-reference relationships as their target path. `MaxItems` bounds the items
and counts the rest.


## Independent interoperability checks

The offline unit suite covers forward/backward reference round trips in
Implicit VR Little Endian, Explicit VR Little Endian, and Explicit VR Big
Endian. Optional independent-fixture checks are also available:

- Set `DICOM_GO_PYDICOM_SR_FIXTURE` to pydicom's `test-SR.dcm` fixture to read
  and navigate its encoded by-reference relationships.
- Set `DICOM_GO_SR_INTEROP_OUTPUT` to emit a synthetic Comprehensive SR fixture
  with forward and backward references for inspection by an independent DICOM
  reader.

These checks demonstrate structural interchange for their fixtures. They do
not establish general SR, TID, CID, or IOD conformance.

Numeric Measurement Macro representations can be preserved with `ReadDocumentWithNumericRepresentations` / `BindNumericRepresentations` using this same metadata snapshot guard. Numeric DS/FD/rational fields are restored after code/unit contexts, and value/unit edits invalidate their association. See [SR_NUMERIC_REPRESENTATIONS.md](SR_NUMERIC_REPRESENTATIONS.md) for the opt-in finite/coherence profile, provenance, explicit editing and comparison limits.


## Detailed measurement context

The additive finite measurement view [SR_MEASUREMENT_CONTEXT.md](SR_MEASUREMENT_CONTEXT.md) keeps the generic document canonical and the existing metadata snapshot authoritative. Read/Build preserve explicit session/time point, method, algorithm identification, site modifiers, qualitative answers and RWVM provenance at admitted levels; observation NUM is separated from measurements. One combined bind reuses numeric source selection and guarded template/code/numeric/reference metadata. See the profile for quotas, strict admission/unsupported findings, explicit1411 RWVM seam, opaque endian output refusal and nested macro limitations. Existing generic and legacy specialized APIs retain their contracts.
