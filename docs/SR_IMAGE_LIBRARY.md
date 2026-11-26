# Structured SR Image Library

`sr.MeasurementReport` accepts `ImageLibraryGroups` and
`ImageLibraryDescriptors` in addition to its original `ImageLibrary` field.
`ImageLibraryDescriptors` represents the TID1600 study summary (including
multiple modalities/target regions), independently of acquisition context.
An `ImageLibraryGroup` contains common acquisition descriptors and image
entries; each `ImageLibraryEntry` keeps its own descriptors beside its image
reference. Descriptors use `ContentItem` and its existing typed CODE, NUM, DATE,
TIME, DATETIME, TEXT and UIDREF values. NUM units remain coded UCUM values.
Nested Image View modifiers are preserved. Extensible descriptors are retained,
including their children and supported opaque value elements.

When authoring, supply either the legacy field or the structured fields. A
manual non-nil legacy slice combined with non-nil structured fields is rejected,
including an empty legacy slice. Rich reads retain a detached flat reference
projection as well as structured groups and metadata, so existing consumers
(including the dcmqi reader fixture) continue to see `ImageLibrary`. On each write,
this reader-derived projection must equal the structured references and their
emitted ordering; divergent edits are refused. Equivalent projections emit one
structured library, preserving descriptors and sidecars. Nil structured fields retain the old behavior, including inferred
references from report measurements. An explicit empty structured slice suppresses
that inference. Descriptor-free one-group reports continue to read into the
legacy field. Multiple groups, acquisition context or richer encoding metadata
read into the structured fields. Unknown rich structural layouts are refused
explicitly; the original Object remains available for generic SR processing.

`Order` on a group and `ImageLibraryOrder` on a report preserve the original
non-significant interleaving. Negative ordinals select descriptors, positive
ordinals select entries/groups. Nil selects descriptors first. Invalid, repeated
or incomplete ordinals are rejected.

## Validation and preservation

`ValidateImageLibrary` checks finite structure, relationship types, recognized
TID 1602–1607 row value types, row multiplicity, UCUM units and modality
applicability. It also recognizes the optional MR TID 1608 extension. TID1601 entry descriptors override group common descriptors with the same
concept, scheme and value type. Both explicit structures survive round-trip;
CODE/TEXT alternatives may coexist. Root TID1600 summary facts are not inherited
as scalar entry acquisition context. An SOP instance cannot appear in different
groups; references to different frames within one group remain available.
PET glucose without an encoded valid Observation DateTime needs glucose
measurement DATE and TIME descriptors. No missing acquisition fact is invented.
Unknown CID membership and the clinical meaning of extensions are not inferred.
The pinned PS3.16 2026d MR rows that encode NUM with CID constraints are retained
as NUM; extraction does not reinterpret them as CODE.

`ValidateImageLibraryTemplate` runs the existing pinned TID 1600 template
registry as a separate layer, returning its complete findings, including unresolved
conditions or CID membership. Neither domain checks nor a peer's ability to read
an SR establish complete SR IOD, clinical or PACS conformance. The application's
existing object/IOD validation remains a separate check.

Code versions, long/URN identities, equivalent codes, template identifications,
and precise DS/FD/rational representations use `ImageLibraryMetadataBinding`.
Metadata paths are relative to the library container rooted at `1`. Reading
binds them to the exact ordered tree. Editing the model invalidates that binding;
call `BindImageLibraryMetadataWithOrder` with explicitly remapped metadata for
the authorized edited tree. No stale path association is inferred. Accessors
return defensive copies.

`PromoteImageLibraryCommonDescriptors` produces detached groups and only promotes
exactly equal descriptors, including their units, values and nested context.
Returned groups, descriptors, headers and opaque resident values are deeply
detached; editing the returned model does not mutate the source.
It never merges groups. Reports with bound metadata require an explicit metadata
remap before promotion: the function refuses them rather than treating different
code versions or numeric representations as equivalent. The caller can then
bind the resulting tree using its explicitly relocated sidecar.

## Explicit dataset extraction

`ExtractImageLibraryEntry(ctx, source, frames, options)` reads only explicitly
supplied, resident dataset attributes. It reads no pixel data, files, deferred
values, BulkData or network services. Shared and selected per-frame acquisition
macros are considered; root/shared/per-frame conflicts are errors. Per-frame
extraction requires exactly one explicit frame. Frame references are checked
against an explicit Number of Frames when available.

Direct mappings include common descriptors, projection orientation/spacing,
cross-sectional position/orientation/spacing, CT acquisition/reconstruction,
MR pulse/field/diffusion, and PET radiopharmaceutical facts. Row/column pixel
spacing order is mapped explicitly. Clinically derived glucose, timing/counts,
and non-direct MR CID seams are reported as absent or not directly extractable.
Missing values are findings, not zero/default measurements. Code metadata is
preserved in the extraction sidecar. Authored numeric representations use the
selected finite float64 scalar and its legal DS spelling, retaining the scalar
in FD rather than silently rounding it during SR emission.

The extraction sidecar is rooted at the detached IMAGE item `1`.
`MetadataAt("1.1.1")` explicitly relocates it to an image entry's full library
path. Pass these code/numeric facts to `BindImageLibraryMetadata` after selecting
the group and entry; the library does not guess their association.

Default limits are 10,000 content items, 32 content levels and 16 MiB of value
bytes. Options are finite even when zero; the maximum accepted item allowance is
1,000,000 and depth is at most 64. Concrete resident bytes are checked before
getters, regardless of a declared element Length. Metadata attribute expansion
is separately bounded. Context cancellation is checked during extraction,
validation and promotion. Error codes and numeric paths contain no descriptor
text, source UID, patient identifier or acquisition value.

## Focused checks

```sh
go test ./sr -run '^TestImageLibrary' -timeout 90s
```

The default run explicitly skips the opt-in DCMTK test. Actual descriptor peer
conversion requires `DICOMGO_DCMTK_INTEGRATION=1` and reviewed `dsr2xml` and
`xml2dsr` tools (`DICOMGO_DCMTK_BIN`). `TestImageLibraryDescriptorsDCMTK` compares
actual converted group associations, concepts, values, units and image references;
it records template findings separately. Peer output capture is capped at 64 KiB
and each conversion has a 15-second deadline. A skipped peer is not peer proof.

## Recorded qualification

On macOS ARM64 with Go 1.27.1, the isolated published `4d1c2d6` source snapshot
plus this implementation passed 62 mandatory SR tests. The one additional,
explicitly recorded skip was the pre-existing optional
`TestMeasurementReportReadsInHighdicom` Python integration test. The two new
TID1600 scope/metadata causals also passed separately. Nineteen affected Twin SR
caller tests passed; their evidence was reused because the subsequent template
wrapper correction does not change those callers or their consumed contract.

Two actual DCMTK 3.7.0 tests passed `dsr2xml` → `xml2dsr` conversion of the same
synthetic DX, CT, MR and PT library. Checks preserve common and entry descriptors,
concepts, values, units, group associations, Study/Series/SOP references and frame
numbers. The tools' previously measured versions were reused after identical
binary and dependency fingerprints were verified. `DCMDICTPATH` was explicitly
bound to the reviewed dictionary from the tools' DCMTK source revision. An earlier
run failed because the default installed dictionary path was absent; that failed
receipt and its warnings remain preserved. No warning was suppressed.

The final peer fixtures produced empty separate template findings and empty peer
warning output. The non-root TID1600 wrapper now validates the library CONTAINER,
relocates defensive metadata copies, and admits only verified reader-derived
legacy projections. A malformed encoded CID on the installed non-extensible
Laterality row is detected at its actual included-template path. This does not
qualify every CID or condition: extensible/reference-defined CIDs, including the
Modality CID, retain the existing catalog limitations. Selected template checks,
peer readability and complete clinical SR IOD/PACS conformance remain separate.

