# Opt-in validation and lifecycle hooks

`dicom-go` provides a uniform validation engine for VR/VM, dictionary,
dataset, File Meta and caller-owned rules. Validation is deliberately opt-in:
the existing `parser.NewReader`, `object.ReadFile`, `object.ReadDataSet`,
`object.WriteFile` and `object.WriteDataSet` paths do not run validation or
hooks and retain their established wire and error behavior.

This package is a structural and value-representation validator. IOD and
Module requirements are checked separately by `validation/iod` (see
[IOD and Module rules](#iod-and-module-rules)). Neither is, by itself, a DICOM
Conformance Statement.

## Basic use

Validate an already materialized dataset:

```go
result, err := validation.ValidateDataSet(ctx, dataset, validation.Options{
	Mode:        validation.ModeStrict,
	Dictionary:  std.Dictionary,
	MaxFindings: 128,
})
if err != nil {
	// errors.Is(err, validation.ErrValidationFailed) identifies strict
	// validation rejection. result.Report remains available.
}
dataset = result.DataSet
```

Parse a raw dataset while retaining parser offsets and lifecycle events:

```go
reader, err := parser.NewReaderWithValidation(
	ctx,
	source,
	transfer.ExplicitVRLittleEndian,
	parser.ReaderOptions{Dictionary: std.Dictionary},
	validation.Options{Mode: validation.ModeWarn, MaxFindings: 128},
)
if err != nil {
	return err
}
dataset, readErr := reader.ReadDataSet()
report := reader.ValidationReport()
```

At the object facade, `ReadDataSetWithValidation`,
`OpenDataSetWithValidation`, `Object.ValidateDataSet`, `File.ValidateFile`
and `WriteDataSetWithValidation` expose the same engine. The root `dicom`
package re-exports the common raw-dataset entry points. File validation and
serialization remain explicit composition: call `File.ValidateFile`, then the
existing `WriteFile` only when the selected policy permits it.

`parser.Reader.Next` exposes header and primitive decoded-element events, but
streaming tokens alone cannot supply a complete dataset to dataset rules.
Call `ReadDataSet` on a validation reader when item/sequence completion,
dataset-complete, pre/post-validation hooks or dataset rules are required.

`pixeldata.MetadataValidationRule` is an optional dataset-rule adapter for
cross-field image metadata and native Pixel Data length. It is not enabled
automatically.

## Modes and collection bounds

- `ModePreserve` (zero value) records findings at their natural severity and
  returns transformed/preserved data without rejecting it.
- `ModeWarn` downgrades validation errors to warnings.
- `ModeStrict` returns `ErrValidationFailed` when validation encounters an
  error, including when the report limit omits that finding.

`MaxFindings`, `MaxDepth` and `MaxElements` bound diagnostics and traversal.
`StopFirst` stops validation after the first finding, whether retained or
omitted. A truncated report sets `Truncated` and increments `Dropped` for
each omitted finding or change. `FindingsDropped` and `ErrorsDropped`
describe omitted diagnostics after applying the selected severity policy.
`Report.HasErrors()` checks both retained and omitted errors.

Reports contain tags, VRs, stable rule codes, paths, offsets and
library-generated messages. They do not contain element values. Hook and rule
callbacks are trusted in-process code: they receive dataset/element copies and
are themselves responsible for not exporting PHI.

## IOD and Module rules

`iod.Validate(obj, iod.Options{})` checks a data set against its IOD. The IOD
comes from the SOP Class UID (`iod.IODForSOPClass`) unless `Options.IOD`
names one ("CT Image"). The rules are the PS3.3 edition in
`iod.StandardVersion` (2026d). `cmd/iodmodulegen -rules` projects them from
the DocBook `part03.xml` into `internal/iodmodules/rules_generated.go`: every
Module and Macro table row, with its nesting depth, Type, Include and the
condition text of 1C/2C rows and conditional Includes.

```sh
go run ./cmd/iodmodulegen -in part03.xml \
	-out internal/iodmodules/modules_generated.go \
	-rules internal/iodmodules/rules_generated.go \
	-constraints internal/iodmodules/constraints_generated.go
```

Validation walks the Module trees into Sequence Items:

- Type 1 absent or empty, Type 2 absent: errors.
- Type 1C/2C: the condition is parsed when it can be formalized. This covers
  presence or absence, "has a value", equality or inequality with Enumerated
  Values (including "equals other than", "is present with Value"), "Value n
  of", "greater than" and non-zero checks, joined by "and" or "or", looked up
  in the current Item first and then in the data set. Attributes may be named
  without their tag ("Number of Frames is greater than 1", matched to the
  standard dictionary) and names may contain "and"/"or"; an AT Value can be
  compared with tags ("Frame Increment Pointer (0028,0009) is Frame Time
  (0018,1063) or Frame Time Vector (0018,1065)"); a code Sequence can be
  required to hold an Item with a given code ("contains an Item with the
  value (392012008, SCT, ...)"). A few conditions worded as facts are read
  from the Attributes that record them: a Frame-Level retrieve from Frame
  Extraction Sequence, Multi-frame Cine data from Frame Increment Pointer,
  contrast media from Contrast/Bolus Agent or its Sequence, lossy
  compression from Lossy Image Compression. When a parsed condition is true, the
  Attribute is checked like Type 1/2. When it is false and PS3.3 says "shall
  not be present otherwise", a present Attribute is a warning. An absent
  Attribute whose condition cannot be parsed is reported as
  `ManualCondition` (info) for manual review.
- Conditional Includes (Macros "Required if ...") are checked only when their
  condition is parsed and true.
- Module selection: Mandatory Modules are always checked. A Conditional
  Module is checked when its parsed condition is true. Otherwise (unparsed
  condition, or User Optional usage) a Module is checked when one of its own
  top-level Attributes is present. Such a Module is inferred, so its errors
  are reported as warnings. `Report.Modules` records each decision and its
  reason.

`iod.ConditionCoverage` is the coverage report: over every table and IOD of
the generated rules, how many conditions are evaluated, and the distinct
texts that are not, most frequent first. For PS3.3 2026d
(`TestConditionCoverage` logs it and keeps the floor):

| Conditions | Earlier grammar | Before source-bound rules | Current |
| --- | --- | --- | --- |
| Type 1C Attribute rows | — | 1,150 / 1,818 | 1,152 / 1,818 |
| Type 2C Attribute rows | — | 99 / 179 | 101 / 179 |
| All 1C/2C Attribute rows | 1,099 / 1,997 (55.0%) | 1,249 / 1,997 (62.5%) | 1,253 / 1,997 (62.7%) |
| Conditional Includes | 10 / 13 | 10 / 13 | 10 / 13 |
| Conditional Modules | 36 / 328 (11.0%) | 116 / 328 (35.4%) | 120 / 328 (36.6%) |
| Distinct condition texts | 716 / 1,494 (47.9%) | 820 / 1,494 (54.9%) | 826 / 1,494 (55.3%) |

`Coverage.Type1C` and `Type2C` split the attribute total. Coverage counts a
condition for which the evaluator exists, including evaluators that report
an unknown result on incomplete input. A condition text shared by different
source rows is completely covered only when every row has an evaluator.
`Unevaluated.Rows` counts the unresolved rows using that text.

### Source-bound CT, MR and SEG conditions

Fourteen source-bound rules supplement the grammar. They share the generated
edition and input SHA-256 above. The normative rule IDs retain table, physical
row and generated index. They are exercised through `Validate` and
`ValidateWithExternalContext` in `TestPriority*`.

| IOD or shared Module | Attribute or Module | Stable rule IDs |
| --- | --- | --- |
| CT Image | Energy Weighting Factor at image and additional-source level | `table_C.8-3/row-53/index-52`, `table_C.8-3/row-62/index-61` |
| Enhanced CT Image, also included by Legacy Converted Enhanced CT | Acquisition DateTime and Acquisition Duration | `table_C.8-114/row-5/index-4`, `table_C.8-114/row-6/index-5` |
| MR Image | Repetition Time, Inversion Time, Trigger Time | `table_C.8-4/row-11/index-10`, `table_C.8-4/row-14/index-13`, `table_C.8-4/row-15/index-14` |
| Segmentation | Fractional Type, Maximum Fractional Value, per-item Algorithm Name | `table_C.8.20-2/row-13/index-12`, `table_C.8.20-2/row-14/index-13`, `table_C.8.20-2/row-18/index-17` |
| Enhanced CT Image | Supplemental Palette Color Lookup Table Module | `table_A.38-1/row-20/index-19` |
| Enhanced MR Image | Supplemental Palette Color Lookup Table Module | `table_A.36-1/row-21/index-20` |
| Segmentation | Frame of Reference and Common Instance Reference Modules | `table_A.51-1/row-9/index-8`, `table_A.51-1/row-23/index-22` |

CT energy weighting requires the exact DCM coded entry 113097 in the root
Derivation Code Sequence. A code in an unrelated nested sequence does not
activate it. Acquisition conditions distinguish ORIGINAL/MIXED, DERIVED and
the legacy CT exception using the actual SOP Class UID. MR time conditions
read multi-valued scanning sequence, variant and gating attributes. Unknown
defined terms remain unresolved when they cannot prove a true branch.

SEG Algorithm Name is evaluated from its own Segment Sequence item.
Fractional requirements distinguish BINARY, FRACTIONAL and LABELMAP. The
Label Map Segmentation Storage SOP Class resolves to the Segmentation IOD.
The two conditional SEG Modules inspect actual Shared/Per-Frame Functional
Group placement and Number of Frames. A root Derivation Image Sequence does
not establish a Functional Group. Absent shared metadata, malformed sequences,
inconsistent frame counts and incomplete per-frame coverage keep the
corresponding decision unresolved. These checks inspect materialized metadata
within the default Functional Group frame/item/element bounds.

Missing, empty or malformed operands yield `ManualCondition` for audited
attribute rules even when the target is present. Conditional Module records
use `condition unknown` unless presence supplies the usual inference, whose
required-attribute findings remain warnings. A false Frame of Reference
requirement still permits checking a present optional Module. External-context
reports identify an unresolved audited rule as
`indeterminate-dataset-condition`. Type 2C targets may be empty when required.
The remaining generated rules continue through the existing grammar.

These rules concern dataset-decidable requirements. Lossy history in SEG source
instances, actual use of contrast or synchronization, incomplete references
and other externally dependent conditions retain their manual/context paths.

[CT Image Module](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.2.html),
[MR Image Module](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.3.html),
[Enhanced CT Image Module](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.15.2.html),
[Segmentation Image Module](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.20.2.html),
[Enhanced CT IOD](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.38.1.3.html),
[Enhanced MR IOD](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.36.2.3.html),
[Segmentation IOD](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.51.3.html).

The most frequent conditions left are facts the data set does not record
("present and consistent in the contributing SOP Instances", "a Performed
Procedure Step SOP Class was involved", "time/cardiac/respiratory
synchronization was applied", "annotation is present", "is to be applied to
referenced image(s)") and references to other tables or sections whose text
the DocBook projection drops ("the conditions in are satisfied").

### Explicit external condition context

`iod.ValidateWithExternalContext(obj, opts, context, maxChecks)` is an opt-in
API returning a separate `ContextReport`; `Validate`, `Options`, the legacy
`Report` shape and the CLI default are unchanged. `ContextReport.Conditions`
identifies each encountered conditional row, Include or module usage by its
stable rule ID, exact attribute/item path, normative source and retained
DocBook cross references. `Local`, `External` and `Pending` separate dataset
checks, caller-resolved checks and unresolved conditions. `Complete()` requires
no pending/dropped checks or error findings; it does not certify a complete IOD
or clinical conformance. Conditional optional modules still follow the existing
presence inference when their condition is unresolved.

The first external bindings are:

| Family | Source rows capable of external resolution | Caller fact |
| --- | --- | --- |
| `PerformedProcedureStepParticipation` | 27 Series Module rows for `(0008,1111)` | A Performed Procedure Step SOP Class was involved in creation of this Series. The separate condition about SOP Class support remains manual. |
| `ContributingSOPInstanceConsistency` | 10 rows of General Contributing Sources Macro, PS3.3 10.10 | The attribute is present **and** consistent across all contributing SOP Instances of this exact invoking sequence item. |

These are external-capable row counts, not additions to dataset-only
`ConditionCoverage` or measured completeness. `ExternalConditionCoverage()` and
`TestExternalContextPublicCompatibilityAndCoverage` reproduce the counts; the
legacy coverage inventory above stays separate. Other facts, including applied
synchronization, display/annotation intent, partial referenced frame/segment
scope and support of particular SOP Classes, remain pending with a fixed reason.

Start with a nil external context to obtain stable `ConditionCheck.Rule` and
`Path`. Supply `ExternalFact` entries with an audited `Family`, that exact
`Rule`/`Path`, `ConditionTrue`, `ConditionFalse` or `ConditionUnknown`, and the
SHA-256 of evidence retained by the caller. `NewExternalContext` copies a finite
snapshot: at most 256 facts and 64 KiB of text, bounded rule/path lengths and
hex digests. It stores no source objects, clinical values, files, callbacks or
network clients. It cannot infer correctness or authenticity of caller evidence;
the caller is responsible for resolving all contributors within each fact's
scope. A fact for item `[1]` cannot resolve item `[2]`.

Unknown or absent evidence leaves explicit pending/manual diagnostics, including
when the target attribute is present. Conflicting true/false assertions for the
same scoped rule produce `ContradictoryExternalContext` (error) and manual review,
even in a module inferred from presence. Contradiction is never last-wins or a
silently false condition. Findings record rule/path and fixed messages without
caller values. Caller evidence hashes appear only in contextual condition checks.
`maxChecks=0` means 10,000; a maximum of 65,536 bounds retained checks and their
identity cache. Truncation is explicit; no discarded check counts as a pass.

The additive generator flag `iodmodulegen -sources rule_sources_generated.go`
preserves physical DocBook table/row, projected index/depth/tag, section,
PS3.3 edition/input hash and cross-reference target identities. It generates
only an additive sidecar; legacy module/rule tables are reproduced and checked
byte for byte during qualification. The pinned source is PS3.3 2026d DocBook
SHA-256 `3ae695b956ba57c490933a49220b515aeaedb5ece86f05c4bf7e94ee0fbcc6ef`.
No arbitrary normative prose is interpreted for external facts at runtime.

`Report.Findings` are typed (`Kind`, `Severity`, `Module`, `Path`, `Tag`,
`Type`, `Condition`) and hold no values. `Report.Errors` selects the errors.
`MaxFindings` bounds the report and sets `Truncated`. The zero-value options
retain these presence/condition checks. Encoding and dictionary VR/VM remain
in the structural validator above.

### Scoped value constraints

`iod.Options{CheckValues: true}` additionally validates literal PS3.3 value
restrictions in the selected Module/Macro context. `dcmvalidate -values` exposes
the same opt-in in text and JSON. A closed Enumerated Values list produces
`InvalidEnumeratedValue`; an additional Defined Term produces the warning
`AdditionalDefinedTerm`, because that list is extensible. Rules inherited from
an inferred optional Module retain the existing warning severity. Entirely
empty Attributes remain governed by Type, so Type 2 empty values are allowed.
A populated tuple's empty component must still satisfy its component list.

Generation is pinned to PS3.3 2026d DocBook SHA-256
`3ae695b956ba57c490933a49220b515aeaedb5ece86f05c4bf7e94ee0fbcc6ef`:
857 enumerations, 303 Defined Terms lists, one contextual count rule and 251
manual qualifications. Source table/row, generated index, depth, tag, section,
edition/hash and stable rule ID accompany value findings; examined values do
not. Conditional list titles compile to predicates. Unproved prose and
cross-section restrictions remain `ManualValueConstraint` and are exposed by
`iod.ValueConstraintCoverage`, rather than guessed from a global tag list.

Value-position restrictions apply to the specified component. Dictionary VM
still counts primitive values, with SQ VM 1 independently of its Item count.
The proven contextual rule checks Dimension Index Values against the root
Dimension Index Sequence's Item count. Present Frame Content and Frame
Usefulness macros in Shared/Per-Frame Functional Groups receive value-only
checks; this does not add default Type/presence requirements. The two source
attributes named Includes Information and Includes Imaging Subject use
supplemental bindings (source index -1), preserving the legacy generated
presence tables byte for byte.

Each primitive examined by value constraints is bounded to 1 MiB. Undecoded,
deferred, malformed or unsupported representations remain manual; structural
encoding validation supplies its own errors. Missing contextual inputs remain
manual too. This is a scoped value validator, not proof of complete IOD or
clinical conformance. Eight selected strict GDCM files produced no new
enumeration/context-count errors and three additional-term warnings in two MR
rules; that small corpus does not qualify arbitrary IODs.

The tests of the object writers (`seg`, `rtstruct`, `sr` with Key Object
Selection, `vps`, `gsps`, `encapdoc`, `encapimage`, `imagebuild` and the
`multiframe` classic and Legacy Converted conversions) require that
`iod.Validate` report no errors. The classic conversion also uses it to
write the Type 2C Attributes whose condition holds, empty (Repetition Time
of a non-echo-planar MR Image).

Two opt-in tests compare the validator with independent tools:

```sh
DICOMGO_DCIODVFY=/path/to/dciodvfy go test ./validation/iod -run DCIODVFY
DICOMGO_DCMTK_INTEGRATION=1 go test ./validation/iod -run DCMPSCHK
```

- The dciodvfy test (dicom3tools) uses a CT and a Secondary Capture image, a
  GSPS, a KOS and an encapsulated PDF written by dicom-go, plus copies of
  each with one top-level Attribute removed. Both tools must report the same
  absent Type 1 and Type 2 top-level Attributes, and none for the objects as
  written.
- The one known difference is Modality in a Secondary Capture: it is Type 1
  in PS3.3's General Series Module, but dciodvfy does not require it there.
- The dcmpschk test (DCMTK) requires that dcmpschk accepts the GSPS as
  written. Whenever dcmpschk rejects a copy, Validate must also report the
  problem.

## Dictionary and VR/VM behavior

The standard dictionary preserves contextual VR alternatives (`xs`, `ox`,
`px`, `lt` and `up`) through the optional `dictionary.VRSpecDictionary`
contract while keeping `dictionary.Entry` comparable and source-compatible.
`dictionary.Chain` gives the first matching overlay ownership of both the
entry and its VR specification. A caller can resolve additional contextual
cases through `Options.ResolveVR`.

Unknown and private tags are retained; they do not become false standard-VR
mismatches. VM is derived from the actual typed or raw value. Sequences,
fragment sequences and Other-byte/word VR payloads each have VM 1 regardless
of item or byte count.

### UID syntax and normalization

Canonical DICOM UIDs contain at least two numeric OID components, use root arc
`0`, `1` or `2`, limit the second arc to `0` through `39` when the root is `0`
or `1`, contain no leading zeroes in multi-digit components, and occupy at most
64 bytes. `core.IsValidUID` checks only this canonical syntax;
`core.NormalizeUID` removes encoded trailing space or NUL padding when the
calling format permits it.

The generic validator, clinical relationship resolver, DICOMweb server,
DICOMDIR, UPS and DIMSE storage planning use the same syntax decision after
their format-specific normalization. DICOMweb route identifiers are not
padded and also obey the server's configurable `MaxUIDBytes` limit. Each
package retains its own typed diagnostic, error or DIMSE status mapping.

For compatibility, digit-and-dot strings that are not valid OIDs, including
`3.1`, `1.40` and `1.02`, are rejected consistently. Empty optional UI values
remain permitted by the generic validator; required UID policy is evaluated
separately.

An encoded UI value may end in the one NUL that PS3.5 9.1 adds to reach even
length. Space padding, or more than one trailing NUL, is reported as
`CodeValuePadding`, although reading still strips it. The other text VRs are
padded with spaces; a value, or one value of a multi-valued element, that ends
in NUL padding (`PATIENT\0`, `CT\0 `) is reported as `CodeValuePadding` too,
while readers strip it as described in
[text value padding](CONFORMANCE.md#text-value-padding).

### Value formats

Besides repertoire and length, the validator checks the formats of PS3.5 6.2:
AS, CS, DS, IS and UI syntax; DA, TM and DT through `dcmtime`, where a space is
trailing padding only and never leads the value; an AE value that consists
solely of spaces; and UR values, which must use the characters of RFC 3986
section 2 (with well-formed percent-encoding) and no leading or embedded
space. The verdicts are cross-checked against DCMTK's `checkStringValue()`
vectors; see [DCMTK value vectors](CONFORMANCE.md#dcmtk-value-vectors) for the
cases where dicom-go deliberately answers differently.

## Lifecycle hooks

Register hooks explicitly in a `HookChain`. A validation operation snapshots
the chain once, so additions affect only later operations. Duplicate or
unstable hook names are rejected. Hooks marked `ConcurrentSafe: false` are
serialized across operations.

| Point | Observe/diagnose/reject | Replace element | Filter element | Skip value | Defer value |
|---|---:|---:|---:|---:|---:|
| `HookElementHeaderRead` | yes | no | no | yes | yes |
| `HookAfterElement` | yes | yes | yes | no | no |
| `HookSequenceComplete` | yes | yes | yes | no | no |
| `HookItemComplete` | yes | no | no | no | no |
| `HookDataSetComplete` | yes | no | no | no | no |
| `HookPreValidation` / `HookPostValidation` | yes | no | no | no | no |
| `HookPreSerialization` | yes | yes | yes | no | no |
| `HookPostWrite` | yes | no | no | no | no |

Mutually exclusive actions, or an action illegal for its point, return
`ErrHookAction` before applying the decision. Parser-side skip/defer initially
supports only defined-length primitive values. Defer additionally requires a
seekable source. Structural framing is always consumed, so filtering cannot
desynchronize the stream.

Panic and callback errors are contained. `HookFailureReject` returns a redacted
`HookError`; `HookFailureFinding` emits a stable diagnostic and discards the
decision. `HookRegistration.Timeout` includes time waiting for a non-concurrent
hook gate, but callback cancellation is cooperative: Go cannot safely preempt a
hook that ignores its context.

Lifecycle callbacks fire once for the original parse. Deferred-value replay,
`CopyElementValueTo`, and `Object.CopyValueTo` suppress replay events. The
writer's post-write event reports logical DICOM bytes accepted by the
serialization layer and whether that element was fully serialized. The object
facade's `WriteValidationResult.BytesWritten` separately reports bytes accepted
by the caller's external writer, including compression effects. A deflated
writer is created lazily, so validation rejection before serialization emits no
empty compressed stream. `ValidationWriteError.Complete` lets callers avoid
retrying an element whose bytes were already committed but whose post-write
hook failed.

## Paths, offsets and object limitations

Parser reports retain absolute offsets, including a configured `BaseOffset`,
and nested paths with zero-based item indexes. Duplicates and original ordering
must be validated before conversion to `object.Object`: Object intentionally
uses last-wins tag lookup and cannot reconstruct collapsed duplicates.

Deferred native and encapsulated Pixel Data remains streamable through the
validated object read/write path. General deferred sequence serialization has
the same existing limitation as the ordinary object writer.

## Testing and performance contract

Synthetic fuzz targets cover VR strings, VM expressions and bounded nested
sequences. Run them independently because Go fuzzing accepts one `-fuzz`
target per process:

```sh
go test ./validation -run='^$' -fuzz=FuzzValidateVRStringsBounded -fuzztime=10m
go test ./validation -run='^$' -fuzz=FuzzValidateMultiplicityDelimitersBounded -fuzztime=10m
go test ./validation -run='^$' -fuzz=FuzzValidateNestedSequencesBounded -fuzztime=10m
```

The ordinary reader/writer output is covered byte-for-byte with validation
disabled. The pre-declared default-path performance gate is no additional
allocation and no more than 5 percent median `ns/op` regression against the
same benchmark before this feature. The opt-in cost is reported separately:

```sh
go test ./parser -run='^$' -bench=BenchmarkReaderValidationLifecycle -benchmem -count=5
```

Validated writers use a report-only built-in view of immutable known values and
finite `Options.WriteLimits`; public validation/hook results remain detached.
Contextual caller functions retain owned inputs. Serialization changes invalidate
previous dataset proof and can be refused when context cannot be re-established
without replaying caller code. See [validated writer memory and scope
contracts](VALIDATION_WRITER_MEMORY.md) for limits, ownership and diagnostic
boundaries.

## Enhanced Functional Group structure

`iod.ValidateFunctionalGroups` explicitly checks the Enhanced CT Image,
Enhanced MR Image and Segmentation structural profiles. Existing `Validate`
and `Options` retain their default behavior. `ValidateWithFunctionalGroups`
composes the finite preflight with the existing IOD/value checks; the CLI
selects it with `dcmvalidate -functional-groups`.

`ValidateFunctionalGroupDataSet` accepts raw `core.DataSet` sequence items and
reports duplicate tags before creating last-wins Object facades. The Object
entry cannot reconstruct root elements already discarded by its materialized
representation. Neither entry resolves deferred values or decodes Pixel Data.

The initial profile checks Shared item cardinality, Per-Frame counts against
Number of Frames, the existing resolver's conflict list, the prohibition on
Shared Frame Content, mandatory CT/MR groups and scoped conditional groups.
SEG does not inherit CT/MR's unconditional Frame Content requirement:
LABELMAP and TILED_FULL cases can omit Per-Frame when no group requires it.
A present Frame of Reference UID requires Pixel Measures plus the patient
position/orientation pair or the slide branch; TILED_FULL alone does not establish
slide geometry. Coordinates not established by the dataset remain explicit manual
findings.
Top-level macro item counts follow their individual ranges. Referenced Image,
Derivation Image and MR Spatial Saturation permit zero or more items; Real World
Value Mapping, Contrast/Bolus Usage, MR Receive Coil, MR Velocity Encoding,
Arterial Spin Labeling and CT Additional X-Ray Source require one or more.
CT Acquisition Details, Geometry, Exposure and X-Ray Details require one item
when Multi-energy CT Acquisition is NO or absent, and allow one or more for YES.
An unknown value produces a manual maximum-cardinality finding for multiple
items while the minimum of one remains enforced. Other selected macros require
one item. Item counts are independent of primitive value multiplicity.
Forbidden MR macros are reported even when their SQ is empty; unresolved
Shared/Per-Frame conflicts produce one conflict finding without contradictory
resolved-item cardinality findings.

Private dimension semantics and external acquisition/derivation context remain
manual; a structural pass does not assert complete IOD conformance.

Frames, raw elements, sequence items, depth, operations and findings have finite
defaults and hard ceilings. Actual materialized primitive payloads are admitted
before decoding, irrespective of encoded length headers, with a 1 MiB per-value
and 16 MiB aggregate metadata ceiling. Unknown custom Value implementations are
rejected without invoking their methods. Pixel Data is excluded from these
metadata byte ceilings. Limit/cancellation errors indicate an incomplete
check. Findings use fixed messages, frame/item paths, rule identifiers and
PS3.3 2026d section provenance. Cardinality of a dimension's primitive values
remains the existing contextual value engine's responsibility; pointer
resolution does not treat SQ item counts as primitive VM.
