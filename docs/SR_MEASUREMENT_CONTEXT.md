# Quantitative context for MeasurementReport


The authoritative value is `DetailedMeasurementReport.Document()`, with the
existing `TemplateMetadataSnapshot`. `Report()` and `Context()` return fresh,
defensive projections after `Check`; mutating a returned projection does not edit
the document. `Dataset()` and `File()` check that same binding before output.
They encode the original ordered generic tree and its code, template, numeric
and reference metadata. They never rebuild a read tree through the lossy legacy
model. This preserves unmodeled derived sections/extensions within the generic
reader's supported tree and sidecar contract, even when the specialized view
reports them in `Validation().Unsupported`.

## Explicit context and levels

| Context | Supported level and relationship |
| --- | --- |
| Activity Session | 1501/1411 group, HAS OBS CONTEXT TEXT |
| Subject/protocol Time Point ID, Time Point, types, order, temporal offset | Group TID 1502, HAS OBS CONTEXT; NUM order uses UCUM `1`, offset uses UCUM `d` and has its own required Event Type CODE child |
| Measurement Method | Group and individual NUM, HAS CONCEPT MOD CODE |
| Finding sites | Group and individual NUM; each site owns its optional Laterality and Topographical Modifier CODE children |
| Algorithm identification | Imaging container, qualitative container, group and individual NUM; TID 4019 HAS CONCEPT MOD rows |
| Real World Value Map | Group CONTAINS COMPOSITE; individual NUM INFERRED FROM COMPOSITE in this explicit profile |
| Qualitative CODE/TEXT answer | Qualitative root container or 1501/1411 group, CONTAINS; optional HAS CONCEPT MOD CODE children for CODE answers |

Algorithm Name TEXT and Version TEXT are mandatory whenever an algorithm is
present; Name CODE, Manufacturer TEXT, ordered Parameters TEXT and Family CODE
are optional. TID 4019's significant row order is checked. The API retains the
explicit parent level rather than copying inherited context into descendants.
PS3.16 2026d TID 1419 row 19 specifies NUM CONTAINS COMPOSITE, whereas
PS3.3 Table A.35.13-2 permits CONTAINS only from CONTAINER. This profile keeps
the generic IOD relationship validation strict and uses the TID 301 row 18 (included by TID 300 row 1b)
INFERRED FROM form as an explicit extension for individual NUM references in
1411 groups. Such a reference produces the bounded `rwvm-inferred-from-extension`
finding in `Validation().Unsupported`; it does not claim satisfaction of TID 1419
row 19. A source NUM CONTAINS COMPOSITE is refused with its original source intact.
The group's RWVM reference and a measurement override can both exist. Their
values are not transformed: this is provenance, not another application of RWVM.
No session, time point, clinical code, tracking ID or RWVM identity is invented.
Existing legacy authoring defaults for language, observer and procedure remain
those of `WriteMeasurementReport`.

Concept recognition uses the exact code form, scheme and scheme version through
`CodeContext`, together with value type and relationship. Code Meaning is never
an identity fallback. Observation-context NUM rows are never projected as numeric
measurements. A recognized numeric measurement with empty Measured Value Sequence
is refused by this specialized profile rather than invented as zero; the original
input Object remains intact. The generic APIs retain their broader behavior.

`MeasurementContextCode` carries the existing Long/URN/scheme-version and
Equivalent Code sidecars. Long/URN values remain data. Numeric representation is
the [existing opt-in finite profile](SR_NUMERIC_REPRESENTATIONS.md): original DS
bytes, native FD bits, SL/UL fractions, preference FD > rational > DS and explicit
half-DS-quantum coherence policy. Neither a finite/coherence refusal nor this
specialized shape refusal declares the original attributes generically invalid
DICOM VRs.

## Authoring and reading

```go
context := sr.MeasurementReportContext{
    Groups: []sr.MeasurementGroupContext{{
        ActivitySession: "Session supplied by the caller",
        Algorithm: &sr.MeasurementAlgorithm{
            Name: "Caller supplied algorithm", Version: "1.0",
        },
    }},
}
view, err := sr.BuildMeasurementReportWithContext(report, context,
    sr.MeasurementContextOptions{})
if err != nil { return err }
file, err := view.File()
```

Nonempty authored Groups/Measurements slices are dense: their counts must match
the corresponding caller supplied legacy groups/measurements. Context site values
must exactly agree with the legacy site's order/identity, or supply the sites if
the legacy list is absent. A provided numeric representation must resolve to the
same native float64 bits as the caller's scalar. Contradictions are errors.
`Path` is a returned locator, not a persistent identity; authoring expects it empty.

Reading admits resident source values before decoding; source/custom callbacks,
lazy payload materialization and network lookup are refused by this finite input
profile. The source remains intact on success and refusal. Current Requested
Procedure Evidence tuples are copied and validated before generic decoding.
An authored RWVM reference requires its Study, Series, SOP Class and SOP Instance
UIDs. The SOP Class must be Real World Value Mapping Storage. Duplicate SOP UID
with conflicting class/study/series is refused. Every admitted content reference
must agree with its supplied evidence. Retained unrelated evidence is also copied;
no source UID/hierarchy is guessed to complete an RWVM tuple. A TID 1411
Referenced Segment row admits Segmentation Storage, Surface Segmentation Storage
or Label Map Segmentation Storage (PS3.4 B.5-1); the profile does not infer the
class from a concept's meaning or admit unrelated image/Height Map classes.

Read retains the admitted Specific Character Set, so preserved raw extension
payloads keep their encoding declaration. Build authors UTF-8. Explicit
`BindMeasurementReportWithContext` defaults to UTF-8 and accepts `CharacterSet`
when the caller intentionally retains another declaration; the caller must keep
raw preserved text and the declaration consistent. This API preserves modeled
SR headers, not byte-identical whole-file/whole-IOD identity. Patient/Study/common
modules follow `Document.Common` and `object.DerivedCommon`; set Common explicitly
when those modules must be inherited. Header/Common fields have the existing
mutable header contract and are outside the content guard. No concurrent mutation
of the document or a Common source is supported.

## One snapshot and deliberate edits

IMAGE Referenced Segment Number was absent from the legacy `ImageReference`
projection. `MeasurementReferenceMetadata` now stores ordered nonzero US segment
numbers at explicit IMAGE paths, alongside matching SOP identity, privately in
**the same** `TemplateMetadataSnapshot`. The shared combined quota, metadata hash,
conservative ambiguity proof and `Check` include these entries. Snapshot output
restores US only inside Referenced SOP Sequence in standard tag order. It never
adds a loose US to IMAGE or duplicates that sequence. Ordinary legacy generic
output remains unchanged when no reference metadata is supplied.

After intentionally editing `Document()`, consumers must supply correctly
associated metadata and call `BindMeasurementContextMetadata` for a fresh
snapshot, then `BindMeasurementReportWithContext` with correctly admitted evidence.
The existing `Metadata()`, `NumericRepresentations()` and `MeasurementReferences()`
accessors return defensive copies, not authority to automatically move paths.
There is no repair/remap/stable-ID inference. Reordering/replacing content is
conservatively stale. Indistinguishable IMAGE subtrees carrying different segment
metadata are refused because legacy values cannot prove which metadata moved.
Do not carry an old numeric representation through a changed value or units:
remove/re-author it explicitly before binding.

Opaque raw `ValueElements` remain byte data inside the generic tree and produce
an `opaque-value-elements` finding. The generic writer does not normalize their
unknown internal byte order. This profile's canonical output is little endian:
`Dataset()`/`File()` refuse big endian sources containing opaque raw values before
output allocation, while leaving the source/document intact. Explicit Bind callers
must set `ValueElementsBigEndian` when those values came from big endian input;
Read derives the flag and Build uses little endian. Native typed core values and
modeled attributes continue to encode across the three supported syntaxes. For
opaque raw values, this profile makes no cross-syntax or endian conversion claim;
callers retaining their original representation must preserve the source Object
and its transfer syntax instead of sending that payload through another syntax.

## Validation and finite limits

`Validation()` keeps unsupported projection findings, the existing recursive
standard-template catalogue findings and PS3.3 relationship findings separately.
The shape profile rejects wrong recognized value type/relationship, singleton
multiplicity, incomplete algorithm/time point, wrong fixed time point units,
orphan site modifier, malformed resident macro, incorrect reference/evidence
association and contradictory authored values. Errors contain codes and validated
numeric paths, never rejected values or parse details. Unknown allowed extensions
are preserved and reported rather than treated as typed fields through inference.

The catalogue defaults to warning mode; `StrictCatalog` selects its strict mode.
Parameterized/conditional/CID catalogue limitations remain catalogue limitations;
profile success is not full TID/CID, IOD, clinical or PACS conformance. Supported
group templates are exactly 1501 and 1411; alternate ROI source forms/templates
remain generic content with bounded unsupported findings. Root qualitative-only
and derived content can be retained, without pretending the specialized numeric
projection completely models them.

Zero limits select finite defaults: reader/snapshot depth 64, 100,000 content or
combined metadata items, 64 MiB logical resident/hash input, 100,000 evidence
references, 300,000 resident source elements/sequence/string items,
1 MiB per projected string and 1,024 unsupported findings. `MaxEvidenceItems`
bounds both evidence facade growth and resident source admission;
`MaxStringBytes` is checked on resident StringValue components and on each whole
raw textual attribute (including delimiters/padding) before any projected string
copying. Referenced SOP Sequence has exactly one item and resident SOP UIDs must
be UI VM 1 with valid identities, including IMAGE without segment metadata,
COMPOSITE and WAVEFORM. PN observer values must be PN VM 1. Optional attributes
inside a recognized Referenced SOP item, such as presentation-state, inline RWVM
or icon sequences, are not retained by the legacy generic reference model. The
opt-in profile refuses these unmodeled nested macro attributes before decoding
instead of reemitting a partial reference. This does not change the generic
reader/writer or assert whole-macro/whole-IOD preservation; the source Object
remains available intact. These are logical
quotas, not peak RSS promises. Code-specific and UI/VR limits still apply.
Quotas are positive; negative options are refused. No runtime scaling or speed
gain is claimed by this functional work.

Fixtures cover 1501/1411 scopes, observation NUM separation, reference/segment
location, three uncompressed transfer syntaxes, DS/FD/fractions, Long/URN codes,
unknown retained content, deliberate edits/rebind, defensive concurrent reads,
resident callback refusal, conflicts and budgets. The optional DCMTK recipe is
one bounded `dsr2xml +Wt` → `xml2dsr` → `dcmdump` comparison, with explicit XML parent
level/relationship checks and reread attribute checks. `+Wt` preserves template
identification in XML; the converter's default omits it. No error-suppression,
constraint-disabling or invalid-item-skipping flags are used. Actual normalization and
refusals must be recorded; skips or merely accepted output are not proof of
content preservation. Native Windows GUI execution and CI are outside scope.

Authority: [DICOM PS3.16 2026d measurement templates](https://dicom.nema.org/medical/dicom/current/output/chtml/part16/chapter_A.html)
and [PS3.3 Comprehensive 3D SR relationships](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.35.13.3.html)
and [TID 4019 Algorithm Identification](https://dicom.nema.org/medical/dicom/current/output/chtml/part16/sect_TID_4019.html).
The implementation uses the local generated standard catalogue and clean-room
contracts, without transplanting external project implementation code.
