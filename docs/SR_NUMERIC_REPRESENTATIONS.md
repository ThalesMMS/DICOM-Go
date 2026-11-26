# Preserving numeric SR representations

`ReadDocumentWithNumericRepresentations` opts into a finite preservation profile
for the Numeric Measurement Macro. It retains the original Numeric Value (DS)
text, Floating Point Value (FD), and paired Rational Numerator/Denominator Value
(SL/UL) in `NumericRepresentationMetadata`. `Measurement` and the legacy reader,
writer and renderer signatures are unchanged. Generic DICOM Objects already
retain these attributes; this opt-in API addresses their loss through the typed
SR model. Reusable behavior belongs to `dicom-go/sr`.

```go
read, snapshot, err := sr.ReadDocumentWithNumericRepresentations(
    dataset, sr.ReadOptions{}, sr.MetadataSnapshotOptions{})
if err != nil {
    return err
}
preserved := snapshot.NumericRepresentations() // defensive copy
for _, entry := range preserved.Entries {
    scalar, provenance, err := entry.Representation.Resolve()
    // provenance.Source identifies FD, RATIONAL or DS;
    // provenance.Exact describes scalar equality to that source.
    _, _, _ = scalar, provenance, err
}
output, err := read.Document.DatasetWithMetadataSnapshot(snapshot)
```

The scalar source is FD, then rational, then DS. FD's native float64 bits are
retained, including signed zero. Rational conversion uses correctly rounded
`big.Rat.Float64`; the integers remain intact. Exactness describes the selected
source, not all alternatives: every finite FD already is a float64, while DS
`0.1` and rational `1/3` are not exactly represented by their modeled scalar.
DS underflow may produce zero with `Exact == false` while the DS survives.
Absent alternatives are never synthesized. A resident DS string preserves
leading/trailing spaces, zeroes and exponent spelling. The Part10 writer may
append the required space for an odd byte length; it never removes a supplied
byte. Reads of encoded even-length DS retain its existing padding exactly.

## Finite profile and coherence

Normative authority is [PS3.3 C.18.1, Numeric Measurement Macro](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.18.html)
and [PS3.5 6.2, Value Representations](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.html)
(2026d at implementation). The macro has zero or one Measured Value Sequence
item. A present item requires DS VM1 and exactly one Measurement Units Code
Sequence item; optional FD/SL/UL attributes have VM1 in this macro. Rational
fields occur together and the unsigned denominator is nonzero. An empty
Measured Value Sequence creates no numeric entry; existing qualifier semantics
remain in the typed reader.

DS follows the ASCII decimal grammar, allows leading/trailing ASCII spaces,
and is limited to 16 bytes including padding. Embedded spaces, multiplicity
separators, NUL, hex, NaN and infinity spelling are refused. The exponent
magnitude is capped at 1,024 before big-number construction; scalar overflow
is refused. Only resident RawValue/StringValue DS, Float64Value/raw8 FD,
Int32Value/raw4 numerator and Uint32Value/raw4 denominator are accepted.
Raw binary values use the containing Object's byte order. Deferred, discarded,
unresolved bulk and custom numeric Values are refused without materialization,
callbacks or I/O. The source Object is unchanged on refusal and retains all
original attributes; the opt-in reader returns no modeled result for that
refused numeric input.

The following are **profile policies**, not general DICOM validity rules:

- FD must be finite. PS3.5 permits IEEE NaN/infinity for the FD VR generally;
  `finite-profile` refusal does not claim their VR encoding is invalid.
- Simultaneous FD and rational must agree with correctly rounded rational to
  float64. Positive and negative zero compare as the same mathematical value;
  FD's original sign bit remains preserved.
- Each more precise alternative must lie within the DS lexical rounding cell:
  one half of its last-digit decimal quantum, inclusive. Comparisons use exact
  rational arithmetic. DS `1` permits FD `1.25` and the boundary `1.5`; DS
  `1.00` has a smaller cell and refuses them. The cell reflects the supplied
  spelling, including trailing zeroes and exponent. It permits loss of DS
  precision but refuses contradictions. DICOM does not prescribe this half
  quantum tolerance; a producer using truncation outside it is outside this
  opt-in profile. Use the intact source Object for data outside the profile.

`NumericRepresentationError` is typed and value-free. It reports stable shape,
DS, finite-profile, coherence, association or resource-limit codes. Only
validated numeric content paths may appear in its message. Underlying numeric
parse errors containing source strings are not exposed.

## Binding, edits and limits

`BindNumericRepresentations(document, codeTemplateMetadata, numericMetadata,
options)` binds authored or already correctly associated representations.
Each path must name an existing NUM with a measurement, and its scalar bits
must equal `Resolve`'s selected scalar. Unset alternatives must have zero
fields. Metadata is copied and its accessors return defensive copies.

Numeric metadata shares `TemplateMetadataSnapshot`'s existing structural guard,
item/byte budgets and ambiguity proof. There is no second remapping or guard
API. Default limits are 64 levels, 100,000 content items, 100,000 combined
numeric/code/template/equivalent metadata items and 64 MiB of fingerprint
input. These are logical traversal/input quotas, not an RSS bound or a promise
of admission before every allocation. The snapshot retains document/node
references. Concurrent reads of an unchanged document are supported;
concurrent mutation is not supported.

An edit to the modeled value, unit, ordered structure or guarded payload makes
the snapshot stale before writing, rendering or template validation. Output is
not produced for a stale binding. Identical modeled subtrees with distinct
numeric metadata fail binding conservatively because the legacy tree supplies
no stable identity to distinguish them. Equivalent metadata on intact copies
is allowed. This guard checks state, not an edit event log; assigning the same
scalar bits does not reveal semantic intent.

For an intentional numeric edit, remove that entry from a caller-owned metadata
copy or supply newly coherent alternatives, set the corresponding modeled
scalar, and explicitly bind again with correctly associated code/template
metadata. A removed numeric entry uses the existing writer's new DS and has no
old FD/fraction. Unit editing also requires a new association; no parallel unit
conversion or automatic reassociation is performed. Writes apply existing
code/unit contexts first and restore numeric fields afterward, preventing unit
context rebuilding from discarding them. The restored numeric macro is emitted
in ascending tag order. Specialized measurement report consumers may reuse this
contract; this change does not claim their independent APIs preserve sidecars.

## Verification scope

Synthetic contracts cover decimal precision, fraction boundaries, alternative
presence and precedence, signed zero, underflow/exactness, long/URN unit codes,
three native transfer syntaxes, defensive copies, unchanged concurrent reads,
explicit edits, stale/ambiguous association, finite limits, invalid macro/VM/VR/
resident kinds and zero-callback refusals. Legacy behavior remains unchanged.

The optional `TestNumericRepresentationsDCMTK` compares actual emitted attributes
through DCMTK `dsr2xml` -> `xml2dsr` and `dcmdump`, enabled by
`DICOMGO_DCMTK_INTEGRATION=1` and `DICOMGO_DCMTK_BIN`. Local DS lexical bytes are
checked exactly. DCMTK 3.7.0+DEV's XML path was observed to trim surrounding DS
spaces while retaining the decimal/exponent spelling; the comparison asserts
that explicit normalization separately, and checks FD native bits and rational
integer attributes exactly. It does not certify DS whitespace preservation by
DCMTK. An initial fixture with an empty patient XML node was refused by xml2dsr;
the final independent fixture supplies synthetic common attributes, and no
ignore-error flags are used. These focused attribute checks do not establish
full IOD, clinical, PACS or performance qualification. No CI or permanent
validation module is required.
